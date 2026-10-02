package server

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"strconv"
	"time"

	"github.com/rene-roid/kanshi/internal/config"
	"github.com/rene-roid/kanshi/internal/dockerstats"
	"github.com/rene-roid/kanshi/internal/history"
	"github.com/rene-roid/kanshi/internal/vitals"
)

// What the page offers to keep when history has never been turned on.
const historyDefaultDays = 7

// The longest the page may ask to keep. The timeline's longest range is a
// week; this is only a bound on what a request can make kanshi store.
const historyMaxDays = 366

func defaultHistoryDays(cfg config.Config) float64 {
	if cfg.HistoryRetention > 0 {
		return cfg.HistoryRetention.Hours() / 24
	}
	return historyDefaultDays
}

func days(d float64) time.Duration { return time.Duration(d * float64(24*time.Hour)) }

/* ── recording ──────────────────────────────────────────────────────────── */

// startHistory opens the store and starts recording into it. Called with
// historyMu held.
func (s *Server) startHistory(d float64) error {
	if s.cfg.HistoryDir == "" {
		return errors.New("no folder to keep it in; set KANSHI_HISTORY_DIR")
	}
	h, err := history.Open(s.cfg.HistoryDir, days(d), s.logf)
	if err != nil {
		return err
	}
	ctx, cancel := context.WithCancel(s.base)
	done := make(chan struct{})
	go func() {
		defer close(done)
		s.record(ctx, h)
	}()
	s.history, s.historyDays = h, d
	s.historyStop = func() { cancel(); <-done }
	return nil
}

// stopHistory stops recording and closes the store. What was recorded stays
// on disk, and is there again if history is turned back on before it ages
// out. Called with historyMu held.
func (s *Server) stopHistory() {
	if s.historyStop != nil {
		s.historyStop()
	}
	s.history, s.historyStop = nil, nil
}

func (s *Server) historyStore() *history.Store {
	s.historyMu.Lock()
	defer s.historyMu.Unlock()
	return s.history
}

// record writes one history entry every HistoryInterval, watched or not. While
// a browser keeps the poller busy it reuses the poller's latest frame; once
// the poller has gone idle it samples on its own. Every rate is a delta
// against the previous sample, so an idle-time record averages the whole
// interval rather than the last five seconds.
func (s *Server) record(ctx context.Context, h *history.Store) {
	defer h.Close()
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
		if err := h.Add(at, frame.Vitals.CPU.Percent, frame.Vitals.Memory.Percent, historyFrame(frame)); err != nil {
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

/* ── handlers ───────────────────────────────────────────────────────────── */

// historyState is what the page's history panel shows, plus the timeline
// itself while history is on.
type historyState struct {
	Enabled bool    `json:"enabled"`
	Days    float64 `json:"days"`
	Every   float64 `json:"every"` // seconds between records
	// Editable and Reason follow the same rules as the network setting.
	Editable bool   `json:"editable"`
	Reason   string `json:"reason,omitempty"`

	// The timeline, only while history is on. Interval is the spacing records
	// can actually have: while the poller runs, records are its frames, so
	// they come no closer together than a poll. The page reads a longer wait
	// than this as a gap.
	Interval  float64         `json:"interval,omitempty"`
	Retention float64         `json:"retention,omitempty"`
	First     int64           `json:"first,omitempty"`
	Last      int64           `json:"last,omitempty"`
	Points    []history.Point `json:"points"`
}

func (s *Server) historyState(r *http.Request) (historyState, *history.Store) {
	s.historyMu.Lock()
	h, d, src := s.history, s.historyDays, s.historySrc
	s.historyMu.Unlock()

	st := historyState{Enabled: h != nil, Days: d, Every: s.cfg.HistoryInterval.Seconds()}
	switch {
	case src == config.SourceEnv:
		st.Reason = "Set by KANSHI_HISTORY_DAYS in kanshi's environment."
	case !s.canSave():
		st.Reason = "Kanshi cannot save this setting here. Set KANSHI_HISTORY_DAYS where it is started — for Docker, in the .env file."
	case !fromLocalhost(r):
		st.Reason = "Open the dashboard on this computer, at localhost, to change this."
	default:
		st.Editable = true
	}
	return st, h
}

// handleHistory is the panel's state, and while history is on the timeline:
// /api/history?from=…&to=…&n=… in Unix seconds, at most n points (default
// 600) with the busiest kept per bucket.
func (s *Server) handleHistory(w http.ResponseWriter, r *http.Request) {
	st, h := s.historyState(r)
	if h == nil {
		writeJSON(w, r, st)
		return
	}
	q := r.URL.Query()
	to := queryInt(q.Get("to"), time.Now().Unix())
	from := queryInt(q.Get("from"), to-3600)
	n := int(min(max(queryInt(q.Get("n"), 600), 10), 2000))
	st.Interval = max(s.cfg.HistoryInterval, s.cfg.PollInterval).Seconds()
	st.Retention = days(st.Days).Seconds()
	st.First, st.Last = h.Span()
	st.Points = h.Series(from, to, n)
	if st.Points == nil {
		st.Points = []history.Point{}
	}
	writeJSON(w, r, st)
}

// handleSetHistory turns history on or off: {"days": 7}, or 0 for off. It
// makes kanshi sample around the clock and write to disk, so it takes the
// same care as the network setting: only from this computer, only from the
// dashboard's own page, and only when the environment does not pin it.
func (s *Server) handleSetHistory(w http.ResponseWriter, r *http.Request) {
	if !fromLocalhost(r) || !sameOrigin(r) {
		http.Error(w, "history can only be turned on or off from this computer", http.StatusForbidden)
		return
	}
	var body struct {
		Days float64 `json:"days"`
	}
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 4096)).Decode(&body); err != nil {
		http.Error(w, "bad request", http.StatusBadRequest)
		return
	}
	if body.Days < 0 || body.Days > historyMaxDays {
		http.Error(w, "days must be between 0 and "+strconv.Itoa(historyMaxDays), http.StatusBadRequest)
		return
	}

	s.historyMu.Lock()
	if s.historySrc == config.SourceEnv || !s.canSave() {
		s.historyMu.Unlock()
		http.Error(w, "the history setting is fixed by how kanshi was started", http.StatusConflict)
		return
	}
	// A new retention means a new store: stop the old one first, so there is
	// never more than one writer in the folder.
	s.stopHistory()
	if body.Days > 0 {
		if err := s.startHistory(body.Days); err != nil {
			s.historyMu.Unlock()
			http.Error(w, "could not turn history on: "+err.Error(), http.StatusInternalServerError)
			return
		}
	}
	saveErr := config.SaveValue(s.cfg.File, "KANSHI_HISTORY_DAYS", strconv.FormatFloat(body.Days, 'g', -1, 64))
	if saveErr == nil {
		s.historySrc = config.SourceFile
	}
	s.historyMu.Unlock()

	if body.Days > 0 {
		s.logf("history turned on from the dashboard, keeping %g days", body.Days)
	} else {
		s.logf("history turned off from the dashboard")
	}
	st, _ := s.historyState(r)
	if saveErr != nil {
		s.logf("warning: could not save %s: %v", s.cfg.File, saveErr)
		st.Reason = "Applied, but not saved: " + saveErr.Error()
	}
	writeJSON(w, r, st)
}

// handleHistoryAt returns one recorded frame: /api/history/at?t=…&dir=…,
// where dir is 0 for the record nearest t, -1 for the one before and 1 for
// the one after.
func (s *Server) handleHistoryAt(w http.ResponseWriter, r *http.Request) {
	h := s.historyStore()
	if h == nil {
		http.Error(w, "history is off", http.StatusNotFound)
		return
	}
	q := r.URL.Query()
	t, err := strconv.ParseInt(q.Get("t"), 10, 64)
	if err != nil {
		http.Error(w, "bad t", http.StatusBadRequest)
		return
	}
	p, frame, ok := h.At(t, int(queryInt(q.Get("dir"), 0)))
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
