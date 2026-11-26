package server

import (
	"encoding/json"
	"net"
	"net/http"
	"net/netip"
	"net/url"
	"strings"

	"github.com/rene-roid/kanshi/internal/access"
	"github.com/rene-roid/kanshi/internal/config"
)

// accessState is what the dashboard's network panel shows.
type accessState struct {
	Mode     string        `json:"mode"`
	Source   config.Source `json:"source"`
	Editable bool          `json:"editable"`
	// Reason says why Editable is false, in words the panel can show as is.
	Reason string        `json:"reason,omitempty"`
	Links  []access.Link `json:"links"`
}

func (s *Server) accessState(r *http.Request) accessState {
	s.accessMu.Lock()
	mgr, src := s.access, s.accessSource
	s.accessMu.Unlock()

	st := accessState{Source: src, Links: []access.Link{}}
	if mgr == nil {
		return st
	}
	st.Mode, st.Links = mgr.Mode().String(), mgr.Links()
	switch {
	case src == config.SourceFlag:
		st.Reason = "Set with --access when kanshi was started."
	case src == config.SourceEnv:
		st.Reason = "Set by KANSHI_ACCESS in kanshi's environment."
	case !s.canSave():
		st.Reason = "Kanshi cannot save this setting here. Set KANSHI_ACCESS where it is started — for Docker, in the .env file."
	case !fromLocalhost(r):
		st.Reason = "Open the dashboard on this computer, at localhost, to change this."
	default:
		st.Editable = true
	}
	return st
}

// canSave is checked once, the first time the network panel asks.
func (s *Server) canSave() bool {
	s.saveOnce.Do(func() { s.saveOK = s.cfg.File != "" && config.Writable(s.cfg.File) })
	return s.saveOK
}

func (s *Server) handleAccess(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, r, s.accessState(r))
}

// handleSetAccess changes who can reach the dashboard. Widening that is the
// one thing here that matters for security, so it is only accepted from the
// machine itself, from the dashboard's own page, and only when no flag or
// environment variable would override it on the next start.
func (s *Server) handleSetAccess(w http.ResponseWriter, r *http.Request) {
	if !fromLocalhost(r) || !sameOrigin(r) {
		http.Error(w, "the network setting can only be changed from this computer", http.StatusForbidden)
		return
	}
	var body struct {
		Mode string `json:"mode"`
	}
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 4096)).Decode(&body); err != nil {
		http.Error(w, "bad request", http.StatusBadRequest)
		return
	}
	mode, err := access.Parse(body.Mode)
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}

	s.accessMu.Lock()
	mgr, src := s.access, s.accessSource
	switch {
	case mgr == nil:
		s.accessMu.Unlock()
		http.Error(w, "not listening yet", http.StatusServiceUnavailable)
		return
	case src == config.SourceFlag || src == config.SourceEnv || !s.canSave():
		s.accessMu.Unlock()
		http.Error(w, "the network setting is fixed by how kanshi was started", http.StatusConflict)
		return
	}
	prev := mgr.Mode()
	if err := mgr.Apply(mode); err != nil {
		_ = mgr.Apply(prev)
		s.accessMu.Unlock()
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	saveErr := config.SaveValue(s.cfg.File, "KANSHI_ACCESS", mode.String())
	if saveErr == nil {
		s.accessSource = config.SourceFile
	}
	s.accessMu.Unlock()

	s.logf("access changed to %s from the dashboard", mode)
	st := s.accessState(r)
	if saveErr != nil {
		s.logf("warning: could not save %s: %v", s.cfg.File, saveErr)
		st.Reason = "Applied, but not saved: " + saveErr.Error()
	}
	writeJSON(w, r, st)
}

// fromLocalhost reports a request that arrived over loopback and was
// addressed to a loopback name. The second half matters: a hostile page can
// point its own domain at 127.0.0.1 (DNS rebinding), but then the Host header
// still carries that domain.
func fromLocalhost(r *http.Request) bool {
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		return false
	}
	if ip, err := netip.ParseAddr(host); err != nil || !ip.Unmap().IsLoopback() {
		return false
	}
	name := r.Host
	if h, _, err := net.SplitHostPort(name); err == nil {
		name = h
	}
	name = strings.Trim(name, "[]")
	if strings.EqualFold(name, "localhost") {
		return true
	}
	ip, err := netip.ParseAddr(name)
	return err == nil && ip.Unmap().IsLoopback()
}

// sameOrigin reports a request made by the dashboard's own script. The custom
// header cannot be attached by a form or an <img> on another site, and a
// cross-origin fetch that tries is stopped by the browser's preflight, which
// this server never approves.
func sameOrigin(r *http.Request) bool {
	if r.Header.Get("X-Kanshi") != "1" {
		return false
	}
	if origin := r.Header.Get("Origin"); origin != "" {
		u, err := url.Parse(origin)
		if err != nil || !strings.EqualFold(u.Host, r.Host) {
			return false
		}
	}
	return true
}

// checkHost refuses requests addressed to a name that is not this machine's.
// With no login, a hostile page could otherwise point its own domain at this
// machine's address (DNS rebinding) and read everything the dashboard shows,
// file names included, as if it were the dashboard itself.
func (s *Server) checkHost(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !allowedHost(r.Host, s.hostname, s.cfg.AllowedHosts) {
			http.Error(w, "kanshi does not answer to "+r.Host+". Open it by IP address or by this machine's name, or add the name to KANSHI_ALLOWED_HOSTS.", http.StatusForbidden)
			return
		}
		next.ServeHTTP(w, r)
	})
}

// localSuffixes are the domains a home router or mDNS gives a machine. Nobody
// outside the network can make them resolve anywhere.
var localSuffixes = []string{"", ".local", ".lan", ".home", ".home.arpa", ".internal"}

// allowedHost reports a Host header that names this machine: an IP address,
// localhost, this machine's name on the local network, a Tailscale MagicDNS
// name (ts.net only resolves inside the tailnet), or one listed in extra.
func allowedHost(host, self string, extra []string) bool {
	if h, _, err := net.SplitHostPort(host); err == nil {
		host = h
	}
	host = strings.ToLower(strings.TrimSuffix(strings.Trim(host, "[]"), "."))
	if _, err := netip.ParseAddr(host); err == nil {
		return true
	}
	if host == "localhost" || strings.HasSuffix(host, ".localhost") || strings.HasSuffix(host, ".ts.net") {
		return true
	}
	if self != "" {
		for _, suffix := range localSuffixes {
			if host == self+suffix {
				return true
			}
		}
	}
	for _, name := range extra {
		if strings.EqualFold(host, name) {
			return true
		}
	}
	return false
}
