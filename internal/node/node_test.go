package node

import (
	"context"
	"encoding/json"
	"errors"
	"net"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"throne-connect/internal/config"
	"throne-connect/internal/source"
	"throne-connect/internal/xray"
)

func activeName(n *Node) string { return nameOf(n.active.Load()) }

func TestCheckActive(t *testing.T) {
	target := startTarget(t)
	s0, s1 := startSocks(t), startSocks(t)
	n, buf := unitNode(t, baseConfig(t, target.URL), s0, s1)
	ctx := ctxT(t)
	n.backends[1].record(okResult(10))

	n.checkActive(ctx)
	if activeName(n) != "srv0" || hasLog(buf, "check failed") {
		t.Fatal("a working active server must be kept quietly")
	}

	n.degraded = true
	n.checkActive(ctx)
	if n.degraded || !hasLog(buf, "connectivity restored") {
		t.Fatal("connectivity not restored")
	}

	s0.fail.Store(true)
	n.checkActive(ctx)
	if activeName(n) != "srv0" || !hasLog(buf, "fail=1/2") {
		t.Fatal("one failure is below the threshold")
	}
	n.checkActive(ctx)
	if activeName(n) != "srv1" || !hasLog(buf, "reason=\"failure:") {
		t.Fatalf("expected a failover to srv1, active %s", activeName(n))
	}
}

func TestCheckActiveDegradedIsQuiet(t *testing.T) {
	target := startTarget(t)
	s0 := startSocks(t)
	s0.fail.Store(true)
	n, buf := unitNode(t, baseConfig(t, target.URL), s0)
	n.degraded = true
	n.checkActive(ctxT(t))
	if hasLog(buf, "check failed") {
		t.Fatal("repeated failures while degraded must not be logged")
	}
}

func TestFailoverRechecksCandidates(t *testing.T) {
	target := startTarget(t)
	s0, s1, s2 := startSocks(t), startSocks(t), startSocks(t)
	n, _ := unitNode(t, baseConfig(t, target.URL), s0, s1, s2)
	n.backends[1].record(okResult(10))
	n.backends[2].record(okResult(90))
	s1.fail.Store(true)

	n.failover(ctxT(t), n.backends[0], "test")
	if activeName(n) != "srv2" {
		t.Fatalf("active %s, want srv2: srv1 died since the last scan", activeName(n))
	}
	if n.failovers != 1 || !strings.Contains(n.lastSwitch, "srv0 → srv2 (failure: test)") {
		t.Fatalf("failovers %d, last %q", n.failovers, n.lastSwitch)
	}
}

func TestFailoverRechecksAtMostThree(t *testing.T) {
	target := startTarget(t)
	var ss []*socksServer
	for range 5 {
		ss = append(ss, startSocks(t))
	}
	n, _ := unitNode(t, baseConfig(t, target.URL), ss...)
	for i := 1; i <= 4; i++ {
		n.backends[i].record(okResult(10 * i))
	}
	for _, s := range ss[:4] {
		s.fail.Store(true)
	}
	n.failover(ctxT(t), n.backends[0], "test")
	if activeName(n) != "srv4" {
		t.Fatalf("active %s", activeName(n))
	}
	if n.backends[4].okTotal != 2 {
		t.Fatalf("srv4 must only be checked by the full scan, ok_total %d", n.backends[4].okTotal)
	}
	if n.backends[1].errTotal != 2 {
		t.Fatalf("srv1 checked %d times, want recheck + scan", n.backends[1].errTotal)
	}
}

func TestFailoverNothingWorks(t *testing.T) {
	target := startTarget(t)
	s0, s1 := startSocks(t), startSocks(t)
	s0.fail.Store(true)
	s1.fail.Store(true)
	n, buf := unitNode(t, baseConfig(t, target.URL), s0, s1)
	n.failover(ctxT(t), n.backends[0], "test")
	n.failover(ctxT(t), n.backends[0], "test")
	if !n.degraded || activeName(n) != "srv0" {
		t.Fatalf("degraded %v active %s", n.degraded, activeName(n))
	}
	if c := countLog(buf, "no server responds"); c != 1 {
		t.Fatalf("degraded logged %d times", c)
	}
}

func TestFailoverActiveRecovered(t *testing.T) {
	target := startTarget(t)
	s0, s1 := startSocks(t), startSocks(t)
	s1.fail.Store(true)
	n, buf := unitNode(t, baseConfig(t, target.URL), s0, s1)
	n.failover(ctxT(t), n.backends[0], "test")
	if n.degraded || activeName(n) != "srv0" || hasLog(buf, "switched") {
		t.Fatal("a recovered active server must be kept")
	}
}

func trackPair(b *backend) {
	c, _ := net.Pipe()
	up, _ := net.Pipe()
	b.track(c, up)
}

func TestSwitchToDropsOldConnections(t *testing.T) {
	cfg := baseConfig(t, "")
	n, buf := unitNode(t, cfg, startSocks(t), startSocks(t))
	a, b := n.backends[0], n.backends[1]
	trackPair(a)

	n.switchTo(b, "keep", false)
	if a.connCount() != 1 {
		t.Fatal("connections dropped without dropOld")
	}
	n.switchTo(a, "back", true)
	if b.connCount() != 0 {
		t.Fatal("unexpected conns on b")
	}
	n.switchTo(b, "drop", true)
	if !hasLog(buf, "dropped_conns=1") {
		t.Fatal("old connections not dropped")
	}
	if n.pinned || n.degraded || n.failovers != 3 || n.switchedAt.IsZero() {
		t.Fatalf("state after switch: pinned %v degraded %v failovers %d", n.pinned, n.degraded, n.failovers)
	}
	if st := n.loadState(); st.ActiveKey != "key-srv1" {
		t.Fatalf("saved active key %q", st.ActiveKey)
	}
}

func TestSwitchToKeepsConnectionsWhenDisabled(t *testing.T) {
	cfg := baseConfig(t, "")
	cfg.DropOnFailover = false
	n, _ := unitNode(t, cfg, startSocks(t), startSocks(t))
	trackPair(n.backends[0])
	n.switchTo(n.backends[1], "x", true)
	if n.backends[0].connCount() != 1 {
		t.Fatal("drop_on_failover=false must keep connections")
	}
}

func TestRebalance(t *testing.T) {
	cfg := baseConfig(t, "")
	n, buf := unitNode(t, cfg, startSocks(t), startSocks(t))
	a, b := n.backends[0], n.backends[1]
	a.record(okResult(300))
	b.record(okResult(100))

	n.rebalance()
	if activeName(n) != "srv0" {
		t.Fatal("rebalance is disabled when switch_better_ms is 0")
	}

	n.cfg.SwitchBetterMs = 50
	n.cfg.MinDwell = config.Duration{Duration: time.Hour}
	n.switchedAt = time.Now()
	n.rebalance()
	if activeName(n) != "srv0" {
		t.Fatal("switched before min_dwell")
	}

	n.cfg.MinDwell = config.Duration{}
	n.pinned = true
	n.rebalance()
	if activeName(n) != "srv0" {
		t.Fatal("switched away from a pinned server")
	}
	n.pinned = false

	a.record(failResult())
	n.rebalance()
	if activeName(n) != "srv0" {
		t.Fatal("rebalance must leave an unhealthy active server to failover")
	}
	a.hist = nil
	a.record(okResult(300))

	n.cfg.SwitchBetterMs = 500
	n.rebalance()
	if activeName(n) != "srv0" {
		t.Fatal("switched for a gain below switch_better_ms")
	}

	n.cfg.SwitchBetterMs = 50
	n.rebalance()
	if activeName(n) != "srv1" || !hasLog(buf, "faster by 200 ms") {
		t.Fatalf("active %s", activeName(n))
	}
}

func TestRebalanceNoCandidates(t *testing.T) {
	cfg := baseConfig(t, "")
	cfg.SwitchBetterMs = 10
	n, _ := unitNode(t, cfg, startSocks(t), startSocks(t))
	n.backends[0].record(okResult(300))
	n.rebalance()
	if activeName(n) != "srv0" {
		t.Fatal("no healthy alternative")
	}
}

func TestState(t *testing.T) {
	n, _ := unitNode(t, baseConfig(t, ""))
	if st := n.loadState(); st.ActiveKey != "" || st.Servers != nil {
		t.Fatalf("empty state: %+v", st)
	}
	servers := []source.Server{testServer("a"), testServer("b")}
	n.saveState(servers)
	n.active.Store(&backend{srv: servers[1]})
	n.saveState(nil)
	st := n.loadState()
	if st.ActiveKey != "key-b" || len(st.Servers) != 2 || st.Servers[0].Name != "a" {
		t.Fatalf("state: %+v", st)
	}

	os.WriteFile(n.statePath(), []byte("{not json"), 0o600)
	if st := n.loadState(); st.ActiveKey != "" {
		t.Fatalf("corrupt state: %+v", st)
	}

	n.cfg.DataDir = filepath.Join(t.TempDir(), "missing")
	n.saveState(servers) // must not panic or create the directory
	if _, err := os.Stat(n.cfg.DataDir); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("saveState created the data dir")
	}
}

func applyNode(t *testing.T) *Node {
	cfg := baseConfig(t, "")
	cfg.BasePort = 31000
	log, buf := newLogs()
	n := New(cfg, xray.NewRunner(fakeXrayBin, cfg.DataDir, log), log, buf)
	return n
}

func names(n *Node) []string {
	var out []string
	for _, b := range n.backends {
		out = append(out, b.srv.Name)
	}
	return out
}

func TestApply(t *testing.T) {
	n := applyNode(t)
	a, b, c := testServer("a"), testServer("b"), testServer("c")
	if err := n.apply([]source.Server{a, b}, true); err != nil {
		t.Fatal(err)
	}
	if n.active.Load() != nil {
		t.Fatal("the initial apply leaves choosing the active server to Run")
	}
	if n.backends[1].port != 31001 || n.fingerprint != source.Fingerprint([]source.Server{a, b}) {
		t.Fatalf("port %d fingerprint %q", n.backends[1].port, n.fingerprint)
	}
	n.backends[0].record(okResult(42))
	n.backends[0].track(net.Pipe())
	n.active.Store(n.backends[1])

	if err := n.apply([]source.Server{b, c, a}, false); err != nil {
		t.Fatal(err)
	}
	if got := names(n); strings.Join(got, ",") != "b,c,a" {
		t.Fatalf("backends %v", got)
	}
	carried := n.backends[2]
	if carried.lastMs() != 42 || carried.okTotal != 1 || carried.dialCount() != 1 || carried.connCount() != 0 || carried.port != 31002 {
		t.Fatalf("history not carried over: %+v", carried)
	}
	if n.active.Load() != n.backends[0] {
		t.Fatal("active server must follow its key")
	}

	n.pinned = true
	if err := n.apply([]source.Server{c}, false); err != nil {
		t.Fatal(err)
	}
	if activeName(n) != "c" || n.pinned {
		t.Fatalf("active %s pinned %v", activeName(n), n.pinned)
	}
}

func TestApplySkipsRejected(t *testing.T) {
	n := applyNode(t)
	broken := testServer("broken")
	broken.Outbound = []byte(`{`)
	err := n.apply([]source.Server{testServer("ok"), testServer(rejectMarker), broken}, true)
	if err != nil {
		t.Fatal(err)
	}
	if got := names(n); len(got) != 1 || got[0] != "ok" {
		t.Fatalf("backends %v", got)
	}
	if !hasLog(n.logs, "testing servers one by one") || !hasLog(n.logs, "server skipped") {
		t.Fatal("skips not logged")
	}
}

func TestApplyNoneAccepted(t *testing.T) {
	n := applyNode(t)
	err := n.apply([]source.Server{testServer(rejectMarker + "1"), testServer(rejectMarker + "2")}, true)
	if err == nil || !strings.Contains(err.Error(), "accepted none") {
		t.Fatalf("err = %v", err)
	}
}

func TestApplyConflict(t *testing.T) {
	n := applyNode(t)
	err := n.apply([]source.Server{testServer(conflictMarker + "1"), testServer(conflictMarker + "2")}, true)
	if err == nil || !strings.Contains(err.Error(), "conflicting") {
		t.Fatalf("err = %v", err)
	}
	if len(n.backends) != 0 {
		t.Fatal("a failed apply must not install backends")
	}
}

// reloadNode is a node on a fresh throne.db whose servers are served by SOCKS
// servers on base_port+i.
func reloadNode(t *testing.T, size int, profiles ...profile) (*Node, []*socksServer, string) {
	target := startTarget(t)
	cfg := baseConfig(t, target.URL)
	base, ss := startSocksRange(t, size)
	cfg.BasePort = base
	cfg.ThroneDB = filepath.Join(t.TempDir(), "throne.db")
	writeThroneDB(t, cfg.ThroneDB, profiles...)
	log, buf := newLogs()
	n := New(cfg, xray.NewRunner(fakeXrayBin, cfg.DataDir, log), log, buf)
	servers, err := n.loadServers()
	if err != nil {
		t.Fatal(err)
	}
	if err := n.apply(servers, true); err != nil {
		t.Fatal(err)
	}
	n.active.Store(n.backends[0])
	return n, ss, cfg.ThroneDB
}

func TestReload(t *testing.T) {
	n, _, db := reloadNode(t, 2, profile{name: "a"})
	c := ctxT(t)

	n.reload(c)
	if hasLog(n.logs, "changed") {
		t.Fatal("an unchanged list must not reload")
	}

	writeThroneDB(t, db, profile{name: "a"}, profile{name: "b"})
	n.reload(c)
	if got := names(n); strings.Join(got, ",") != "a,b" {
		t.Fatalf("backends %v", got)
	}
	if !hasLog(n.logs, "server list in Throne changed") || activeName(n) != "a" {
		t.Fatalf("active %s", activeName(n))
	}
	if !n.backends[1].healthy() {
		t.Fatal("the new list must be scanned")
	}
	if st := n.loadState(); len(st.Servers) != 2 {
		t.Fatalf("state not saved: %+v", st)
	}
}

func TestReloadFailsOverWhenActiveDies(t *testing.T) {
	n, ss, db := reloadNode(t, 2, profile{name: "a"})
	ss[0].fail.Store(true)
	writeThroneDB(t, db, profile{name: "a"}, profile{name: "b"})
	n.reload(ctxT(t))
	if activeName(n) != "b" {
		t.Fatalf("active %s", activeName(n))
	}
}

func TestReloadKeepsListOnProblems(t *testing.T) {
	n, _, db := reloadNode(t, 1, profile{name: "a"})
	c := ctxT(t)

	writeThroneDB(t, db)
	n.reload(c)
	if !hasLog(n.logs, "no matching profiles left") {
		t.Fatal("empty list not reported")
	}

	writeThroneDB(t, db, profile{name: rejectMarker})
	n.reload(c)
	if !hasLog(n.logs, "new list not applied") || names(n)[0] != "a" {
		t.Fatal("a rejected list must keep the old one")
	}

	os.Remove(db)
	n.reload(c)
	if !hasLog(n.logs, "could not read throne.db") || names(n)[0] != "a" {
		t.Fatal("unreadable db must keep the old list")
	}
}

func TestReloadXrayNotUp(t *testing.T) {
	n, ss, db := reloadNode(t, 1, profile{name: "a"})
	ss[0].close()
	writeThroneDB(t, db, profile{name: "b"})
	c, cancel := context.WithCancel(context.Background())
	cancel()
	n.reload(c)
	if !hasLog(n.logs, "xray did not come up after reload") {
		t.Fatal("xray failure not logged")
	}
}

func TestLogSummary(t *testing.T) {
	n, buf := unitNode(t, baseConfig(t, ""), startSocks(t), startSocks(t))
	n.backends[1].record(okResult(1))
	n.logSummary()
	if !hasLog(buf, "alive=1 total=2 active=srv0") {
		lines, _ := buf.Since(0)
		t.Fatalf("summary: %+v", lines)
	}
}

// runConfig is a config for Run on a fresh throne.db, with SOCKS servers on
// base_port+i standing in for xray.
func runConfig(t *testing.T, size int, profiles ...profile) (config.Config, []*socksServer) {
	target := startTarget(t)
	cfg := baseConfig(t, target.URL)
	base, ss := startSocksRange(t, size)
	cfg.BasePort = base
	cfg.ThroneDB = filepath.Join(t.TempDir(), "throne.db")
	writeThroneDB(t, cfg.ThroneDB, profiles...)
	return cfg, ss
}

type running struct {
	n      *Node
	cancel context.CancelFunc
	done   chan struct{}
	err    error
}

func startNode(t *testing.T, cfg config.Config) *running {
	log, buf := newLogs()
	n := New(cfg, xray.NewRunner(fakeXrayBin, cfg.DataDir, log), log, buf)
	ctx, cancel := context.WithCancel(context.Background())
	r := &running{n: n, cancel: cancel, done: make(chan struct{})}
	go func() { r.err = n.Run(ctx); close(r.done) }()
	t.Cleanup(func() {
		cancel()
		select {
		case <-r.done:
		case <-time.After(10 * time.Second):
		}
	})
	return r
}

func (r *running) stop(t *testing.T) error {
	t.Helper()
	r.cancel()
	select {
	case <-r.done:
		return r.err
	case <-time.After(10 * time.Second):
		t.Fatal("Run did not return")
		return nil
	}
}

func (r *running) status(t *testing.T) Status {
	st, _ := r.n.Status(context.Background())
	return st
}

func (r *running) waitStarted(t *testing.T) {
	t.Helper()
	eventually(t, 15*time.Second, "node started", func() bool {
		select {
		case <-r.done:
			t.Fatalf("Run returned early: %v", r.err)
		default:
		}
		return !r.status(t).Starting
	})
}

func TestRun(t *testing.T) {
	cfg, _ := runConfig(t, 3, profile{name: "a"}, profile{name: "b"})
	cfg.APIListen = freeAddr(t)
	cfg.CheckInterval = config.Duration{Duration: 20 * time.Millisecond}
	cfg.ScanInterval = config.Duration{Duration: 20 * time.Millisecond}
	cfg.ReloadInterval = config.Duration{Duration: 20 * time.Millisecond}

	r := startNode(t, cfg)
	r.waitStarted(t)
	c := NewClient(cfg.APIListen)
	st, err := c.Status(context.Background())
	if err != nil || st.Active != "a" || len(st.Servers) != 2 {
		t.Fatalf("status %+v, err %v", st, err)
	}

	echo := startEcho(t)
	conn := socksDial(t, cfg.Listen, echo)
	conn.Close()

	if active, err := c.Switch(context.Background(), "b"); err != nil || active != "b" {
		t.Fatalf("switch: %q %v", active, err)
	}

	writeThroneDB(t, cfg.ThroneDB, profile{name: "a"}, profile{name: "b"}, profile{name: "c"})
	eventually(t, 10*time.Second, "reload picked up the new profile", func() bool { return len(r.status(t).Servers) == 3 })
	eventually(t, 10*time.Second, "periodic checks", func() bool {
		st := r.status(t)
		return st.Servers[1].OK >= 3 && st.Servers[2].OK >= 2
	})
	if st := r.status(t); st.Active != "b" || !st.Pinned {
		t.Fatalf("after reload: active %s pinned %v", st.Active, st.Pinned)
	}

	if err := r.stop(t); err != nil {
		t.Fatal(err)
	}
	if !hasLog(r.n.logs, "shutting down") {
		t.Fatal("shutdown not logged")
	}

	// A restart resumes on the last active server.
	r = startNode(t, cfg)
	r.waitStarted(t)
	if st := r.status(t); st.Active != "b" {
		t.Fatalf("resumed on %s", st.Active)
	}
	if err := r.stop(t); err != nil {
		t.Fatal(err)
	}
}

func TestRunInitialServerDown(t *testing.T) {
	cfg, ss := runConfig(t, 2, profile{name: "a"}, profile{name: "b"})
	ss[0].fail.Store(true)
	r := startNode(t, cfg)
	r.waitStarted(t)
	if st := r.status(t); st.Active != "b" || st.Switches != 1 {
		t.Fatalf("active %s switches %d", st.Active, st.Switches)
	}
	if !hasLog(r.n.logs, "initial server not responding") {
		t.Fatal("reason not logged")
	}
	r.stop(t)
}

func TestRunFromSavedState(t *testing.T) {
	cfg, _ := runConfig(t, 1)
	os.Remove(cfg.ThroneDB)
	data, _ := json.Marshal(state{ActiveKey: "key-a", Servers: []source.Server{testServer("a")}})
	os.WriteFile(filepath.Join(cfg.DataDir, "state.json"), data, 0o600)

	r := startNode(t, cfg)
	r.waitStarted(t)
	if st := r.status(t); st.Active != "a" {
		t.Fatalf("active %s", st.Active)
	}
	if !hasLog(r.n.logs, "using the saved server list") {
		t.Fatal("fallback not logged")
	}
	r.stop(t)
}

func TestRunAPIListenFails(t *testing.T) {
	cfg, _ := runConfig(t, 1, profile{name: "a"})
	busy, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer busy.Close()
	cfg.APIListen = busy.Addr().String()
	r := startNode(t, cfg)
	r.waitStarted(t)
	if !hasLog(r.n.logs, "API not started") {
		t.Fatal("API failure not logged")
	}
	r.stop(t)
}

func runErr(t *testing.T, cfg config.Config) error {
	t.Helper()
	r := startNode(t, cfg)
	select {
	case <-r.done:
		return r.err
	case <-time.After(15 * time.Second):
		t.Fatal("Run did not fail")
		return nil
	}
}

func TestRunErrors(t *testing.T) {
	t.Run("no db and no saved list", func(t *testing.T) {
		cfg, _ := runConfig(t, 1)
		os.Remove(cfg.ThroneDB)
		if err := runErr(t, cfg); err == nil || !strings.Contains(err.Error(), "loading servers from Throne") {
			t.Fatalf("err = %v", err)
		}
	})
	t.Run("no matching profiles", func(t *testing.T) {
		cfg, _ := runConfig(t, 1)
		if err := runErr(t, cfg); err == nil || !strings.Contains(err.Error(), "no xray profiles matched") {
			t.Fatalf("err = %v", err)
		}
	})
	t.Run("data dir", func(t *testing.T) {
		cfg, _ := runConfig(t, 1, profile{name: "a"})
		file := filepath.Join(t.TempDir(), "file")
		os.WriteFile(file, nil, 0o600)
		cfg.DataDir = filepath.Join(file, "data")
		if err := runErr(t, cfg); err == nil {
			t.Fatal("expected an error")
		}
	})
	t.Run("all rejected", func(t *testing.T) {
		cfg, _ := runConfig(t, 1, profile{name: rejectMarker})
		if err := runErr(t, cfg); err == nil || !strings.Contains(err.Error(), "accepted none") {
			t.Fatalf("err = %v", err)
		}
	})
	t.Run("listen", func(t *testing.T) {
		cfg, _ := runConfig(t, 1, profile{name: "a"})
		busy, err := net.Listen("tcp", "127.0.0.1:0")
		if err != nil {
			t.Fatal(err)
		}
		defer busy.Close()
		cfg.Listen = busy.Addr().String()
		if err := runErr(t, cfg); err == nil || !strings.Contains(err.Error(), "listen") {
			t.Fatalf("err = %v", err)
		}
	})
	t.Run("xray never comes up", func(t *testing.T) {
		cfg, ss := runConfig(t, 1, profile{name: "a"})
		ss[0].close()
		r := startNode(t, cfg)
		time.Sleep(200 * time.Millisecond)
		if err := r.stop(t); !errors.Is(err, context.Canceled) {
			t.Fatalf("err = %v", err)
		}
	})
}

func TestCandidates(t *testing.T) {
	n, _ := unitNode(t, baseConfig(t, ""), startSocks(t), startSocks(t), startSocks(t), startSocks(t), startSocks(t))
	for i, ms := range []int{0, 50, 10, 50, -1} {
		if ms >= 0 && i > 0 {
			n.backends[i].record(okResult(ms))
		}
	}
	n.backends[0].record(okResult(1))
	n.backends[4].record(failResult())
	var got []string
	for _, b := range n.candidates(n.backends[0]) {
		got = append(got, b.srv.Name)
	}
	if strings.Join(got, ",") != "srv2,srv1,srv3" {
		t.Fatalf("candidates %v: best first, ties stable, excluded and unhealthy left out", got)
	}
}
