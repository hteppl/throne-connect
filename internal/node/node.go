// Package node is the failover engine: it health-checks every server, keeps one
// active, and fails over to the best healthy server when the active one stops working.
package node

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"maps"
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
	"throne-connect/internal/tun"
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
	disabled    map[string]bool // keys of servers turned off by the user, saved in state.json

	active atomic.Pointer[backend]
	reqCh  chan request
}

// request is a manual action (switch, enable, disable) run by the Run loop, so it
// never races with health checks and failovers.
type request struct {
	run   func(context.Context) error
	reply chan error
}

type state struct {
	ActiveKey string          `json:"active_key"`
	Servers   []source.Server `json:"servers"`
	Disabled  []string        `json:"disabled,omitempty"`
}

// New creates a node. logs, if not nil, is served by the API for the TUI.
func New(cfg config.Config, runner *xray.Runner, log *slog.Logger, logs *logbuf.Buffer) *Node {
	return &Node{
		cfg: cfg, runner: runner, log: log, logs: logs,
		disabled: map[string]bool{}, reqCh: make(chan request), startedAt: time.Now(),
	}
}

func (n *Node) statePath() string { return filepath.Join(n.cfg.DataDir, "state.json") }

func (n *Node) loadState() state {
	var st state
	if data, err := os.ReadFile(n.statePath()); err == nil {
		json.Unmarshal(data, &st)
	}
	return st
}

// saveState remembers the active server, the server list and the disabled
// servers, so a restart resumes on the same server and still works if
// throne.db is unreadable.
func (n *Node) saveState(servers []source.Server) {
	st := n.loadState()
	if b := n.active.Load(); b != nil {
		st.ActiveKey = b.srv.Key
	}
	if servers != nil {
		st.Servers = servers
	}
	n.mu.Lock()
	st.Disabled = slices.Sorted(maps.Keys(n.disabled))
	n.mu.Unlock()
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
	n.mu.Lock()
	for _, k := range st.Disabled {
		n.disabled[k] = true
	}
	n.mu.Unlock()

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
	// Stop xray before waiting for it, or an early error return would hang.
	defer func() { cancel(); wg.Wait() }()

	if err := n.waitXray(runCtx); err != nil {
		return err
	}

	// Start on the last used server right away so clients work during the first scan.
	n.mu.Lock()
	first := n.firstEnabled()
	for _, b := range n.backends {
		if b.srv.Key == st.ActiveKey && !b.disabled {
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

	if n.cfg.TUN.Enabled {
		tn, err := tun.Start(runCtx, tun.Options{
			Name:      n.cfg.TUN.Name,
			Address:   n.cfg.TUN.Address,
			MTU:       n.cfg.TUN.MTU,
			SocksAddr: socksDialAddr(n.cfg.Listen),
			FWMark:    n.cfg.TUN.FWMark,
			Table:     n.cfg.TUN.Table,
			DNS:       n.cfg.TUN.DNS,
			Exclude:   n.cfg.TUN.Exclude,
			LogLevel:  n.cfg.XrayLogLevel,
		}, n.log)
		if err != nil {
			return fmt.Errorf("tun: %w", err)
		}
		defer tn.Close()
	}

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
		case req := <-n.reqCh:
			req.reply <- req.run(runCtx)
		}
	}
}

func (n *Node) waitXray(ctx context.Context) error {
	n.mu.Lock()
	addr := n.backends[0].addr()
	n.mu.Unlock()
	return xray.WaitReady(ctx, addr, 15*time.Second)
}

// socksDialAddr turns the SOCKS listen address into one the local TUN can dial:
// a wildcard host (0.0.0.0, ::, empty) becomes loopback.
func socksDialAddr(listen string) string {
	host, port, err := net.SplitHostPort(listen)
	if err != nil {
		return listen
	}
	switch host {
	case "", "0.0.0.0", "::":
		host = "127.0.0.1"
	}
	return net.JoinHostPort(host, port)
}

// apply installs a new server list: builds and validates the xray config, then
// swaps in fresh backends, carrying over health history by server key.
func (n *Node) apply(servers []source.Server, initial bool) error {
	mark := 0
	if n.cfg.TUN.Enabled {
		mark = n.cfg.TUN.FWMark
	}
	build := func(s []source.Server) ([]byte, error) {
		return xray.BuildConfig(s, n.cfg.BasePort, n.cfg.DNS, n.cfg.XrayLogLevel, mark)
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
		b := &backend{srv: s, port: n.cfg.BasePort + i, conns: map[net.Conn]net.Conn{}, disabled: n.disabled[s.Key]}
		if o := old[s.Key]; o != nil {
			b.hist, b.fails, b.lastOK, b.okTotal, b.errTotal = o.hist, o.fails, o.lastOK, o.okTotal, o.errTotal
			b.dials = o.dialCount()
		}
		if s.Key == activeKey {
			newActive = b
		}
		n.backends[i] = b
	}
	if n.firstEnabled().disabled {
		// The new list holds only disabled servers. Use them anyway rather than
		// leave Throne without a proxy; the saved choice is kept for later lists.
		n.log.Warn("every server is disabled, using them anyway", "servers", len(n.backends))
		for _, b := range n.backends {
			b.disabled = false
		}
	}
	n.fingerprint = source.Fingerprint(servers)
	if !initial {
		if newActive == nil || newActive.disabled {
			newActive = n.firstEnabled()
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

// firstEnabled returns the first server that is not disabled, or the first one
// if all are. Callers must hold n.mu, and there must be at least one server.
func (n *Node) firstEnabled() *backend {
	for _, b := range n.backends {
		if !b.disabled {
			return b
		}
	}
	return n.backends[0]
}

// scan checks every enabled server in parallel.
func (n *Node) scan(ctx context.Context) {
	n.mu.Lock()
	var list []*backend
	for _, b := range n.backends {
		if !b.disabled {
			list = append(list, b)
		}
	}
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

// candidates returns enabled servers whose last check passed, best first.
// Callers must hold n.mu.
func (n *Node) candidates(exclude *backend) []*backend {
	var out []*backend
	for _, b := range n.backends {
		if b != exclude && !b.disabled && b.healthy() {
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

// pickHealthy finds the best working server other than `from`. Candidates are
// re-checked first so we never jump to a server that died since the last scan;
// if none of the top three passes, every server is scanned again. It returns
// nil if nothing works.
func (n *Node) pickHealthy(ctx context.Context, from *backend) *backend {
	n.mu.Lock()
	cands := n.candidates(from)
	n.mu.Unlock()
	for i, c := range cands {
		if i == 3 {
			break
		}
		if n.probe(ctx, c).ok {
			return c
		}
	}
	n.scan(ctx)
	n.mu.Lock()
	defer n.mu.Unlock()
	if cands = n.candidates(from); len(cands) > 0 {
		return cands[0]
	}
	return nil
}

// failover moves off `from` to the best working server.
func (n *Node) failover(ctx context.Context, from *backend, reason string) {
	if c := n.pickHealthy(ctx, from); c != nil {
		n.switchTo(c, "failure: "+reason, true)
		return
	}
	n.mu.Lock()
	recovered := from.healthy()
	wasDegraded := n.degraded
	n.mu.Unlock()
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
	n.mu.Lock()
	off := b.disabled
	n.mu.Unlock()
	if off {
		return fmt.Errorf("%s is disabled, enable it first", b.srv.Name)
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

// setDisabled turns a server off or back on. A disabled server is not checked
// and never used. Disabling the active server first moves to the best working
// one; if nothing else works, the server stays enabled. Enabling checks the
// server right away. It returns the server's name.
func (n *Node) setDisabled(ctx context.Context, target string, off bool) (string, error) {
	n.mu.Lock()
	b := n.find(target)
	if b == nil {
		n.mu.Unlock()
		return "", fmt.Errorf("server %q not found", target)
	}
	name := b.srv.Name
	if b.disabled == off {
		n.mu.Unlock()
		return name, nil
	}
	if !off {
		b.disabled = false
		delete(n.disabled, b.srv.Key)
		n.mu.Unlock()
		r := n.probe(ctx, b)
		n.log.Info("server enabled", "server", name, "ok", r.ok)
		n.saveState(nil)
		return name, nil
	}

	enabled := 0
	for _, x := range n.backends {
		if !x.disabled {
			enabled++
		}
	}
	if enabled <= 1 {
		n.mu.Unlock()
		return "", fmt.Errorf("%s is the only enabled server", name)
	}
	b.disabled = true
	n.disabled[b.srv.Key] = true
	n.mu.Unlock()

	if n.active.Load() == b {
		next := n.pickHealthy(ctx, b)
		if next == nil {
			n.mu.Lock()
			b.disabled = false
			delete(n.disabled, b.srv.Key)
			n.mu.Unlock()
			return "", fmt.Errorf("%s is active and no other server responds, so it stays enabled", name)
		}
		n.switchTo(next, "disabled "+name, true)
	}
	n.log.Info("server disabled", "server", name)
	n.saveState(nil)
	return name, nil
}

func (n *Node) logSummary() {
	n.mu.Lock()
	defer n.mu.Unlock()
	alive, off := 0, 0
	for _, b := range n.backends {
		switch {
		case b.disabled:
			off++
		case b.healthy():
			alive++
		}
	}
	args := []any{"alive", alive, "total", len(n.backends), "active", nameOf(n.active.Load())}
	if off > 0 {
		args = append(args, "disabled", off)
	}
	n.log.Info("server scan", args...)
}

func nameOf(b *backend) string {
	if b == nil {
		return "-"
	}
	return b.srv.Name
}
