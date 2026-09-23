package server

import (
	"bufio"
	"context"
	"encoding/json"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"testing/fstest"
	"time"

	"github.com/rene-roid/kanshi/internal/access"
	"github.com/rene-roid/kanshi/internal/config"
)

var web = fstest.MapFS{
	"index.html": {Data: []byte(`<link href="/static/style.css"><script src="/static/app.js"></script>`)},
	"app.js":     {Data: []byte(strings.Repeat("console.log('kanshi');\n", 200))},
	"style.css":  {Data: []byte(strings.Repeat("body { color: red }\n", 200))},
}

func newServer(t *testing.T) *Server {
	t.Helper()
	cfg := config.Config{
		PollInterval:    time.Second,
		IdleTimeout:     time.Second,
		DockerHost:      "unix://" + filepath.Join(t.TempDir(), "no-docker.sock"),
		StorageRoots:    []string{t.TempDir()},
		StorageCPU:      100,
		TreeDepth:       2,
		AccessSource:    config.SourceDefault,
		File:            filepath.Join(t.TempDir(), config.FileName),
		StorageInterval: time.Hour,
	}
	s, err := New(cfg, web, "test", t.Logf)
	if err != nil {
		t.Fatal(err)
	}
	return s
}

func do(s *Server, method, target string, edit func(*http.Request)) *httptest.ResponseRecorder {
	r := httptest.NewRequest(method, target, nil)
	r.RemoteAddr = "127.0.0.1:50000"
	r.Host = "localhost:8100"
	if edit != nil {
		edit(r)
	}
	w := httptest.NewRecorder()
	s.Handler().ServeHTTP(w, r)
	return w
}

func TestStaticAssetsAreFingerprinted(t *testing.T) {
	s := newServer(t)
	index := do(s, "GET", "/", nil)
	body := index.Body.String()
	f := s.assets.files["app.js"]
	if !strings.Contains(body, `"/static/app.js?v=`+f.etag+`"`) {
		t.Fatalf("index does not reference the fingerprinted URL: %s", body)
	}
	if index.Header().Get("Cache-Control") != "no-cache" {
		t.Error("the page itself must always be revalidated")
	}

	js := do(s, "GET", "/static/app.js?v="+f.etag, func(r *http.Request) { r.Header.Set("Accept-Encoding", "gzip") })
	if !strings.Contains(js.Header().Get("Cache-Control"), "immutable") || js.Header().Get("Content-Encoding") != "gzip" {
		t.Errorf("headers = %v", js.Header())
	}
	if js.Header().Get("Content-Type") != "text/javascript; charset=utf-8" {
		t.Errorf("content type = %q", js.Header().Get("Content-Type"))
	}
	again := do(s, "GET", "/static/app.js?v="+f.etag, func(r *http.Request) { r.Header.Set("If-None-Match", js.Header().Get("ETag")) })
	if again.Code != http.StatusNotModified {
		t.Errorf("revalidation = %d, want 304", again.Code)
	}
	if do(s, "GET", "/static/nope.js", nil).Code != http.StatusNotFound {
		t.Error("unknown asset should 404")
	}
	if do(s, "GET", "/elsewhere", nil).Code != http.StatusNotFound {
		t.Error("unknown page should 404")
	}
}

func TestRescanNeedsTheDashboard(t *testing.T) {
	s := newServer(t)
	if w := do(s, "GET", "/api/storage/rescan", nil); w.Code != http.StatusMethodNotAllowed {
		t.Errorf("GET = %d", w.Code)
	}
	if w := do(s, "POST", "/api/storage/rescan", nil); w.Code != http.StatusForbidden {
		t.Errorf("POST without the header = %d", w.Code)
	}
	cross := func(r *http.Request) { r.Header.Set("X-Kanshi", "1"); r.Header.Set("Origin", "http://evil.example") }
	if w := do(s, "POST", "/api/storage/rescan", cross); w.Code != http.StatusForbidden {
		t.Errorf("cross-origin POST = %d", w.Code)
	}
	own := func(r *http.Request) { r.Header.Set("X-Kanshi", "1"); r.Header.Set("Origin", "http://localhost:8100") }
	if w := do(s, "POST", "/api/storage/rescan", own); w.Code != http.StatusOK {
		t.Errorf("POST from the dashboard = %d", w.Code)
	}
}

func startAccess(t *testing.T, s *Server) {
	t.Helper()
	l, err := net.Listen("tcp4", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	port := l.Addr().(*net.TCPAddr).Port
	l.Close()
	m := access.NewManager(port, &http.Server{Handler: http.NotFoundHandler()}, t.Logf)
	m.Interfaces = func() []access.Interface { return nil }
	if err := m.Apply(access.Mode{}); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(m.Close)
	s.access = m
}

func TestAccessChangeIsLocalOnly(t *testing.T) {
	s := newServer(t)
	startAccess(t, s)
	post := func(edit func(*http.Request)) *httptest.ResponseRecorder {
		return do(s, "POST", "/api/access", func(r *http.Request) {
			r.Header.Set("X-Kanshi", "1")
			r.Body = ioNopCloser(`{"mode":"tailscale"}`)
			if edit != nil {
				edit(r)
			}
		})
	}

	if w := post(func(r *http.Request) { r.RemoteAddr = "192.168.1.20:4000" }); w.Code != http.StatusForbidden {
		t.Errorf("from the LAN = %d", w.Code)
	}
	if w := post(func(r *http.Request) { r.Host = "attacker.example:8100" }); w.Code != http.StatusForbidden {
		t.Errorf("DNS-rebound host = %d", w.Code)
	}
	if w := post(func(r *http.Request) { r.Header.Del("X-Kanshi") }); w.Code != http.StatusForbidden {
		t.Errorf("without the header = %d", w.Code)
	}

	lan := do(s, "GET", "/api/access", func(r *http.Request) { r.RemoteAddr = "192.168.1.20:4000"; r.Host = "192.168.1.5:8100" })
	var st accessState
	json.Unmarshal(lan.Body.Bytes(), &st)
	if st.Editable || st.Reason == "" {
		t.Errorf("seen from the LAN the setting must be read-only: %+v", st)
	}

	w := post(nil)
	if w.Code != http.StatusOK {
		t.Fatalf("local change = %d %s", w.Code, w.Body)
	}
	json.Unmarshal(w.Body.Bytes(), &st)
	if st.Mode != "tailscale" || st.Source != config.SourceFile {
		t.Errorf("after change: %+v", st)
	}
	saved, _ := os.ReadFile(s.cfg.File)
	if !strings.Contains(string(saved), "KANSHI_ACCESS=tailscale") {
		t.Errorf("not persisted: %q", saved)
	}

	s.accessSource = config.SourceEnv
	if w := post(nil); w.Code != http.StatusConflict {
		t.Errorf("pinned by the environment = %d", w.Code)
	}
}

func TestStreamSendsFrames(t *testing.T) {
	s := newServer(t)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	s.base = ctx
	go s.Poll(ctx)

	ts := httptest.NewServer(s.Handler())
	defer ts.Close()
	resp, err := http.Get(ts.URL + "/api/stream")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	sc := bufio.NewScanner(resp.Body)
	sc.Buffer(make([]byte, 1<<20), 1<<20)
	deadline := time.After(10 * time.Second)
	got := make(chan Frame, 1)
	go func() {
		for sc.Scan() {
			if data, ok := strings.CutPrefix(sc.Text(), "data: "); ok {
				var f Frame
				json.Unmarshal([]byte(data), &f)
				got <- f
				return
			}
		}
	}()
	select {
	case f := <-got:
		if f.Vitals == nil || f.Docker == nil || !f.Docker.Unavailable {
			t.Errorf("frame = %+v", f)
		}
	case <-deadline:
		t.Fatal("no frame within 10s")
	}
}

func ioNopCloser(s string) *nopBody { return &nopBody{strings.NewReader(s)} }

type nopBody struct{ *strings.Reader }

func (nopBody) Close() error { return nil }
