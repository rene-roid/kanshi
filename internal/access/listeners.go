package access

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/netip"
	"sort"
	"strconv"
	"sync"
	"time"
)

// How often a mode that depends on interfaces is re-checked. Tailscale coming
// up after Kanshi, a laptop joining Wi-Fi, a DHCP lease changing: all of them
// are picked up within this long, without a restart.
const rescanEvery = 30 * time.Second

// Manager keeps one listener per address the current mode wants, all feeding
// the same http.Server.
type Manager struct {
	port   int
	server *http.Server
	logf   func(format string, args ...any)
	// Interfaces is swapped out by tests.
	Interfaces func() []Interface

	mu       sync.Mutex
	mode     Mode
	active   map[netip.Addr]*listener
	failures map[netip.Addr]string // last bind error per address, so it is logged once
}

type listener struct {
	ep Endpoint
	ln net.Listener
}

func NewManager(port int, server *http.Server, logf func(string, ...any)) *Manager {
	return &Manager{
		port:       port,
		server:     server,
		logf:       logf,
		Interfaces: SystemInterfaces,
		active:     make(map[netip.Addr]*listener),
		failures:   make(map[netip.Addr]string),
	}
}

// ErrLoopback means the one listener that must always work could not be
// opened — almost always because something else already has the port.
var ErrLoopback = errors.New("cannot listen on localhost")

// Apply switches to a new mode, opening and closing listeners as needed. It
// only fails when 127.0.0.1 cannot be bound; any other address that will not
// bind is logged and retried on the next interface check.
func (m *Manager) Apply(mode Mode) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.mode = mode
	return m.reconcile()
}

func (m *Manager) Mode() Mode {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.mode
}

func (m *Manager) reconcile() error {
	var ifaces []Interface
	if m.mode.dynamic() {
		ifaces = m.Interfaces()
	}
	want := m.mode.Endpoints(ifaces)
	wanted := make(map[netip.Addr]Endpoint, len(want))
	for _, ep := range want {
		wanted[ep.Addr] = ep
	}

	// Close first: switching to "all" has to release 127.0.0.1 before
	// 0.0.0.0 can take the same port, and the reverse going back.
	for addr, l := range m.active {
		if _, ok := wanted[addr]; !ok {
			l.ln.Close()
			delete(m.active, addr)
			if l.ep.Kind != KindLocal {
				m.logf("stopped listening on %s", URL(addr, m.port))
			}
		}
	}
	for addr := range m.failures {
		if _, ok := wanted[addr]; !ok {
			delete(m.failures, addr)
		}
	}

	var loopErr error
	for _, ep := range want {
		if _, ok := m.active[ep.Addr]; ok {
			continue
		}
		ln, err := listen(ep.Addr, m.port)
		if err != nil {
			if ep.Addr == loop4 || (ep.Kind == KindAll && ep.Addr.Is4()) {
				loopErr = fmt.Errorf("%w on port %d: %v", ErrLoopback, m.port, err)
				continue
			}
			// IPv6 loopback and "::" are simply absent on hosts with IPv6
			// turned off. That is not worth a warning, or a retry.
			if ep.Addr == loop6 || ep.Addr == netip.IPv6Unspecified() {
				continue
			}
			if m.failures[ep.Addr] != err.Error() {
				m.logf("warning: cannot listen on %s yet (%v); will keep trying", URL(ep.Addr, m.port), err)
			}
			m.failures[ep.Addr] = err.Error()
			continue
		}
		if _, failed := m.failures[ep.Addr]; failed {
			m.logf("now listening on %s", URL(ep.Addr, m.port))
			delete(m.failures, ep.Addr)
		}
		m.active[ep.Addr] = &listener{ep: ep, ln: ln}
		go func() {
			// Serve returns once the listener is closed, either here or by
			// Shutdown. Neither is an error worth reporting.
			_ = m.server.Serve(ln)
		}()
	}
	return loopErr
}

func listen(addr netip.Addr, port int) (net.Listener, error) {
	network := "tcp4"
	if addr.Is6() {
		// tcp6 sets IPV6_V6ONLY, so "::" does not also try to take 0.0.0.0.
		network = "tcp6"
	}
	return net.Listen(network, net.JoinHostPort(addr.String(), strconv.Itoa(port)))
}

// Watch re-applies the current mode as interfaces come and go, and retries
// addresses that failed to bind. It returns when ctx is cancelled.
func (m *Manager) Watch(ctx context.Context) {
	t := time.NewTicker(rescanEvery)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
		m.mu.Lock()
		if m.mode.dynamic() || len(m.failures) > 0 {
			_ = m.reconcile()
		}
		m.mu.Unlock()
	}
}

// Close stops every listener.
func (m *Manager) Close() {
	m.mu.Lock()
	defer m.mu.Unlock()
	for addr, l := range m.active {
		l.ln.Close()
		delete(m.active, addr)
	}
}

// Link is one address the dashboard can be opened at.
type Link struct {
	Kind Kind   `json:"kind"`
	URL  string `json:"url"`
}

// Links lists where the dashboard is reachable right now. Loopback collapses
// into one localhost link, and a wildcard listener expands into the machine's
// actual addresses, since "0.0.0.0" is not something you can open.
func (m *Manager) Links() []Link {
	m.mu.Lock()
	var eps []Endpoint
	wildcard := false
	for _, l := range m.active {
		if l.ep.Kind == KindAll {
			wildcard = true
			continue
		}
		eps = append(eps, l.ep)
	}
	m.mu.Unlock()

	if wildcard {
		eps = Reachable(m.Interfaces())
	}
	rank := map[Kind]int{KindLocal: 0, KindTailscale: 1, KindLAN: 2, KindCustom: 3, KindOther: 4}
	sort.SliceStable(eps, func(i, j int) bool {
		a, b := eps[i], eps[j]
		if rank[a.Kind] != rank[b.Kind] {
			return rank[a.Kind] < rank[b.Kind]
		}
		if a.Addr.Is4() != b.Addr.Is4() {
			return a.Addr.Is4() // IPv4 first: shorter, and what people type
		}
		return a.Addr.Less(b.Addr)
	})

	out := []Link{}
	local := false
	for _, ep := range eps {
		if ep.Kind == KindLocal {
			if !local {
				out = append(out, Link{KindLocal, "http://localhost:" + strconv.Itoa(m.port)})
				local = true
			}
			continue
		}
		out = append(out, Link{ep.Kind, URL(ep.Addr, m.port)})
	}
	return out
}

// URL formats an address the way a browser wants it.
func URL(addr netip.Addr, port int) string {
	return "http://" + net.JoinHostPort(addr.String(), strconv.Itoa(port))
}
