// Package server wires the poller, the storage scanner and the HTTP surface
// together.
package server

import (
	"compress/gzip"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"io/fs"
	"net/http"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/rene-roid/kanshi/internal/access"
	"github.com/rene-roid/kanshi/internal/config"
	"github.com/rene-roid/kanshi/internal/dockerstats"
	"github.com/rene-roid/kanshi/internal/history"
	"github.com/rene-roid/kanshi/internal/roots"
	"github.com/rene-roid/kanshi/internal/storage"
	"github.com/rene-roid/kanshi/internal/vitals"
)

// Frame is one push to the browser. Vitals and Docker are explicit nulls
// rather than omitted, because app.js tests each for truthiness. Storage is
// only the walk queue's status: the page refetches the folder it shows when
// a walk finishes, so nothing polls for it.
type Frame struct {
	Vitals  *vitals.Sample      `json:"vitals"`
	Docker  *dockerstats.Result `json:"docker"`
	Storage *storage.Status     `json:"storage"`
	Error   string              `json:"error,omitempty"`
}

// Server holds the shared state the poller writes and the handlers read.
type Server struct {
	cfg     config.Config
	version string
	logf    func(format string, args ...any)
	roots   *roots.Resolver
	vitals  *vitals.Reader
	docker  *dockerstats.Client
	storage *storage.Scanner
	history *history.Store // nil unless KANSHI_HISTORY_DAYS is set
	assets  *assets

	// base outlives any single request. Work that mutates shared state, such
	// as a sample, is started under it rather than the request context, so a
	// browser navigating away cannot abort it for everyone else.
	base context.Context

	mu       sync.RWMutex
	latest   Frame
	latestAt time.Time
	payload  []byte // latest, already marshalled, for newly connected streams
	subs     map[chan []byte]struct{}
	lastSee  time.Time

	// wake releases the poller from its idle sleep. Buffered by one so a
	// signal is never lost and no sender ever blocks.
	wake chan struct{}

	// access is set once Run starts listening. accessSource is where the
	// current mode came from, which decides whether the page may change it.
	access       *access.Manager
	accessMu     sync.Mutex
	accessSource config.Source
	saveOnce     sync.Once
	saveOK       bool

	// OnReady, when set, runs once the dashboard is listening.
	OnReady func()
}

func New(cfg config.Config, web fs.FS, version string, logf func(string, ...any)) (*Server, error) {
	a, err := loadAssets(web, cfg.WebDir != "")
	if err != nil {
		return nil, err
	}
	r := roots.NewResolver(cfg.StorageRoots, cfg.HostRoot)
	// History is an extra, so a folder that cannot be written costs the
	// timeline, not the dashboard.
	var h *history.Store
	switch {
	case cfg.HistoryRetention <= 0:
	case cfg.HistoryDir == "":
		logf("history is off: no folder to keep it in; set KANSHI_HISTORY_DIR")
	default:
		if h, err = history.Open(cfg.HistoryDir, cfg.HistoryRetention, logf); err != nil {
			logf("history is off: %v", err)
			h = nil
		}
	}
	return &Server{
		base:         context.Background(),
		cfg:          cfg,
		version:      version,
		logf:         logf,
		roots:        r,
		vitals:       vitals.New(r),
		docker:       dockerstats.New(cfg),
		storage:      storage.New(cfg, r, logf),
		history:      h,
		assets:       a,
		subs:         make(map[chan []byte]struct{}),
		wake:         make(chan struct{}, 1),
		accessSource: cfg.AccessSource,
	}, nil
}

/* ── polling ────────────────────────────────────────────────────────────── */

// sampleOnce takes one reading of both halves at the same moment. The vitals
// read is pure /proc work, so it runs alongside the Docker round trips rather
// than adding its latency to them.
func (s *Server) sampleOnce(ctx context.Context) Frame {
	var host vitals.Sample
	var containers dockerstats.Result

	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		host = s.vitals.Sample()
	}()
	containers = s.docker.Sample(ctx)
	wg.Wait()

	scan := s.storage.Status()
	frame := Frame{Vitals: &host, Docker: &containers, Storage: &scan}
	payload, err := json.Marshal(frame)
	if err != nil {
		payload, _ = json.Marshal(Frame{Error: err.Error()})
	}
	s.mu.Lock()
	s.latest, s.payload, s.latestAt = frame, payload, time.Now()
	s.mu.Unlock()
	return frame
}

// seed fills the delta baselines so the first frame shows real numbers.
//
// Both CPU readings are differences against a previous sample, so a cold poller
// would otherwise publish a screen of zeros. One throwaway pass plus a short
// gap costs ~1s and makes the first frame the user sees correct.
func (s *Server) seed(ctx context.Context) {
	s.vitals.Prime()
	s.docker.Sample(ctx)
	select {
	case <-ctx.Done():
		return
	case <-time.After(time.Second):
	}
	s.vitals.Prime()
}

// Poll is the live loop. Once nobody has been connected for IdleTimeout it
// stops touching /proc and the Docker daemon entirely, and sleeps until a
// browser comes back.
func (s *Server) Poll(ctx context.Context) {
	timer := time.NewTimer(time.Hour)
	timer.Stop()
	defer timer.Stop()

	s.seed(ctx)
	for ctx.Err() == nil {
		if s.idle() {
			// Nobody is watching. The timer is only a safety net: waking
			// here does nothing but check again, and the baselines are only
			// re-seeded once a browser has actually come back.
			timer.Reset(time.Minute)
			select {
			case <-ctx.Done():
				return
			case <-timer.C:
			case <-s.wake:
			}
			timer.Stop()
			if !s.idle() {
				s.seed(ctx)
			}
			continue
		}

		s.sampleOnce(ctx)
		if ctx.Err() != nil {
			return
		}
		s.mu.RLock()
		payload := s.payload
		s.mu.RUnlock()
		s.broadcast(payload)

		timer.Reset(s.cfg.PollInterval)
		select {
		case <-ctx.Done():
			return
		case <-timer.C:
		}
	}
}

func (s *Server) idle() bool {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return len(s.subs) == 0 && time.Since(s.lastSee) > s.cfg.IdleTimeout
}

// touch records browser activity and wakes an idle poller.
func (s *Server) touch() {
	s.mu.Lock()
	s.lastSee = time.Now()
	s.mu.Unlock()
	signal(s.wake)
}

func signal(ch chan struct{}) {
	select {
	case ch <- struct{}{}:
	default: // already pending
	}
}

func (s *Server) broadcast(payload []byte) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	for ch := range s.subs {
		select {
		case ch <- payload:
		default:
			// The subscriber is behind. Drop its stale frame and hand it the
			// fresh one — a slow phone must never stall the poller.
			select {
			case <-ch:
			default:
			}
			select {
			case ch <- payload:
			default:
			}
		}
	}
}

/* ── history ────────────────────────────────────────────────────────────── */

// record writes one history entry every HistoryInterval, watched or not. While
// a browser keeps the poller busy it reuses the poller's latest frame; once
// the poller has gone idle it samples on its own. Every rate is a delta
// against the previous sample, so an idle-time record averages the whole
// interval rather than the last five seconds.
func (s *Server) record(ctx context.Context) {
	if s.history == nil {
		return
	}
	defer s.history.Close()
	tick := time.NewTicker(s.cfg.HistoryInterval)
	defer tick.Stop()
	var prev time.Time
	for {
		select {
		case <-ctx.Done():
			return
		case <-tick.C:
		}
		s.mu.RLock()
		frame, at := s.latest, s.latestAt
		s.mu.RUnlock()
		if s.idle() || frame.Vitals == nil {
			frame = s.sampleOnce(ctx)
			if ctx.Err() != nil {
				return
			}
			at = time.Now()
		} else if !at.After(prev) {
			// The poller has not ticked since the last record, which only
			// happens when records are more frequent than polls.
			continue
		}
		prev = at
		if err := s.history.Add(at, frame.Vitals.CPU.Percent, frame.Vitals.Memory.Percent, historyFrame(frame)); err != nil {
			s.logf("history: %v", err)
		}
	}
}

// historyFrame is what a record keeps: the vitals and containers, without
// the parts a look back has no use for. Port mappings are most of a
// container's bytes and a link into the past opens the present anyway.
func historyFrame(f Frame) []byte {
	out := struct {
		Vitals *vitals.Sample      `json:"vitals"`
		Docker *dockerstats.Result `json:"docker"`
	}{Vitals: f.Vitals}
	if f.Docker != nil {
		d := *f.Docker
		d.Containers = make([]dockerstats.Container, len(f.Docker.Containers))
		for i, c := range f.Docker.Containers {
			c.Ports, c.Blkio, c.Image = nil, nil, ""
			d.Containers[i] = c
		}
		out.Docker = &d
	}
	b, _ := json.Marshal(out)
	return b
}

// handleHistory is the timeline: /api/history?from=…&to=…&n=… in Unix
// seconds, at most n points (default 600) with the busiest kept per bucket.
func (s *Server) handleHistory(w http.ResponseWriter, r *http.Request) {
	if s.history == nil {
		writeJSON(w, r, map[string]bool{"enabled": false})
		return
	}
	q := r.URL.Query()
	now := time.Now().Unix()
	to := queryInt(q.Get("to"), now)
	from := queryInt(q.Get("from"), to-3600)
	n := int(min(max(queryInt(q.Get("n"), 600), 10), 2000))
	first, last := s.history.Span()
	points := s.history.Series(from, to, n)
	if points == nil {
		points = []history.Point{}
	}
	writeJSON(w, r, map[string]any{
		"enabled":   true,
		"interval":  s.cfg.HistoryInterval.Seconds(),
		"retention": s.cfg.HistoryRetention.Seconds(),
		"first":     first,
		"last":      last,
		"points":    points,
	})
}

// handleHistoryAt returns one recorded frame: /api/history/at?t=…&dir=…,
// where dir is 0 for the record nearest t, -1 for the one before and 1 for
// the one after.
func (s *Server) handleHistoryAt(w http.ResponseWriter, r *http.Request) {
	if s.history == nil {
		http.Error(w, "history is off", http.StatusNotFound)
		return
	}
	q := r.URL.Query()
	t, err := strconv.ParseInt(q.Get("t"), 10, 64)
	if err != nil {
		http.Error(w, "bad t", http.StatusBadRequest)
		return
	}
	p, frame, ok := s.history.At(t, int(queryInt(q.Get("dir"), 0)))
	if !ok {
		http.Error(w, "nothing recorded there", http.StatusNotFound)
		return
	}
	writeJSON(w, r, map[string]any{"t": p.T, "frame": frame})
}

func queryInt(v string, def int64) int64 {
	n, err := strconv.ParseInt(v, 10, 64)
	if err != nil {
		return def
	}
	return n
}

/* ── handlers ───────────────────────────────────────────────────────────── */

func (s *Server) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /api/vitals", s.handleVitals)
	mux.HandleFunc("GET /api/containers", s.handleContainers)
	mux.HandleFunc("GET /api/storage", s.handleStorage)
	mux.HandleFunc("POST /api/storage/rescan", s.handleRescan)
	mux.HandleFunc("GET /api/storage/dir", s.handleStorageDir)
	mux.HandleFunc("GET /api/config", s.handleConfig)
	mux.HandleFunc("GET /api/access", s.handleAccess)
	mux.HandleFunc("POST /api/access", s.handleSetAccess)
	mux.HandleFunc("GET /api/stream", s.handleStream)
	mux.HandleFunc("GET /api/history", s.handleHistory)
	mux.HandleFunc("GET /api/history/at", s.handleHistoryAt)
	mux.HandleFunc("GET /healthz", s.handleHealth)
	mux.HandleFunc("GET /static/", s.assets.serveStatic)
	mux.HandleFunc("GET /{$}", s.assets.serveIndex)
	return mux
}

// Responses larger than this are gzipped for clients that accept it. Below
// it the header overhead eats the saving.
const gzipMin = 1024

var gzipPool = sync.Pool{New: func() any {
	w, _ := gzip.NewWriterLevel(io.Discard, gzip.BestSpeed)
	return w
}}

func acceptsGzip(r *http.Request) bool {
	return strings.Contains(r.Header.Get("Accept-Encoding"), "gzip")
}

func writeJSON(w http.ResponseWriter, r *http.Request, v any) {
	body, err := json.Marshal(v)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	h := w.Header()
	h.Set("Content-Type", "application/json")
	h.Set("Cache-Control", "no-cache")
	h.Set("Vary", "Accept-Encoding")
	if len(body) < gzipMin || !acceptsGzip(r) {
		_, _ = w.Write(body)
		return
	}
	h.Set("Content-Encoding", "gzip")
	gz := gzipPool.Get().(*gzip.Writer)
	gz.Reset(w)
	_, _ = gz.Write(body)
	_ = gz.Close()
	gzipPool.Put(gz)
}

func (s *Server) handleVitals(w http.ResponseWriter, r *http.Request) {
	s.touch()
	s.mu.RLock()
	latest := s.latest
	s.mu.RUnlock()
	if latest.Vitals == nil {
		latest = s.sampleOnce(s.base)
	}
	writeJSON(w, r, latest.Vitals)
}

func (s *Server) handleContainers(w http.ResponseWriter, r *http.Request) {
	s.touch()
	s.mu.RLock()
	latest := s.latest
	s.mu.RUnlock()
	if latest.Docker == nil {
		latest = s.sampleOnce(s.base)
	}
	writeJSON(w, r, latest.Docker)
}

func (s *Server) handleStorage(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, r, s.storage.Snapshot())
}

// handleStorageDir reads one directory and sizes its subfolders from the
// cache: /api/storage/dir?root=0&path=home/yuuki. Subfolders the cache is
// missing come back pending, and are walked in the background.
func (s *Server) handleStorageDir(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	root, err := strconv.Atoi(q.Get("root"))
	if err != nil {
		http.Error(w, "bad root", http.StatusBadRequest)
		return
	}
	listing, ok := s.storage.List(root, q.Get("path"))
	if !ok {
		http.Error(w, "no such root", http.StatusNotFound)
		return
	}
	writeJSON(w, r, listing)
}

// handleRescan re-walks the subfolders of one directory, named the same way
// as for handleStorageDir. It has to come from the dashboard itself: the
// custom header cannot be set by a plain form or an <img> on some other site,
// so a page you happen to visit cannot keep your disks busy.
func (s *Server) handleRescan(w http.ResponseWriter, r *http.Request) {
	if !sameOrigin(r) {
		http.Error(w, "forbidden", http.StatusForbidden)
		return
	}
	q := r.URL.Query()
	root, err := strconv.Atoi(q.Get("root"))
	if err != nil {
		http.Error(w, "bad root", http.StatusBadRequest)
		return
	}
	status, ok := s.storage.Rescan(root, q.Get("path"))
	if !ok {
		http.Error(w, "no such root", http.StatusNotFound)
		return
	}
	writeJSON(w, r, status)
}

func (s *Server) handleConfig(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, r, map[string]any{
		"version":          s.version,
		"os":               runtime.GOOS,
		"poll_interval":    s.cfg.PollInterval.Seconds(),
		"storage_interval": s.cfg.StorageInterval.Seconds(),
		"tree_depth":       s.cfg.TreeDepth,
	})
}

func (s *Server) handleHealth(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, r, map[string]bool{"ok": true})
}

func (s *Server) handleStream(w http.ResponseWriter, r *http.Request) {
	flusher, ok := w.(http.Flusher)
	if !ok {
		http.Error(w, "streaming unsupported", http.StatusInternalServerError)
		return
	}

	ch := make(chan []byte, 1)
	s.mu.Lock()
	s.subs[ch] = struct{}{}
	s.mu.Unlock()
	s.touch()
	defer func() {
		s.mu.Lock()
		delete(s.subs, ch)
		s.mu.Unlock()
	}()

	h := w.Header()
	h.Set("Content-Type", "text/event-stream")
	h.Set("Cache-Control", "no-cache, no-transform")
	h.Set("X-Accel-Buffering", "no") // tell any reverse proxy not to buffer us
	h.Set("Connection", "keep-alive")

	// One gzip stream for the life of the connection. Consecutive frames are
	// nearly identical, and deflate's window spans several of them, so each
	// frame after the first shrinks to a few hundred bytes — a fraction of
	// what a phone on mobile data would otherwise pull every tick.
	var out io.Writer = w
	var gz *gzip.Writer
	if acceptsGzip(r) {
		h.Set("Content-Encoding", "gzip")
		h.Set("Vary", "Accept-Encoding")
		gz, _ = gzip.NewWriterLevel(w, gzip.BestSpeed)
		defer gz.Close()
		out = gz
	}
	w.WriteHeader(http.StatusOK)
	send := func(chunks ...string) {
		for _, c := range chunks {
			_, _ = io.WriteString(out, c)
		}
		if gz != nil {
			_ = gz.Flush()
		}
		flusher.Flush()
	}

	// Hand the newcomer the last frame immediately, so a reconnecting phone
	// paints real numbers instead of waiting out a poll interval.
	s.mu.RLock()
	payload := s.payload
	s.mu.RUnlock()
	if payload != nil {
		send("data: ", string(payload), "\n\n")
	} else {
		send(": hello\n\n") // gets the headers and the gzip header out now
	}

	keepalive := time.NewTicker(25 * time.Second)
	defer keepalive.Stop()

	for {
		select {
		case <-r.Context().Done():
			return
		case <-s.base.Done():
			// A stream never ends on its own, so without this every shutdown
			// would sit out the full Shutdown grace period waiting on it.
			return
		case payload := <-ch:
			s.touch()
			send("data: ", string(payload), "\n\n")
		case <-keepalive.C:
			// Keeps mobile proxies from closing an idle stream.
			send(": keepalive\n\n")
		}
	}
}

/* ── lifecycle ──────────────────────────────────────────────────────────── */

// Run starts the background workers, listens according to mode, and serves
// until ctx is cancelled.
func (s *Server) Run(ctx context.Context, mode access.Mode) error {
	// Assigned before the listener accepts anything, so no handler can observe
	// the placeholder set in New.
	s.base = ctx

	srv := &http.Server{
		Handler: s.Handler(),
		// No write deadline: an SSE stream is meant to stay open indefinitely.
		ReadHeaderTimeout: 10 * time.Second,
		IdleTimeout:       65 * time.Second,
	}
	mgr := access.NewManager(s.cfg.Port, srv, s.logf)
	if err := mgr.Apply(mode); err != nil {
		return err
	}
	s.accessMu.Lock()
	s.access = mgr
	s.accessMu.Unlock()
	s.banner(mode)
	if s.OnReady != nil {
		go s.OnReady()
	}

	var wg sync.WaitGroup
	wg.Add(4)
	go func() { defer wg.Done(); s.Poll(ctx) }()
	go func() { defer wg.Done(); s.record(ctx) }()
	go func() { defer wg.Done(); s.storage.Loop(ctx) }()
	go func() { defer wg.Done(); mgr.Watch(ctx) }()

	<-ctx.Done()

	shutdown, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	_ = srv.Shutdown(shutdown)
	mgr.Close()
	s.docker.Close()
	wg.Wait()
	s.storage.Close()
	return nil
}

func (s *Server) banner(mode access.Mode) {
	from := ""
	switch s.accessSource {
	case config.SourceFlag:
		from = " (from --access)"
	case config.SourceEnv:
		from = " (from the environment)"
	case config.SourceFile:
		from = " (from " + s.cfg.File + ")"
	}
	fmt.Printf("kanshi %s — access: %s%s\n", s.version, mode, from)
	for _, l := range s.access.Links() {
		fmt.Printf("  %-10s %s\n", kindLabel(l.Kind)+":", l.URL)
	}
	if mode.All {
		s.logf("warning: access \"all\" listens on every interface, and kanshi has no login")
	}
}

func kindLabel(k access.Kind) string {
	switch k {
	case access.KindLocal:
		return "Local"
	case access.KindLAN:
		return "LAN"
	case access.KindTailscale:
		return "Tailscale"
	case access.KindCustom:
		return "Address"
	}
	return "Other"
}
