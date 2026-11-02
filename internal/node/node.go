// Package node is the failover engine: it health-checks every server, keeps one
// active, and fails over to the best healthy server when the active one stops working.
package node

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"os"
	"path/filepath"
	"slices"
	"sync"
	"sync/atomic"
	"time"

	"throne-connect/internal/config"
	"throne-connect/internal/logbuf"
	"throne-connect/internal/source"
	"throne-connect/internal/xray"
)

type Node struct {
	cfg    config.Config
	log    *slog.Logger
	logs   *logbuf.Buffer // may be nil
	runner *xray.Runner

	mu          sync.Mutex
	backends    []*backend
	fingerprint string
	switchedAt  time.Time
	pinned      bool // manual choice: no "faster server" switches until it fails
	degraded    bool // nothing works, most likely no internet
	failovers   int
	lastSwitch  string
	startedAt   time.Time

	active   atomic.Pointer[backend]
	switchCh chan switchReq
}

type switchReq struct {
	target string
	reply  chan error
}

type state struct {
	ActiveKey string          `json:"active_key"`
	Servers   []source.Server `json:"servers"`
}

// New creates a node. logs, if not nil, is served by the API for the TUI.
func New(cfg config.Config, runner *xray.Runner, log *slog.Logger, logs *logbuf.Buffer) *Node {
	return &Node{cfg: cfg, runner: runner, log: log, logs: logs, switchCh: make(chan switchReq), startedAt: time.Now()}
}

func (n *Node) statePath() string { return filepath.Join(n.cfg.DataDir, "state.json") }

func (n *Node) loadState() state {
	var st state
	if data, err := os.ReadFile(n.statePath()); err == nil {
		json.Unmarshal(data, &st)
	}
	return st
}

// saveState remembers the active server and the server list, so a restart
// resumes on the same server and still works if throne.db is unreadable.
func (n *Node) saveState(servers []source.Server) {
	st := n.loadState()
	if b := n.active.Load(); b != nil {
		st.ActiveKey = b.srv.Key
	}
	if servers != nil {
		st.Servers = servers
	}
	data, _ := json.MarshalIndent(st, "", "  ")
	tmp := n.statePath() + ".new"
	if os.WriteFile(tmp, data, 0o600) == nil {
		os.Rename(tmp, n.statePath())
	}
}

func (n *Node) loadServers() ([]source.Server, error) {
	return source.LoadThrone(n.cfg.ThroneDB, n.cfg.Groups, n.cfg.Include, n.cfg.Exclude)
}

// Run blocks until ctx is cancelled.
func (n *Node) Run(ctx context.Context) error {
	if err := os.MkdirAll(n.cfg.DataDir, 0o700); err != nil {
		return err
	}
	st := n.loadState()

	servers, err := n.loadServers()
	if err != nil || len(servers) == 0 {
		if len(st.Servers) == 0 {
			if err == nil {
				err = errors.New("no xray profiles matched (check groups/include/exclude)")
			}
			return fmt.Errorf("loading servers from Throne: %w", err)
		}
		n.log.Warn("throne.db unavailable, using the saved server list", "err", err, "servers", len(st.Servers))
		servers = st.Servers
	}
	if err := n.apply(servers, true); err != nil {
		return err
	}

	runCtx, cancel := context.WithCancel(ctx)
	defer cancel()
	var wg sync.WaitGroup
	wg.Add(1)
	go func() { defer wg.Done(); n.runner.Run(runCtx) }()
	defer wg.Wait()

	if err := n.waitXray(runCtx); err != nil {
		return err
	}

	// Start on the last used server right away so clients work during the first scan.
	n.mu.Lock()
	first := n.backends[0]
	for _, b := range n.backends {
		if b.srv.Key == st.ActiveKey {
			first = b
		}
	}
	n.mu.Unlock()
	n.active.Store(first)

	ln, err := net.Listen("tcp", n.cfg.Listen)
	if err != nil {
		return fmt.Errorf("listen %s: %w", n.cfg.Listen, err)
	}
	go func() { <-runCtx.Done(); ln.Close() }()
	go n.serve(runCtx, ln)
	n.log.Info("node started", "listen", n.cfg.Listen, "servers", len(n.backends))

	if n.cfg.APIListen != "" {
		if err := n.startAPI(runCtx); err != nil {
			n.log.Warn("API not started", "err", err)
		}
	}

	n.scan(runCtx)
	n.mu.Lock()
	if !first.healthy() {
		n.mu.Unlock()
		n.failover(runCtx, first, "initial server not responding")
	} else {
		n.switchedAt = time.Now()
		n.mu.Unlock()
		n.log.Info("active server", "server", first.srv.Name, "ms", first.lastMs())
		n.saveState(servers)
	}
	n.logSummary()

	check := time.NewTicker(n.cfg.CheckInterval.Duration)
	scan := time.NewTicker(n.cfg.ScanInterval.Duration)
	reload := time.NewTicker(n.cfg.ReloadInterval.Duration)
	defer check.Stop()
	defer scan.Stop()
	defer reload.Stop()

	for {
		select {
		case <-ctx.Done():
			n.log.Info("shutting down")
			return nil
		case <-check.C:
			n.checkActive(runCtx)
		case <-scan.C:
			n.scan(runCtx)
			n.rebalance()
		case <-reload.C:
			n.reload(runCtx)
		case req := <-n.switchCh:
			req.reply <- n.manualSwitch(runCtx, req.target)
		}
	}
}

func (n *Node) waitXray(ctx context.Context) error {
	n.mu.Lock()
	addr := n.backends[0].addr()
	n.mu.Unlock()
	return xray.WaitReady(ctx, addr, 15*time.Second)
}

// apply installs a new server list: builds and validates the xray config, then
// swaps in fresh backends, carrying over health history by server key.
func (n *Node) apply(servers []source.Server, initial bool) error {
	build := func(s []source.Server) ([]byte, error) {
		return xray.BuildConfig(s, n.cfg.BasePort, n.cfg.DNS, n.cfg.XrayLogLevel)
	}
	cfg, err := build(servers)
	if err == nil {
		err = n.runner.Apply(cfg)
	}
	if err != nil {
		n.log.Warn("xray config failed validation, testing servers one by one", "err", err)
		servers = n.runner.FilterValid(servers, build)
		if len(servers) == 0 {
			return errors.New("xray accepted none of the servers")
		}
		if cfg, err = build(servers); err == nil {
			err = n.runner.Apply(cfg)
		}
		if err != nil {
			return err
		}
	}

	n.mu.Lock()
	defer n.mu.Unlock()
	old := map[string]*backend{}
	for _, b := range n.backends {
		old[b.srv.Key] = b
	}
	var activeKey string
	if a := n.active.Load(); a != nil {
		activeKey = a.srv.Key
	}
	n.backends = make([]*backend, len(servers))
	var newActive *backend
	for i, s := range servers {
		b := &backend{srv: s, port: n.cfg.BasePort + i, conns: map[net.Conn]net.Conn{}}
		if o := old[s.Key]; o != nil {
			b.hist, b.fails, b.lastOK, b.okTotal, b.errTotal = o.hist, o.fails, o.lastOK, o.okTotal, o.errTotal
			b.dials = o.dialCount()
		}
		if s.Key == activeKey {
			newActive = b
		}
		n.backends[i] = b
	}
	n.fingerprint = source.Fingerprint(servers)
	if !initial {
		if newActive == nil {
			newActive = n.backends[0]
			n.pinned = false
		}
		n.active.Store(newActive)
	}
	return nil
}

func (n *Node) reload(ctx context.Context) {
	servers, err := n.loadServers()
	if err != nil {
		n.log.Debug("could not read throne.db, keeping the current list", "err", err)
		return
	}
	if len(servers) == 0 {
		n.log.Warn("no matching profiles left in throne.db, keeping the current list")
		return
	}
	n.mu.Lock()
	same := source.Fingerprint(servers) == n.fingerprint
	n.mu.Unlock()
	if same {
		return
	}
	n.log.Info("server list in Throne changed, reloading xray", "servers", len(servers))
	if err := n.apply(servers, false); err != nil {
		n.log.Error("new list not applied, staying on the old one", "err", err)
		return
	}
	if err := n.waitXray(ctx); err != nil {
		n.log.Error("xray did not come up after reload", "err", err)
	}
	n.saveState(servers)
	n.scan(ctx)
	n.mu.Lock()
	a := n.active.Load()
	ok := a.healthy()
	n.mu.Unlock()
	if !ok {
		n.failover(ctx, a, "after server list update")
	}
}

func (n *Node) probe(ctx context.Context, b *backend) result {
	r := probe(ctx, b.port, n.cfg.TestURL, n.cfg.Timeout.Duration)
	n.mu.Lock()
	b.record(r)
	n.mu.Unlock()
	return r
}

// scan checks every server in parallel.
func (n *Node) scan(ctx context.Context) {
	n.mu.Lock()
	list := slices.Clone(n.backends)
	n.mu.Unlock()
	sem := make(chan struct{}, 8)
	var wg sync.WaitGroup
	for _, b := range list {
		wg.Add(1)
		sem <- struct{}{}
		go func() {
			defer wg.Done()
			defer func() { <-sem }()
			n.probe(ctx, b)
		}()
	}
	wg.Wait()
}

func (n *Node) checkActive(ctx context.Context) {
	a := n.active.Load()
	r := n.probe(ctx, a)
	n.mu.Lock()
	fails := a.fails
	degraded := n.degraded
	n.mu.Unlock()

	if r.ok {
		if degraded {
			n.mu.Lock()
			n.degraded = false
			n.mu.Unlock()
			n.log.Info("connectivity restored", "server", a.srv.Name, "ms", r.ms)
		}
		return
	}
	if !degraded {
		n.log.Warn("active server check failed", "server", a.srv.Name, "fail", fmt.Sprintf("%d/%d", fails, n.cfg.FailThreshold), "err", r.err)
	}
	if fails >= n.cfg.FailThreshold {
		n.failover(ctx, a, r.err)
	}
}

// candidates returns servers whose last check passed, best first.
// Callers must hold n.mu.
func (n *Node) candidates(exclude *backend) []*backend {
	var out []*backend
	for _, b := range n.backends {
		if b != exclude && b.healthy() {
			out = append(out, b)
		}
	}
	slices.SortStableFunc(out, func(x, y *backend) int {
		sx, sy := x.score(), y.score()
		switch {
		case sx < sy:
			return -1
		case sx > sy:
			return 1
		}
		return 0
	})
	return out
}

// failover moves off `from`. Candidates are re-checked right before switching so
// we never jump to a server that died since the last scan.
func (n *Node) failover(ctx context.Context, from *backend, reason string) {
	n.mu.Lock()
	cands := n.candidates(from)
	n.mu.Unlock()
	for i, c := range cands {
		if i == 3 {
			break
		}
		if n.probe(ctx, c).ok {
			n.switchTo(c, "failure: "+reason, true)
			return
		}
	}

	n.scan(ctx)
	n.mu.Lock()
	cands = n.candidates(from)
	recovered := from.healthy()
	wasDegraded := n.degraded
	n.mu.Unlock()
	if len(cands) > 0 {
		n.switchTo(cands[0], "failure: "+reason, true)
		return
	}
	if recovered {
		return
	}
	if !wasDegraded {
		n.mu.Lock()
		n.degraded = true
		n.mu.Unlock()
		n.log.Error("no server responds, probably no internet; still checking", "servers", len(n.backends))
	}
}

func (n *Node) switchTo(b *backend, reason string, dropOld bool) {
	old := n.active.Swap(b)
	n.mu.Lock()
	n.switchedAt = time.Now()
	n.pinned = false
	n.degraded = false
	n.failovers++
	n.lastSwitch = fmt.Sprintf("%s: %s → %s (%s)", time.Now().Format("15:04:05"), nameOf(old), b.srv.Name, reason)
	ms := b.lastMs()
	n.mu.Unlock()

	dropped := 0
	if dropOld && old != nil && old != b && n.cfg.DropOnFailover {
		dropped = old.closeAll()
	}
	n.log.Info("switched", "from", nameOf(old), "to", b.srv.Name, "ms", ms, "reason", reason, "dropped_conns", dropped)
	n.saveState(nil)
}

// rebalance moves to a clearly faster server, if enabled. A working server is
// otherwise kept, because switching itself costs stability.
func (n *Node) rebalance() {
	if n.cfg.SwitchBetterMs <= 0 {
		return
	}
	a := n.active.Load()
	n.mu.Lock()
	if n.pinned || time.Since(n.switchedAt) < n.cfg.MinDwell.Duration || !a.healthy() {
		n.mu.Unlock()
		return
	}
	cands := n.candidates(a)
	var best *backend
	if len(cands) > 0 && cands[0].score()+float64(n.cfg.SwitchBetterMs) < a.score() {
		best = cands[0]
	}
	gain := 0.0
	if best != nil {
		gain = a.score() - best.score()
	}
	n.mu.Unlock()
	if best != nil {
		n.switchTo(best, fmt.Sprintf("faster by %.0f ms", gain), false)
	}
}

func (n *Node) manualSwitch(ctx context.Context, target string) error {
	n.mu.Lock()
	b := n.find(target)
	n.mu.Unlock()
	if b == nil {
		return fmt.Errorf("server %q not found", target)
	}
	if r := n.probe(ctx, b); !r.ok {
		return fmt.Errorf("%s is not responding: %s", b.srv.Name, r.err)
	}
	n.switchTo(b, "manual", false)
	n.mu.Lock()
	n.pinned = true
	n.mu.Unlock()
	return nil
}

func (n *Node) logSummary() {
	n.mu.Lock()
	defer n.mu.Unlock()
	alive := 0
	for _, b := range n.backends {
		if b.healthy() {
			alive++
		}
	}
	n.log.Info("server scan", "alive", alive, "total", len(n.backends), "active", nameOf(n.active.Load()))
}

func nameOf(b *backend) string {
	if b == nil {
		return "-"
	}
	return b.srv.Name
}
