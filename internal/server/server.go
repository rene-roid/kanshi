// Package server wires the poller, the storage scanner and the HTTP surface
// together.
package server

import (
	"context"
	"encoding/json"
	"fmt"
	"io/fs"
	"net"
	"net/http"
	"strconv"
	"sync"
	"time"

	"github.com/yuuki824/kanshi/internal/config"
	"github.com/yuuki824/kanshi/internal/dockerstats"
	"github.com/yuuki824/kanshi/internal/storage"
	"github.com/yuuki824/kanshi/internal/vitals"
)

// Frame is one push to the browser. Vitals and Docker are explicit nulls
// rather than omitted, because app.js tests each for truthiness. Storage is
// only the scan status: the page refetches the storage map itself when the
// scan timestamp moves, so nothing polls for it.
type Frame struct {
	Vitals  *vitals.Sample      `json:"vitals"`
	Docker  *dockerstats.Result `json:"docker"`
	Storage *storage.Status     `json:"storage"`
	Error   string              `json:"error,omitempty"`
}

// Server holds the shared state the poller writes and the handlers read.
type Server struct {
	cfg     config.Config
	vitals  *vitals.Reader
	docker  *dockerstats.Client
	storage *storage.Scanner
	web     fs.FS

	// base outlives any single request. Work that mutates shared state — a
	// sample, a storage walk — is started under it rather than the request
	// context, so a browser navigating away mid-walk cannot abort a 76-second
	// scan and leave an error on the snapshot everyone else reads.
	base context.Context

	mu      sync.RWMutex
	latest  Frame
	subs    map[chan []byte]struct{}
	lastSee time.Time

	// wake releases the poller from its idle sleep. Buffered by one so a
	// signal is never lost and no sender ever blocks.
	wake chan struct{}
}

func New(cfg config.Config, web fs.FS) *Server {
	return &Server{
		base:    context.Background(),
		cfg:     cfg,
		vitals:  vitals.New(cfg),
		docker:  dockerstats.New(cfg),
		storage: storage.New(cfg),
		web:     web,
		subs:    make(map[chan []byte]struct{}),
		wake:    make(chan struct{}, 1),
	}
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
	s.mu.Lock()
	s.latest = frame
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

// Poll is the live loop. It stops touching the Docker socket entirely once
// nobody has been connected for IdleTimeout.
func (s *Server) Poll(ctx context.Context) {
	s.seed(ctx)
	for {
		if s.idle() {
			// Nobody is watching: sleep until a new subscriber or a REST
			// request wakes us, then re-seed because the baselines are stale.
			select {
			case <-ctx.Done():
				return
			case <-s.wake:
			case <-time.After(time.Minute):
			}
			s.seed(ctx)
			continue
		}

		frame := s.sampleOnce(ctx)
		if ctx.Err() != nil {
			return
		}
		s.broadcast(frame)

		select {
		case <-ctx.Done():
			return
		case <-time.After(s.cfg.PollInterval):
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
	select {
	case s.wake <- struct{}{}:
	default: // already pending
	}
}

func (s *Server) broadcast(frame Frame) {
	payload, err := json.Marshal(frame)
	if err != nil {
		payload, _ = json.Marshal(Frame{Error: err.Error()})
	}
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

/* ── handlers ───────────────────────────────────────────────────────────── */

func (s *Server) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("/api/vitals", s.handleVitals)
	mux.HandleFunc("/api/containers", s.handleContainers)
	mux.HandleFunc("/api/storage", s.handleStorage)
	mux.HandleFunc("/api/storage/rescan", s.handleRescan)
	mux.HandleFunc("/api/storage/dir", s.handleStorageDir)
	mux.HandleFunc("/api/config", s.handleConfig)
	mux.HandleFunc("/api/stream", s.handleStream)
	mux.HandleFunc("/healthz", s.handleHealth)
	mux.Handle("/static/", http.StripPrefix("/static/", http.FileServer(http.FS(s.web))))
	mux.HandleFunc("/", s.handleIndex)
	return mux
}

func writeJSON(w http.ResponseWriter, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "no-cache")
	_ = json.NewEncoder(w).Encode(v)
}

func (s *Server) handleVitals(w http.ResponseWriter, _ *http.Request) {
	s.touch()
	s.mu.RLock()
	latest := s.latest
	s.mu.RUnlock()
	if latest.Vitals == nil {
		latest = s.sampleOnce(s.base)
	}
	writeJSON(w, latest.Vitals)
}

func (s *Server) handleContainers(w http.ResponseWriter, _ *http.Request) {
	s.touch()
	s.mu.RLock()
	latest := s.latest
	s.mu.RUnlock()
	if latest.Docker == nil {
		latest = s.sampleOnce(s.base)
	}
	writeJSON(w, latest.Docker)
}

func (s *Server) handleStorage(w http.ResponseWriter, _ *http.Request) {
	snap := s.storage.Snapshot()
	// The very first request arrives before the background loop has finished
	// its opening walk. Kick one off and return immediately rather than
	// blocking the request for the walk's full length — the live stream
	// carries the percentage from here on.
	if snap.ScannedAt == nil && !snap.Scanning {
		snap = s.storage.ScanAsync(s.base, true)
	}
	writeJSON(w, snap)
}

// handleStorageDir serves one directory of the cached walk:
// /api/storage/dir?root=0&path=home/yuuki. It never touches the disk.
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
	writeJSON(w, listing)
}

func (s *Server) handleRescan(w http.ResponseWriter, _ *http.Request) {
	writeJSON(w, s.storage.ScanAsync(s.base, false))
}

func (s *Server) handleConfig(w http.ResponseWriter, _ *http.Request) {
	writeJSON(w, map[string]any{
		"poll_interval":    s.cfg.PollInterval.Seconds(),
		"storage_interval": s.cfg.StorageInterval.Seconds(),
		"tree_depth":       s.cfg.TreeDepth,
	})
}

func (s *Server) handleHealth(w http.ResponseWriter, _ *http.Request) {
	writeJSON(w, map[string]bool{"ok": true})
}

func (s *Server) handleIndex(w http.ResponseWriter, r *http.Request) {
	if r.URL.Path != "/" {
		http.NotFound(w, r)
		return
	}
	index, err := fs.ReadFile(s.web, "index.html")
	if err != nil {
		http.Error(w, "index.html missing", http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Header().Set("Cache-Control", "no-cache")
	_, _ = w.Write(index)
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
	w.WriteHeader(http.StatusOK)

	// Hand the newcomer the last frame immediately, so a reconnecting phone
	// paints real numbers instead of waiting out a poll interval.
	s.mu.RLock()
	latest := s.latest
	s.mu.RUnlock()
	if latest.Vitals != nil {
		if payload, err := json.Marshal(latest); err == nil {
			fmt.Fprintf(w, "data: %s\n\n", payload)
			flusher.Flush()
		}
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
			fmt.Fprintf(w, "data: %s\n\n", payload)
			flusher.Flush()
		case <-keepalive.C:
			// Keeps mobile proxies from closing an idle stream.
			fmt.Fprint(w, ": keepalive\n\n")
			flusher.Flush()
		}
	}
}

/* ── lifecycle ──────────────────────────────────────────────────────────── */

// Run starts the background workers and serves until ctx is cancelled.
func (s *Server) Run(ctx context.Context) error {
	// Assigned before the listener accepts anything, so no handler can observe
	// the placeholder set in New.
	s.base = ctx

	var wg sync.WaitGroup
	wg.Add(2)
	go func() { defer wg.Done(); s.Poll(ctx) }()
	go func() { defer wg.Done(); s.storage.Loop(ctx) }()

	addr := net.JoinHostPort(s.cfg.Host, strconv.Itoa(s.cfg.Port))
	srv := &http.Server{
		Addr:    addr,
		Handler: s.Handler(),
		// No write deadline: an SSE stream is meant to stay open indefinitely.
		ReadHeaderTimeout: 10 * time.Second,
		IdleTimeout:       65 * time.Second,
	}

	listener, err := net.Listen("tcp", addr)
	if err != nil {
		return err
	}
	fmt.Printf("kanshi listening on http://%s\n", addr)

	errc := make(chan error, 1)
	go func() { errc <- srv.Serve(listener) }()

	select {
	case err := <-errc:
		return err
	case <-ctx.Done():
	}

	shutdown, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	_ = srv.Shutdown(shutdown)
	s.docker.Close()
	wg.Wait()
	return nil
}
