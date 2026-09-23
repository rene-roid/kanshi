package server

import (
	"bytes"
	"compress/gzip"
	"crypto/sha256"
	"encoding/hex"
	"io/fs"
	"net/http"
	"path"
	"strconv"
	"strings"
)

// asset is one embedded file, prepared once at startup: hashed for its ETag
// and cache-busting URL, and gzipped ahead of time so no request pays for it.
type asset struct {
	body  []byte
	gz    []byte // nil when compression would not help (PNGs, tiny files)
	etag  string
	ctype string
}

// assets serves the frontend.
//
// index.html refers to every other file as /static/<name>?v=<hash>. The hash
// changes whenever the file does, so those URLs can be cached forever and a
// returning phone downloads nothing but the page itself — which it only
// revalidates, with a 304 when nothing changed.
type assets struct {
	dev   bool // KANSHI_WEB_DIR: read from disk on every request instead
	web   fs.FS
	index *asset
	files map[string]*asset
}

func loadAssets(web fs.FS, dev bool) (*assets, error) {
	a := &assets{dev: dev, web: web, files: make(map[string]*asset)}
	if dev {
		return a, nil
	}
	entries, err := fs.ReadDir(web, ".")
	if err != nil {
		return nil, err
	}
	for _, e := range entries {
		if e.IsDir() || e.Name() == "index.html" {
			continue
		}
		body, err := fs.ReadFile(web, e.Name())
		if err != nil {
			return nil, err
		}
		a.files[e.Name()] = newAsset(e.Name(), body)
	}

	index, err := fs.ReadFile(web, "index.html")
	if err != nil {
		return nil, err
	}
	for name, f := range a.files {
		index = bytes.ReplaceAll(index, []byte(`"/static/`+name+`"`), []byte(`"/static/`+name+`?v=`+f.etag+`"`))
	}
	a.index = newAsset("index.html", index)
	return a, nil
}

func newAsset(name string, body []byte) *asset {
	sum := sha256.Sum256(body)
	a := &asset{body: body, etag: hex.EncodeToString(sum[:8]), ctype: contentType(name)}
	if strings.HasPrefix(a.ctype, "text/") || strings.HasPrefix(a.ctype, "image/svg") {
		var buf bytes.Buffer
		gz, _ := gzip.NewWriterLevel(&buf, gzip.BestCompression)
		_, _ = gz.Write(body)
		_ = gz.Close()
		if buf.Len() < len(body)*9/10 {
			a.gz = buf.Bytes()
		}
	}
	return a
}

// contentType is a fixed table rather than mime.TypeByExtension, which on
// Windows reads the registry — where a stray install can map .js to
// text/plain and stop the browser from running the script.
func contentType(name string) string {
	switch path.Ext(name) {
	case ".html":
		return "text/html; charset=utf-8"
	case ".css":
		return "text/css; charset=utf-8"
	case ".js":
		return "text/javascript; charset=utf-8"
	case ".svg":
		return "image/svg+xml"
	case ".png":
		return "image/png"
	case ".ico":
		return "image/x-icon"
	case ".json", ".webmanifest":
		return "application/json"
	}
	return "application/octet-stream"
}

func (a *assets) serveIndex(w http.ResponseWriter, r *http.Request) {
	if a.dev {
		a.serveDev(w, r, "index.html")
		return
	}
	a.write(w, r, a.index, false)
}

func (a *assets) serveStatic(w http.ResponseWriter, r *http.Request) {
	name := strings.TrimPrefix(r.URL.Path, "/static/")
	if a.dev {
		a.serveDev(w, r, name)
		return
	}
	f := a.files[name]
	if f == nil {
		http.NotFound(w, r)
		return
	}
	a.write(w, r, f, r.URL.Query().Get("v") == f.etag)
}

func (a *assets) write(w http.ResponseWriter, r *http.Request, f *asset, immutable bool) {
	h := w.Header()
	h.Set("Content-Type", f.ctype)
	h.Set("Vary", "Accept-Encoding")
	if immutable {
		h.Set("Cache-Control", "public, max-age=31536000, immutable")
	} else {
		h.Set("Cache-Control", "no-cache")
	}
	// Weak, because the gzipped and plain bodies share it.
	h.Set("ETag", `W/"`+f.etag+`"`)
	if strings.Contains(r.Header.Get("If-None-Match"), `"`+f.etag+`"`) {
		w.WriteHeader(http.StatusNotModified)
		return
	}
	body := f.body
	if f.gz != nil && acceptsGzip(r) {
		h.Set("Content-Encoding", "gzip")
		body = f.gz
	}
	h.Set("Content-Length", strconv.Itoa(len(body)))
	if r.Method != http.MethodHead {
		_, _ = w.Write(body)
	}
}

// serveDev reads straight from KANSHI_WEB_DIR, uncached, so an edit shows up
// on the next reload.
func (a *assets) serveDev(w http.ResponseWriter, r *http.Request, name string) {
	body, err := fs.ReadFile(a.web, name)
	if err != nil {
		http.NotFound(w, r)
		return
	}
	w.Header().Set("Content-Type", contentType(name))
	w.Header().Set("Cache-Control", "no-store")
	_, _ = w.Write(body)
}
