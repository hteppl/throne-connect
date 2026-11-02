package xray

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"testing"
	"time"

	"throne-connect/internal/source"
)

const fakeEnv = "THRONE_CONNECT_XRAY_FAKE"

// Markers in a config steer the fake xray: reject fails validation, crash makes
// the served process exit right away.
const (
	rejectMarker = "reject-me"
	crashMarker  = "crash-me"
)

func TestMain(m *testing.M) {
	if os.Getenv(fakeEnv) == "1" {
		os.Exit(fakeXray(os.Args[1:]))
	}
	os.Setenv(fakeEnv, "1")
	os.Exit(m.Run())
}

func fakeXray(args []string) int {
	path := args[len(args)-1]
	data, err := os.ReadFile(path)
	if err != nil {
		fmt.Println(err)
		return 2
	}
	s := string(data)
	if args[1] == "-test" {
		if strings.Contains(s, rejectMarker) {
			fmt.Println("line1\nline2\nline3\nbad outbound")
			return 1
		}
		return 0
	}
	fmt.Println("  started  ")
	fmt.Println()
	if strings.Contains(s, crashMarker) {
		return 1
	}
	time.Sleep(time.Hour)
	return 0
}

func testBin(t *testing.T) string {
	exe, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	return exe
}

// syncBuf collects log output from several goroutines.
type syncBuf struct {
	mu sync.Mutex
	sb strings.Builder
}

func (b *syncBuf) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.sb.Write(p)
}

func (b *syncBuf) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.sb.String()
}

func newRunner(t *testing.T, bin string) (*Runner, *syncBuf) {
	buf := &syncBuf{}
	log := slog.New(slog.NewTextHandler(buf, &slog.HandlerOptions{Level: slog.LevelDebug}))
	return NewRunner(bin, t.TempDir(), log), buf
}

func eventually(t *testing.T, msg string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for !cond() {
		if time.Now().After(deadline) {
			t.Fatalf("timed out: %s", msg)
		}
		time.Sleep(10 * time.Millisecond)
	}
}

func server(name, outbound string) source.Server {
	return source.Server{Key: "k-" + name, Name: name, Outbound: []byte(outbound)}
}

func TestTags(t *testing.T) {
	if InboundTag(3) != "in-3" || OutboundTag(3) != "srv-3" {
		t.Fatal(InboundTag(3), OutboundTag(3))
	}
}

func TestBuildConfig(t *testing.T) {
	servers := []source.Server{
		server("a", `{"protocol":"vless","tag":"orig"}`),
		server("b", `{"protocol":"trojan","streamSettings":{"network":"ws","sockopt":{"domainStrategy":"UseIP","mark":1}}}`),
	}
	data, err := BuildConfig(servers, 10800, []string{"1.1.1.1"}, "debug")
	if err != nil {
		t.Fatal(err)
	}
	var cfg struct {
		Log      map[string]any `json:"log"`
		DNS      map[string]any `json:"dns"`
		Inbounds []struct {
			Tag    string `json:"tag"`
			Listen string `json:"listen"`
			Port   int    `json:"port"`
		} `json:"inbounds"`
		Outbounds []struct {
			Tag            string `json:"tag"`
			Protocol       string `json:"protocol"`
			StreamSettings struct {
				Network string         `json:"network"`
				Sockopt map[string]any `json:"sockopt"`
			} `json:"streamSettings"`
		} `json:"outbounds"`
		Routing struct {
			Rules []struct {
				InboundTag  []string `json:"inboundTag"`
				OutboundTag string   `json:"outboundTag"`
			} `json:"rules"`
		} `json:"routing"`
	}
	if err := json.Unmarshal(data, &cfg); err != nil {
		t.Fatal(err)
	}
	if cfg.Log["loglevel"] != "debug" || cfg.DNS["tag"] != "dns-internal" {
		t.Fatalf("log/dns: %v %v", cfg.Log, cfg.DNS)
	}
	if len(cfg.Inbounds) != 2 || cfg.Inbounds[1].Port != 10801 || cfg.Inbounds[1].Tag != "in-1" || cfg.Inbounds[0].Listen != "127.0.0.1" {
		t.Fatalf("inbounds: %+v", cfg.Inbounds)
	}
	if len(cfg.Outbounds) != 3 || cfg.Outbounds[0].Tag != "direct" {
		t.Fatalf("outbounds: %+v", cfg.Outbounds)
	}
	a, b := cfg.Outbounds[1], cfg.Outbounds[2]
	if a.Tag != "srv-0" || a.Protocol != "vless" || a.StreamSettings.Sockopt["domainStrategy"] != "UseIPv4" {
		t.Fatalf("outbound a: %+v", a)
	}
	if b.Tag != "srv-1" || b.StreamSettings.Network != "ws" || b.StreamSettings.Sockopt["domainStrategy"] != "UseIP" || b.StreamSettings.Sockopt["mark"] != 1.0 {
		t.Fatalf("outbound b kept its settings: %+v", b)
	}
	rules := cfg.Routing.Rules
	if len(rules) != 3 || rules[0].OutboundTag != "direct" || rules[2].InboundTag[0] != "in-1" || rules[2].OutboundTag != "srv-1" {
		t.Fatalf("rules: %+v", rules)
	}
}

func TestBuildConfigBadOutbound(t *testing.T) {
	_, err := BuildConfig([]source.Server{server("broken", `{`)}, 1, nil, "warning")
	if err == nil || !strings.Contains(err.Error(), `"broken"`) {
		t.Fatalf("err = %v", err)
	}
}

func TestFindBinaryExplicit(t *testing.T) {
	p := filepath.Join(t.TempDir(), "my-xray")
	os.WriteFile(p, nil, 0o755)
	got, err := FindBinary(p)
	if err != nil || got != p {
		t.Fatalf("got %q, %v", got, err)
	}
	if _, err := FindBinary(p + "-missing"); err == nil || !strings.Contains(err.Error(), "xray_path") {
		t.Fatalf("err = %v", err)
	}
}

func binName() string {
	if runtime.GOOS == "windows" {
		return "xray.exe"
	}
	return "xray"
}

func TestFindBinaryInBin(t *testing.T) {
	dir := t.TempDir()
	t.Chdir(dir)
	os.Mkdir("bin", 0o755)
	os.WriteFile(filepath.Join("bin", binName()), nil, 0o755)
	t.Setenv("PATH", "")
	got, err := FindBinary("")
	if err != nil {
		t.Fatal(err)
	}
	want, _ := filepath.Abs(filepath.Join("bin", binName()))
	if got != want {
		t.Fatalf("got %q, want %q", got, want)
	}
}

func TestFindBinaryOnPath(t *testing.T) {
	t.Chdir(t.TempDir())
	pathDir := t.TempDir()
	p := filepath.Join(pathDir, binName())
	os.WriteFile(p, []byte("#!/bin/sh\n"), 0o755)
	t.Setenv("PATH", pathDir)
	got, err := FindBinary("")
	if err != nil || got != p {
		t.Fatalf("got %q, %v", got, err)
	}
}

func TestFindBinaryNotFound(t *testing.T) {
	t.Chdir(t.TempDir())
	// A directory named like the binary must not count.
	os.MkdirAll(filepath.Join("bin", binName()), 0o755)
	t.Setenv("PATH", "")
	if _, err := FindBinary(""); err == nil || !strings.Contains(err.Error(), "xray not found") {
		t.Fatalf("err = %v", err)
	}
}

func TestApply(t *testing.T) {
	r, _ := newRunner(t, testBin(t))
	if err := r.Apply([]byte(`{"ok":1}`)); err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(r.cfgPath)
	if err != nil || string(data) != `{"ok":1}` {
		t.Fatalf("installed config %q, %v", data, err)
	}
	// A second Apply must not block on the pending restart signal.
	if err := r.Apply([]byte(`{"ok":2}`)); err != nil {
		t.Fatal(err)
	}
	if len(r.restart) != 1 {
		t.Fatalf("restart signals pending: %d", len(r.restart))
	}
	leftovers, _ := filepath.Glob(filepath.Join(filepath.Dir(r.cfgPath), "xray-test-*"))
	if len(leftovers) != 0 {
		t.Fatalf("temp files left: %v", leftovers)
	}
}

func TestApplyRejected(t *testing.T) {
	r, _ := newRunner(t, testBin(t))
	os.WriteFile(r.cfgPath, []byte("previous"), 0o600)
	err := r.Apply([]byte(rejectMarker))
	if err == nil || !strings.Contains(err.Error(), "xray rejected config") {
		t.Fatalf("err = %v", err)
	}
	if !strings.Contains(err.Error(), "line2 | line3 | bad outbound") || strings.Contains(err.Error(), "line1") {
		t.Fatalf("expected the last 3 output lines: %v", err)
	}
	if data, _ := os.ReadFile(r.cfgPath); string(data) != "previous" {
		t.Fatalf("previous config replaced: %q", data)
	}
	if len(r.restart) != 0 {
		t.Fatal("restart requested for a rejected config")
	}
}

func TestApplyNoDataDir(t *testing.T) {
	r := NewRunner(testBin(t), filepath.Join(t.TempDir(), "missing"), slog.New(slog.DiscardHandler))
	if err := r.Apply([]byte("{}")); err == nil {
		t.Fatal("expected an error")
	}
}

func TestApplyWriteFails(t *testing.T) {
	r, _ := newRunner(t, testBin(t))
	os.Mkdir(r.cfgPath+".new", 0o700)
	if err := r.Apply([]byte("{}")); err == nil {
		t.Fatal("expected an error")
	}
}

func TestApplyRenameFails(t *testing.T) {
	r, _ := newRunner(t, testBin(t))
	os.MkdirAll(filepath.Join(r.cfgPath, "busy"), 0o700)
	if err := r.Apply([]byte("{}")); err == nil {
		t.Fatal("expected an error")
	}
}

func TestFilterValid(t *testing.T) {
	r, logs := newRunner(t, testBin(t))
	servers := []source.Server{
		server("good", `{}`),
		server("rejected", rejectMarker),
		server("unbuildable", ``),
		server("good2", `{}`),
	}
	build := func(s []source.Server) ([]byte, error) {
		if len(s[0].Outbound) == 0 {
			return nil, errors.New("cannot build")
		}
		return s[0].Outbound, nil
	}
	got := r.FilterValid(servers, build)
	if len(got) != 2 || got[0].Name != "good" || got[1].Name != "good2" {
		t.Fatalf("got %+v", got)
	}
	out := logs.String()
	if !strings.Contains(out, "server=rejected") || !strings.Contains(out, "server=unbuildable") {
		t.Fatalf("skips not logged:\n%s", out)
	}
}

func startRun(t *testing.T, r *Runner) (context.CancelFunc, chan struct{}) {
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { r.Run(ctx); close(done) }()
	t.Cleanup(func() {
		cancel()
		<-done
	})
	return cancel, done
}

func waitDone(t *testing.T, done chan struct{}) {
	t.Helper()
	select {
	case <-done:
	case <-time.After(10 * time.Second):
		t.Fatal("Run did not return")
	}
}

func TestRunRestartsOnApply(t *testing.T) {
	r, logs := newRunner(t, testBin(t))
	if err := r.Apply([]byte(`{"v":1}`)); err != nil {
		t.Fatal(err)
	}
	cancel, done := startRun(t, r)
	eventually(t, "xray started", func() bool { return strings.Contains(logs.String(), "xray: started") })
	if r.Restarts.Load() != 0 {
		t.Fatal("the initial Apply must not count as a restart")
	}
	if err := r.Apply([]byte(`{"v":2}`)); err != nil {
		t.Fatal(err)
	}
	eventually(t, "restart after Apply", func() bool { return r.Restarts.Load() == 1 })
	eventually(t, "second start", func() bool { return strings.Count(logs.String(), "xray started") == 2 })
	cancel()
	waitDone(t, done)
	if strings.Contains(logs.String(), "xray exited") {
		t.Fatalf("a restart is not a crash:\n%s", logs.String())
	}
}

func TestRunRestartsAfterCrash(t *testing.T) {
	r, logs := newRunner(t, testBin(t))
	if err := r.Apply([]byte(crashMarker)); err != nil {
		t.Fatal(err)
	}
	cancel, done := startRun(t, r)
	eventually(t, "crash logged", func() bool { return strings.Contains(logs.String(), "xray exited") })
	// The runner is now backing off; a new config ends the wait early.
	if err := r.Apply([]byte(`{"fixed":true}`)); err != nil {
		t.Fatal(err)
	}
	eventually(t, "restarted with the new config", func() bool {
		return strings.Count(logs.String(), "xray started") >= 2 && r.Restarts.Load() >= 1
	})
	cancel()
	waitDone(t, done)
}

func TestRunBackoffAfterCrash(t *testing.T) {
	r, logs := newRunner(t, testBin(t))
	if err := r.Apply([]byte(crashMarker)); err != nil {
		t.Fatal(err)
	}
	cancel, done := startRun(t, r)
	eventually(t, "restarted after the backoff", func() bool { return r.Restarts.Load() >= 2 })
	cancel()
	waitDone(t, done)
	if strings.Count(logs.String(), "xray exited") < 2 {
		t.Fatalf("logs:\n%s", logs.String())
	}
}

func TestRunStartFails(t *testing.T) {
	r, logs := newRunner(t, filepath.Join(t.TempDir(), "no-such-xray"))
	cancel, done := startRun(t, r)
	eventually(t, "start failure logged", func() bool { return strings.Contains(logs.String(), "failed to start xray") })
	cancel()
	waitDone(t, done)
}

func TestRunCancelledBeforeStart(t *testing.T) {
	r, _ := newRunner(t, testBin(t))
	r.restart <- struct{}{}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	r.Run(ctx)
	if len(r.restart) != 0 {
		t.Fatal("initial restart signal not drained")
	}
}

func TestWaitReady(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	if err := WaitReady(context.Background(), ln.Addr().String(), time.Second); err != nil {
		t.Fatal(err)
	}
}

func closedAddr(t *testing.T) string {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	addr := ln.Addr().String()
	ln.Close()
	return addr
}

func TestWaitReadyLater(t *testing.T) {
	addr := closedAddr(t)
	go func() {
		time.Sleep(250 * time.Millisecond)
		if ln, err := net.Listen("tcp", addr); err == nil {
			t.Cleanup(func() { ln.Close() })
		}
	}()
	if err := WaitReady(context.Background(), addr, 5*time.Second); err != nil {
		t.Fatal(err)
	}
}

func TestWaitReadyTimeout(t *testing.T) {
	err := WaitReady(context.Background(), closedAddr(t), 150*time.Millisecond)
	if err == nil || !strings.Contains(err.Error(), "did not open") {
		t.Fatalf("err = %v", err)
	}
}

func TestWaitReadyCancelled(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	time.AfterFunc(150*time.Millisecond, cancel)
	if err := WaitReady(ctx, closedAddr(t), time.Minute); !errors.Is(err, context.Canceled) {
		t.Fatalf("err = %v", err)
	}
}

func TestLastLines(t *testing.T) {
	cases := map[string]string{
		"":             "",
		"one":          "one",
		"a\nb\nc\nd\n": "b | c | d",
		"\n a\nb\n\n":  "a | b",
	}
	for in, want := range cases {
		if got := lastLines(in, 3); got != want {
			t.Errorf("lastLines(%q) = %q, want %q", in, got, want)
		}
	}
}
