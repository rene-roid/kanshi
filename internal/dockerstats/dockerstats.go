// Package dockerstats talks to the Docker Engine API over its unix socket.
//
// It uses `GET /containers/{id}/stats?stream=false&one-shot=true`. The one-shot
// form returns immediately; without it the daemon blocks each request for a
// full collection cycle to produce `precpu_stats`, which measured 8.3s per tick
// across 31 containers versus 0.07s here.
//
// The tradeoff is that one-shot zeroes `precpu_stats`, so CPU% is computed
// against the previous tick's counters instead — the same thing the daemon
// would have done, just over the poll interval rather than a 1s window. That is
// also a steadier number to read at a glance.
package dockerstats

import (
	"context"
	"encoding/json"
	"fmt"
	"math"
	"net"
	"net/http"
	"sort"
	"sync"
	"time"

	"github.com/yuuki824/kanshi/internal/config"
)

// Pinned rather than negotiated: every field Kanshi reads has been stable
// since 1.43, and asking for a version the daemon predates is a hard error.
const apiVersion = "v1.43"

// Client holds the socket transport and the previous tick's counters. Every
// rate here is a delta, so the client has to outlive a single sample.
type Client struct {
	cfg  config.Config
	http *http.Client

	mu      sync.Mutex
	prevCPU map[string]cpuCounters
	prevNet map[string]netCounters
}

type cpuCounters struct{ total, system uint64 }
type netCounters struct {
	at     time.Time
	rx, tx uint64
}

func New(cfg config.Config) *Client {
	socket := cfg.DockerSocket
	dialer := &net.Dialer{Timeout: 5 * time.Second}
	return &Client{
		cfg: cfg,
		http: &http.Client{
			Timeout: 20 * time.Second,
			Transport: &http.Transport{
				DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
					return dialer.DialContext(ctx, "unix", socket)
				},
				// One idle connection per in-flight request, so a tick reuses
				// the sockets the previous tick opened instead of paying a
				// connect per container.
				MaxIdleConns:        cfg.DockerConcurrency,
				MaxIdleConnsPerHost: cfg.DockerConcurrency,
				IdleConnTimeout:     90 * time.Second,
			},
		},
		prevCPU: make(map[string]cpuCounters),
		prevNet: make(map[string]netCounters),
	}
}

// Close releases the idle sockets so a shutdown does not leave the daemon
// holding connections.
func (c *Client) Close() { c.http.CloseIdleConnections() }

func (c *Client) get(ctx context.Context, path string, out any) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, "http://docker/"+apiVersion+path, nil)
	if err != nil {
		return err
	}
	resp, err := c.http.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 400 {
		return fmt.Errorf("HTTPStatusError: %s", resp.Status)
	}
	return json.NewDecoder(resp.Body).Decode(out)
}

/* ── engine payloads ────────────────────────────────────────────────────── */

type containerMeta struct {
	ID      string            `json:"Id"`
	Names   []string          `json:"Names"`
	Image   string            `json:"Image"`
	State   string            `json:"State"`
	Status  string            `json:"Status"`
	Created int64             `json:"Created"`
	Labels  map[string]string `json:"Labels"`
	Ports   []portMeta        `json:"Ports"`
}

// portMeta is one entry of the engine's Ports array. PublicPort is absent for
// a port that is merely EXPOSEd rather than published to the host.
type portMeta struct {
	IP          string `json:"IP"`
	PrivatePort int    `json:"PrivatePort"`
	PublicPort  int    `json:"PublicPort"`
	Type        string `json:"Type"`
}

// Pointers on total_usage and system_cpu_usage so a missing field is
// distinguishable from a genuine zero — a container that has used no CPU at
// all still reports 0, and that is a real reading.
type statsJSON struct {
	CPUStats struct {
		CPUUsage struct {
			TotalUsage  *uint64  `json:"total_usage"`
			PercpuUsage []uint64 `json:"percpu_usage"`
		} `json:"cpu_usage"`
		SystemUsage *uint64 `json:"system_cpu_usage"`
		OnlineCPUs  int     `json:"online_cpus"`
	} `json:"cpu_stats"`
	MemoryStats struct {
		Usage *uint64           `json:"usage"`
		Limit uint64            `json:"limit"`
		Stats map[string]uint64 `json:"stats"`
	} `json:"memory_stats"`
	Networks map[string]struct {
		RxBytes uint64 `json:"rx_bytes"`
		TxBytes uint64 `json:"tx_bytes"`
	} `json:"networks"`
	PidsStats struct {
		Current int `json:"current"`
	} `json:"pids_stats"`
	BlkioStats struct {
		IoServiceBytesRecursive []struct {
			Op    string `json:"op"`
			Value uint64 `json:"value"`
		} `json:"io_service_bytes_recursive"`
	} `json:"blkio_stats"`
}

/* ── wire payload ───────────────────────────────────────────────────────── */

// Result is the JSON the browser consumes; field names are part of the wire
// contract with web/app.js.
type Result struct {
	Containers []Container `json:"containers"`
	Running    int         `json:"running"`
	Total      int         `json:"total"`
	Error      string      `json:"error,omitempty"`
}

type Container struct {
	ID         string  `json:"id"`
	Name       string  `json:"name"`
	FullName   string  `json:"full_name"`
	Project    string  `json:"project"`
	Image      string  `json:"image"`
	State      string  `json:"state"`
	Status     string  `json:"status"`
	Health     *string `json:"health"`
	Created    int64   `json:"created"`
	CPU        float64 `json:"cpu"`
	MemUsed    uint64  `json:"mem_used"`
	MemLimit   uint64  `json:"mem_limit"`
	MemPercent float64 `json:"mem_percent"`
	PIDs       int     `json:"pids"`
	Net        *Net    `json:"net"`
	Blkio      *Blkio  `json:"blkio"`
	Ports      []Port  `json:"ports"`
}

// Port is one published or exposed mapping. Public is 0 when the port is only
// exposed inside Docker's own networks, which the UI renders without a link
// because there is no host address to send the browser to.
type Port struct {
	Public  int    `json:"public"`
	Private int    `json:"private"`
	Type    string `json:"type"`
	IP      string `json:"ip,omitempty"`
}

type Net struct {
	RX     uint64  `json:"rx"`
	TX     uint64  `json:"tx"`
	RXRate float64 `json:"rx_rate"`
	TXRate float64 `json:"tx_rate"`
}

type Blkio struct {
	Read  uint64 `json:"read"`
	Write uint64 `json:"write"`
}

/* ── derived figures ────────────────────────────────────────────────────── */

// cpuPercent measures against the previous tick. 100% = one full core, the
// same scale `docker stats` prints.
func (c *Client) cpuPercent(id string, s *statsJSON) float64 {
	usage, system := s.CPUStats.CPUUsage.TotalUsage, s.CPUStats.SystemUsage
	if usage == nil || system == nil {
		return 0
	}

	c.mu.Lock()
	prev, seen := c.prevCPU[id]
	c.prevCPU[id] = cpuCounters{total: *usage, system: *system}
	c.mu.Unlock()
	if !seen {
		return 0 // first sighting; the next tick has a real delta
	}

	// A restarted container resets its counters — report 0 rather than a
	// nonsensical negative or a huge spike.
	if *usage < prev.total || *system <= prev.system {
		return 0
	}
	cpuDelta := float64(*usage - prev.total)
	sysDelta := float64(*system - prev.system)

	// online_cpus is absent on older daemons; fall back to the per-cpu array.
	ncpu := s.CPUStats.OnlineCPUs
	if ncpu == 0 {
		ncpu = len(s.CPUStats.CPUUsage.PercpuUsage)
	}
	if ncpu == 0 {
		ncpu = 1
	}
	pct := math.Min(cpuDelta/sysDelta*float64(ncpu)*100, float64(ncpu)*100)
	return math.Round(pct*100) / 100
}

func memory(s *statsJSON) (used, limit uint64) {
	if s.MemoryStats.Usage == nil {
		return 0, 0
	}
	used = *s.MemoryStats.Usage
	// Match `docker stats`: subtract page cache so the number reflects the
	// working set. cgroup v2 exposes inactive_file, v1 exposes cache.
	if v, ok := s.MemoryStats.Stats["inactive_file"]; ok {
		used -= min(v, used)
	} else if v, ok := s.MemoryStats.Stats["cache"]; ok {
		used -= min(v, used)
	}
	return used, s.MemoryStats.Limit
}

func (c *Client) network(id string, s *statsJSON, now time.Time) *Net {
	if len(s.Networks) == 0 {
		// Containers on `network_mode: service:...` (e.g. behind gluetun)
		// report no interfaces of their own — their traffic shows up on the
		// provider instead.
		c.mu.Lock()
		delete(c.prevNet, id)
		c.mu.Unlock()
		return nil
	}
	var rx, tx uint64
	for _, n := range s.Networks {
		rx += n.RxBytes
		tx += n.TxBytes
	}

	c.mu.Lock()
	prev, seen := c.prevNet[id]
	c.prevNet[id] = netCounters{at: now, rx: rx, tx: tx}
	c.mu.Unlock()

	out := &Net{RX: rx, TX: tx}
	if seen && now.After(prev.at) {
		dt := now.Sub(prev.at).Seconds()
		// A restarted container resets its counters; clamp instead of going
		// negative.
		if rx > prev.rx {
			out.RXRate = float64(rx-prev.rx) / dt
		}
		if tx > prev.tx {
			out.TXRate = float64(tx-prev.tx) / dt
		}
	}
	return out
}

func blockIO(s *statsJSON) *Blkio {
	entries := s.BlkioStats.IoServiceBytesRecursive
	if len(entries) == 0 {
		return nil // commonly empty under cgroup v2
	}
	var out Blkio
	for _, e := range entries {
		switch {
		case equalFold(e.Op, "read"):
			out.Read += e.Value
		case equalFold(e.Op, "write"):
			out.Write += e.Value
		}
	}
	return &out
}

// identify returns (full name, display name, project).
//
// Runtipi names containers `<project>-<service>-1`, so at phone width three
// Immich containers all truncate to the same "immich_migra…". The compose
// labels carry the service name on its own, which is what actually
// distinguishes them.
func identify(meta containerMeta) (full, display, project string) {
	full = "?"
	if len(meta.Names) > 0 {
		full = trimLeadingSlash(meta.Names[0])
	}
	service := meta.Labels["com.docker.compose.service"]
	project = meta.Labels["com.docker.compose.project"]
	display = service
	if display == "" {
		display = full
	}
	return full, display, project
}

// knownHealth is the set the UI styles. Anything else the daemon writes in
// parentheses (a port mapping, a custom status) is not a health state and must
// not be shown as one.
var knownHealth = map[string]bool{
	"healthy": true, "unhealthy": true, "health: starting": true, "starting": true,
}

// health digs the state out of the human-readable Status string, which is the
// only place /containers/json exposes it.
func health(status string) *string {
	open := lastIndexByte(status, '(')
	if open < 0 {
		return nil
	}
	inner := trimTrailingParen(status[open+1:])
	if !knownHealth[inner] {
		return nil
	}
	return &inner
}

// ports normalises the engine's Ports array for the UI.
//
// A dual-stack publish shows up twice — once on 0.0.0.0 and once on :: — for
// what is one mapping as far as anybody reading the dashboard is concerned, so
// entries are collapsed on (public, private, proto). The surviving IP is kept
// only when every binding for that mapping is to a specific address: a
// wildcard bind tells the browser nothing it does not already know, whereas a
// 127.0.0.1-only publish is worth showing because the link will not work from
// another machine.
func ports(meta containerMeta) []Port {
	if len(meta.Ports) == 0 {
		return nil
	}
	type key struct {
		public, private int
		proto           string
	}
	seen := make(map[key]int, len(meta.Ports))
	out := make([]Port, 0, len(meta.Ports))
	for _, p := range meta.Ports {
		if p.PrivatePort == 0 {
			continue
		}
		proto := p.Type
		if proto == "" {
			proto = "tcp"
		}
		k := key{p.PublicPort, p.PrivatePort, proto}
		if i, ok := seen[k]; ok {
			if isWildcard(p.IP) {
				out[i].IP = ""
			}
			continue
		}
		seen[k] = len(out)
		ip := p.IP
		if isWildcard(ip) {
			ip = ""
		}
		out = append(out, Port{Public: p.PublicPort, Private: p.PrivatePort, Type: proto, IP: ip})
	}

	// Published ports first — those are the ones you can actually click — then
	// ascending, so the order is stable across ticks regardless of how the
	// daemon happened to list them.
	sort.Slice(out, func(i, j int) bool {
		a, b := out[i], out[j]
		if (a.Public == 0) != (b.Public == 0) {
			return b.Public == 0
		}
		if a.Public != b.Public {
			return a.Public < b.Public
		}
		if a.Private != b.Private {
			return a.Private < b.Private
		}
		return a.Type < b.Type
	})
	return out
}

// isWildcard reports whether the daemon bound every address rather than a
// chosen one. An empty IP means the same thing on an exposed-only port.
func isWildcard(ip string) bool {
	return ip == "" || ip == "0.0.0.0" || ip == "::" || ip == "[::]"
}

/* ── sampling ───────────────────────────────────────────────────────────── */

func (c *Client) one(ctx context.Context, meta containerMeta) *Container {
	var s statsJSON
	if err := c.get(ctx, "/containers/"+meta.ID+"/stats?stream=false&one-shot=true", &s); err != nil {
		return nil
	}
	used, limit := memory(&s)
	full, name, project := identify(meta)

	out := &Container{
		ID:       shortID(meta.ID),
		Name:     name,
		FullName: full,
		Project:  project,
		Image:    meta.Image,
		State:    meta.State,
		Status:   meta.Status,
		Health:   health(meta.Status),
		Created:  meta.Created,
		CPU:      c.cpuPercent(meta.ID, &s),
		MemUsed:  used,
		MemLimit: limit,
		PIDs:     s.PidsStats.Current,
		Net:      c.network(meta.ID, &s, time.Now()),
		Blkio:    blockIO(&s),
		Ports:    ports(meta),
	}
	if limit > 0 {
		out.MemPercent = math.Round(float64(used)/float64(limit)*100*100) / 100
	}
	return out
}

// Sample runs one full pass: list containers, then fetch stats for the running
// ones concurrently.
func (c *Client) Sample(ctx context.Context) Result {
	var listing []containerMeta
	if err := c.get(ctx, "/containers/json?all=true", &listing); err != nil {
		return Result{Error: errString(err), Containers: []Container{}}
	}

	var running []containerMeta
	for _, meta := range listing {
		if meta.State == "running" {
			running = append(running, meta)
		}
	}

	results := make([]*Container, len(running))
	sem := make(chan struct{}, max(1, c.cfg.DockerConcurrency))
	var wg sync.WaitGroup
	for i, meta := range running {
		wg.Add(1)
		go func(i int, meta containerMeta) {
			defer wg.Done()
			sem <- struct{}{}
			defer func() { <-sem }()
			results[i] = c.one(ctx, meta)
		}(i, meta)
	}
	wg.Wait()

	containers := make([]Container, 0, len(listing))
	for _, r := range results {
		if r != nil {
			containers = append(containers, *r)
		}
	}

	// Keep stopped containers visible but without stats, so a crashed service
	// is obvious at a glance rather than silently missing from the list.
	for _, meta := range listing {
		if meta.State == "running" {
			continue
		}
		full, name, project := identify(meta)
		containers = append(containers, Container{
			ID: shortID(meta.ID), Name: name, FullName: full, Project: project,
			Image: meta.Image, State: meta.State, Status: meta.Status,
			Created: meta.Created, Ports: ports(meta),
		})
	}

	c.forgetStopped(running)

	sort.SliceStable(containers, func(i, j int) bool {
		a, b := containers[i], containers[j]
		if (a.State == "running") != (b.State == "running") {
			return a.State == "running"
		}
		if a.CPU != b.CPU {
			return a.CPU > b.CPU
		}
		return a.Name < b.Name
	})

	return Result{Containers: containers, Running: len(running), Total: len(listing)}
}

// forgetStopped drops baselines for containers that are gone, so the maps do
// not grow without bound on a host that churns through short-lived jobs.
func (c *Client) forgetStopped(running []containerMeta) {
	live := make(map[string]bool, len(running))
	for _, meta := range running {
		live[meta.ID] = true
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	for id := range c.prevCPU {
		if !live[id] {
			delete(c.prevCPU, id)
		}
	}
	for id := range c.prevNet {
		if !live[id] {
			delete(c.prevNet, id)
		}
	}
}

/* ── small helpers ──────────────────────────────────────────────────────── */

func shortID(id string) string {
	if len(id) > 12 {
		return id[:12]
	}
	return id
}

func trimLeadingSlash(s string) string {
	for len(s) > 0 && s[0] == '/' {
		s = s[1:]
	}
	return s
}

func trimTrailingParen(s string) string {
	for len(s) > 0 && s[len(s)-1] == ')' {
		s = s[:len(s)-1]
	}
	return s
}

func lastIndexByte(s string, b byte) int {
	for i := len(s) - 1; i >= 0; i-- {
		if s[i] == b {
			return i
		}
	}
	return -1
}

func equalFold(a, b string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := 0; i < len(a); i++ {
		ca, cb := a[i], b[i]
		if 'A' <= ca && ca <= 'Z' {
			ca += 'a' - 'A'
		}
		if 'A' <= cb && cb <= 'Z' {
			cb += 'a' - 'A'
		}
		if ca != cb {
			return false
		}
	}
	return true
}

// errString keeps the "Type: message" shape the dashboard already renders for
// Docker failures.
func errString(err error) string {
	return fmt.Sprintf("%T: %v", err, err)
}
