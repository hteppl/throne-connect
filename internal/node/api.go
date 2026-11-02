package node

import (
	"context"
	"encoding/json"
	"errors"
	"net"
	"net/http"
	"strconv"
	"strings"
	"time"

	"throne-connect/internal/logbuf"
)

type ServerStatus struct {
	Index   int     `json:"index"`
	Name    string  `json:"name"`
	Group   string  `json:"group"`
	Key     string  `json:"key"`
	Port    int     `json:"port"`
	Active  bool    `json:"active"`
	Healthy bool    `json:"healthy"`
	LastMs  int     `json:"last_ms"`
	Score   float64 `json:"score"`
	Fails   int     `json:"consecutive_fails"`
	OK      int     `json:"ok_total"`
	Errors  int     `json:"err_total"`
	LastErr string  `json:"last_err,omitempty"`
	Conns   int64   `json:"conns"` // total connections opened to this server
}

type Status struct {
	Starting     bool           `json:"starting,omitempty"` // xray is not up or the first scan is running
	Listen       string         `json:"listen"`
	Uptime       string         `json:"uptime"`
	Active       string         `json:"active"`
	ActiveSince  string         `json:"active_since"`
	Pinned       bool           `json:"pinned"`
	Degraded     bool           `json:"degraded"`
	Switches     int            `json:"switches"`
	LastSwitch   string         `json:"last_switch,omitempty"`
	XrayRestarts int64          `json:"xray_restarts"`
	Servers      []ServerStatus `json:"servers"`
}

// Status reports the node state. It never fails; the error is there so Node and
// Client can be used interchangeably.
func (n *Node) Status(context.Context) (Status, error) {
	a := n.active.Load()
	n.mu.Lock()
	defer n.mu.Unlock()
	st := Status{
		Starting:     n.switchedAt.IsZero(),
		Listen:       n.cfg.Listen,
		Uptime:       time.Since(n.startedAt).Round(time.Second).String(),
		Active:       nameOf(a),
		Pinned:       n.pinned,
		Degraded:     n.degraded,
		Switches:     n.failovers,
		LastSwitch:   n.lastSwitch,
		XrayRestarts: n.runner.Restarts.Load(),
	}
	if !n.switchedAt.IsZero() {
		st.ActiveSince = n.switchedAt.Format(time.DateTime)
	}
	for i, b := range n.backends {
		s := ServerStatus{
			Index: i + 1, Name: b.srv.Name, Group: b.srv.Group, Key: b.srv.Key, Port: b.port,
			Active: b == a, Healthy: b.healthy(), LastMs: b.lastMs(), Fails: b.fails,
			OK: b.okTotal, Errors: b.errTotal, Conns: b.dialCount(),
		}
		if sc := b.score(); sc < 1e9 {
			s.Score = sc
		} else {
			s.Score = -1
		}
		if len(b.hist) > 0 && !b.hist[len(b.hist)-1].ok {
			s.LastErr = b.hist[len(b.hist)-1].err
		}
		st.Servers = append(st.Servers, s)
	}
	return st, nil
}

// Switch moves to the given server (see find) and pins it. It returns the name
// of the new active server.
func (n *Node) Switch(ctx context.Context, target string) (string, error) {
	req := switchReq{target: target, reply: make(chan error, 1)}
	select {
	case n.switchCh <- req:
	case <-ctx.Done():
		return "", errors.New("the node is busy, try again")
	}
	if err := <-req.reply; err != nil {
		return "", err
	}
	return nameOf(n.active.Load()), nil
}

// Logs returns log lines newer than after and the value to pass next time.
func (n *Node) Logs(_ context.Context, after uint64) ([]logbuf.Line, uint64, error) {
	if n.logs == nil {
		return nil, after, nil
	}
	lines, next := n.logs.Since(after)
	return lines, next, nil
}

// find resolves a server by 1-based index, key, exact name or unique name substring.
// Callers must hold n.mu.
func (n *Node) find(target string) *backend {
	if i, err := strconv.Atoi(target); err == nil && i >= 1 && i <= len(n.backends) {
		return n.backends[i-1]
	}
	var sub []*backend
	t := strings.ToLower(target)
	for _, b := range n.backends {
		if b.srv.Key == target || b.srv.Name == target {
			return b
		}
		if strings.Contains(strings.ToLower(b.srv.Name), t) {
			sub = append(sub, b)
		}
	}
	if len(sub) == 1 {
		return sub[0]
	}
	return nil
}

func (n *Node) startAPI(ctx context.Context) error {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /status", func(w http.ResponseWriter, r *http.Request) {
		st, _ := n.Status(r.Context())
		writeJSON(w, http.StatusOK, st)
	})
	mux.HandleFunc("GET /logs", func(w http.ResponseWriter, r *http.Request) {
		after, _ := strconv.ParseUint(r.URL.Query().Get("after"), 10, 64)
		lines, next, _ := n.Logs(r.Context(), after)
		writeJSON(w, http.StatusOK, LogsResponse{Lines: lines, Next: next})
	})
	mux.HandleFunc("POST /switch", func(w http.ResponseWriter, r *http.Request) {
		target := r.URL.Query().Get("to")
		if target == "" {
			writeJSON(w, http.StatusBadRequest, map[string]string{"error": "missing ?to="})
			return
		}
		active, err := n.Switch(r.Context(), target)
		if err != nil {
			writeJSON(w, http.StatusConflict, map[string]string{"error": err.Error()})
			return
		}
		writeJSON(w, http.StatusOK, map[string]string{"active": active})
	})

	ln, err := net.Listen("tcp", n.cfg.APIListen)
	if err != nil {
		return err
	}
	srv := &http.Server{Handler: mux, ReadHeaderTimeout: 5 * time.Second}
	go func() { <-ctx.Done(); srv.Close() }()
	go func() {
		if err := srv.Serve(ln); err != nil && !errors.Is(err, http.ErrServerClosed) {
			n.log.Warn("API stopped", "err", err)
		}
	}()
	n.log.Info("API", "listen", n.cfg.APIListen)
	return nil
}

type LogsResponse struct {
	Lines []logbuf.Line `json:"lines"`
	Next  uint64        `json:"next"`
}

func writeJSON(w http.ResponseWriter, code int, v any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(code)
	enc := json.NewEncoder(w)
	enc.SetIndent("", "  ")
	enc.Encode(v)
}
