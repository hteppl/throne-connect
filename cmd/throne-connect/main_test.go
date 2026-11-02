package main

import (
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"runtime"
	"strings"
	"testing"

	"throne-connect/internal/config"
	"throne-connect/internal/logbuf"
	"throne-connect/internal/node"
)

// runMain calls main with args in this process and returns its stdout.
func runMain(t *testing.T, args ...string) string {
	t.Helper()
	orig := os.Args
	defer func() { os.Args = orig }()
	os.Args = append([]string{"throne-connect"}, args...)
	return captureStdout(t, main)
}

// mainProcess runs main with args in a child process, for paths that exit.
func mainProcess(t *testing.T, args ...string) (int, string) {
	t.Helper()
	exe, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	list, _ := json.Marshal(args)
	cmd := exec.Command(exe, "-test.run=^$")
	for _, kv := range os.Environ() {
		if !strings.HasPrefix(kv, envFakeXray+"=") {
			cmd.Env = append(cmd.Env, kv)
		}
	}
	cmd.Env = append(cmd.Env, envMainArgs+"="+string(list))
	cmd.Stdin = strings.NewReader("")
	out, err := cmd.CombinedOutput()
	code := 0
	if ee, ok := err.(*exec.ExitError); ok {
		code = ee.ExitCode()
	} else if err != nil {
		t.Fatal(err)
	}
	return code, string(out)
}

func TestVersion(t *testing.T) {
	if out := runMain(t, "version"); out != "throne-connect dev\n" {
		t.Fatalf("out %q", out)
	}
}

func TestInitConfig(t *testing.T) {
	path := filepath.Join(t.TempDir(), "sub", "config.json")
	out := runMain(t, "init", "-c", path)
	if out != "created "+path+"\n" {
		t.Fatalf("out %q", out)
	}
	cfg, used, err := config.Load(path)
	if err != nil || used != path || cfg.ThroneDB == "" {
		t.Fatalf("cfg %+v used %q err %v", cfg, used, err)
	}
	if st, _ := os.Stat(path); st.Mode().Perm() != 0o600 {
		t.Fatalf("mode %v", st.Mode())
	}
	if err := initConfig(path); err == nil || !strings.Contains(err.Error(), "already exists") {
		t.Fatalf("err = %v", err)
	}
}

func TestInitConfigDefaultPath(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("XDG_CONFIG_HOME", filepath.Join(home, ".config"))
	captureStdout(t, func() {
		if err := initConfig(""); err != nil {
			t.Error(err)
		}
	})
	if _, err := os.Stat(config.DefaultPath()); err != nil {
		t.Fatal(err)
	}
}

func TestInitConfigErrors(t *testing.T) {
	file := filepath.Join(t.TempDir(), "file")
	os.WriteFile(file, nil, 0o600)
	if err := initConfig(filepath.Join(file, "config.json")); err == nil {
		t.Fatal("expected a stat error")
	}

	if runtime.GOOS == "windows" || os.Geteuid() == 0 {
		t.Skip("a read-only directory does not stop writes here")
	}
	ro := filepath.Join(t.TempDir(), "ro")
	os.Mkdir(ro, 0o500)
	t.Cleanup(func() { os.Chmod(ro, 0o700) })
	if err := initConfig(filepath.Join(ro, "sub", "config.json")); err == nil {
		t.Fatal("expected a mkdir error")
	}
	if err := initConfig(filepath.Join(ro, "config.json")); err == nil {
		t.Fatal("expected a write error")
	}
}

func TestList(t *testing.T) {
	_, path := nodeConfig(t)
	out := runMain(t, "list", "-c", path)
	for _, want := range []string{"#  NAME", "1  Amsterdam  Main", "2  Berlin", "total: 2"} {
		if !strings.Contains(out, want) {
			t.Fatalf("missing %q in:\n%s", want, out)
		}
	}
	if strings.Contains(out, "SingBox") || strings.Contains(out, "Archived") {
		t.Fatalf("unexpected servers:\n%s", out)
	}
}

func TestListError(t *testing.T) {
	cfg := config.Default()
	cfg.ThroneDB = filepath.Join(t.TempDir(), "missing.db")
	if err := list(cfg); err == nil {
		t.Fatal("expected an error")
	}
}

// fakeAPI serves a fixed status and records switch requests.
func fakeAPI(t *testing.T, st node.Status) (config.Config, *[]string) {
	var switches []string
	mux := http.NewServeMux()
	mux.HandleFunc("GET /status", func(w http.ResponseWriter, r *http.Request) {
		json.NewEncoder(w).Encode(st)
	})
	mux.HandleFunc("GET /logs", func(w http.ResponseWriter, r *http.Request) {
		json.NewEncoder(w).Encode(node.LogsResponse{Lines: []logbuf.Line{{Seq: 1, Text: "hello"}}, Next: 1})
	})
	mux.HandleFunc("POST /switch", func(w http.ResponseWriter, r *http.Request) {
		to := r.URL.Query().Get("to")
		switches = append(switches, to)
		if to == "bad" {
			w.WriteHeader(http.StatusConflict)
			json.NewEncoder(w).Encode(map[string]string{"error": "server \"bad\" not found"})
			return
		}
		json.NewEncoder(w).Encode(map[string]string{"active": "Berlin"})
	})
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	cfg := config.Default()
	cfg.APIListen = strings.TrimPrefix(srv.URL, "http://")
	return cfg, &switches
}

func sampleStatus() node.Status {
	return node.Status{
		Listen: "127.0.0.1:10808", Uptime: "1m0s", Active: "Amsterdam", ActiveSince: "2026-01-01 10:00:00",
		Switches: 2, XrayRestarts: 1,
		Servers: []node.ServerStatus{
			{Index: 1, Name: "Amsterdam", Active: true, Healthy: true, LastMs: 42, Score: 45, OK: 5, Errors: 1, Conns: 3},
			{Index: 2, Name: "Berlin", Score: -1, Errors: 4, LastErr: "timeout"},
		},
	}
}

func TestStatus(t *testing.T) {
	cfg, _ := fakeAPI(t, sampleStatus())
	var err error
	out := captureStdout(t, func() { err = status(cfg) })
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{
		"State:       OK", "Active:      Amsterdam, since 2026-01-01 10:00:00", "socks5://127.0.0.1:10808",
		"switches: 2, xray restarts: 1", "▶  1  Amsterdam  42 ms  45", "5/1", "2  Berlin     ✗      -", "timeout",
	} {
		if !strings.Contains(out, want) {
			t.Fatalf("missing %q in:\n%s", want, out)
		}
	}
	if strings.Contains(out, "Last switch") || strings.Contains(out, "pinned") {
		t.Fatalf("unexpected lines:\n%s", out)
	}
}

func TestStatusDegradedPinned(t *testing.T) {
	st := sampleStatus()
	st.Degraded, st.Pinned, st.LastSwitch = true, true, "10:00:00: a → b (manual)"
	cfg, _ := fakeAPI(t, st)
	var err error
	out := captureStdout(t, func() { err = status(cfg) })
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"NO CONNECTIVITY", "(pinned manually)", "Last switch: 10:00:00: a → b (manual)"} {
		if !strings.Contains(out, want) {
			t.Fatalf("missing %q in:\n%s", want, out)
		}
	}
}

func TestStatusAndSwitchViaMain(t *testing.T) {
	cfg, switches := fakeAPI(t, sampleStatus())
	path := writeConfig(t, cfg)
	if out := runMain(t, "status", "-c", path); !strings.Contains(out, "Amsterdam") {
		t.Fatalf("status out:\n%s", out)
	}
	if out := runMain(t, "switch", "-c", path, "2"); out != "active server: Berlin\n" {
		t.Fatalf("switch out %q", out)
	}
	if len(*switches) != 1 || (*switches)[0] != "2" {
		t.Fatalf("switches %v", *switches)
	}
}

func TestSwitchError(t *testing.T) {
	cfg, _ := fakeAPI(t, sampleStatus())
	if err := switchTo(cfg, "bad"); err == nil || err.Error() != `server "bad" not found` {
		t.Fatalf("err = %v", err)
	}
}

func TestAPIDisabled(t *testing.T) {
	cfg := config.Default()
	cfg.APIListen = ""
	for _, err := range []error{status(cfg), switchTo(cfg, "1")} {
		if err == nil || !strings.Contains(err.Error(), "api_listen is disabled") {
			t.Fatalf("err = %v", err)
		}
	}
}

func TestStatusNodeDown(t *testing.T) {
	cfg := config.Default()
	cfg.APIListen = freeAddr(t)
	if err := status(cfg); err == nil || !strings.Contains(err.Error(), "is the node running?") {
		t.Fatalf("err = %v", err)
	}
}

func TestNewNodeErrors(t *testing.T) {
	log := slog.New(slog.DiscardHandler)
	cfg := config.Default()
	cfg.XrayPath = filepath.Join(t.TempDir(), "missing")
	if _, err := newNode(cfg, log, nil); err == nil {
		t.Fatal("expected a missing xray error")
	}
	if err := run(cfg, log, nil); err == nil {
		t.Fatal("run must fail without xray")
	}

	cfg, _ = nodeConfig(t)
	file := filepath.Join(t.TempDir(), "file")
	os.WriteFile(file, nil, 0o600)
	cfg.DataDir = filepath.Join(file, "data")
	if _, err := newNode(cfg, log, nil); err == nil {
		t.Fatal("expected a data dir error")
	}
}

func TestStderrHandlerTimeFormat(t *testing.T) {
	out := captureStderr(t, func() {
		h := stderrHandler(slog.LevelInfo)
		slog.New(h).Info("hi", "k", 1)
		slog.New(h).Debug("hidden")
	})
	if !regexp.MustCompile(`^time="\d{4}-\d\d-\d\d \d\d:\d\d:\d\d" level=INFO msg=hi k=1\n$`).MatchString(out) {
		t.Fatalf("out %q", out)
	}
}

// captureStderr redirects os.Stderr while fn runs and returns what was written.
func captureStderr(t *testing.T, fn func()) string {
	t.Helper()
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	orig := os.Stderr
	os.Stderr = w
	var buf strings.Builder
	done := make(chan struct{})
	go func() { io.Copy(&buf, r); close(done) }()
	func() {
		defer func() { os.Stderr = orig; w.Close() }()
		fn()
	}()
	<-done
	r.Close()
	return buf.String()
}

func TestTUINoXray(t *testing.T) {
	cfg := config.Default()
	cfg.APIListen = ""
	cfg.XrayPath = filepath.Join(t.TempDir(), "missing")
	if err := runTUI(cfg, "", slog.LevelInfo, logbuf.New(10)); err == nil || !strings.Contains(err.Error(), "xray_path") {
		t.Fatalf("err = %v", err)
	}
}

func TestMainExits(t *testing.T) {
	cfg, path := nodeConfig(t)
	bad := filepath.Join(t.TempDir(), "bad.json")
	os.WriteFile(bad, []byte("{"), 0o600)
	noAPI := cfg
	noAPI.APIListen = ""

	cases := []struct {
		name string
		args []string
		code int
		want string
	}{
		{"unknown command", []string{"frobnicate", "-c", path}, 2, "Usage:"},
		{"bad config", []string{"status", "-c", bad}, 1, "error: config:"},
		{"switch without target", []string{"switch", "-c", path}, 1, "specify a server"},
		{"init existing", []string{"init", "-c", path}, 1, "already exists"},
		{"list error", []string{"list", "-c", writeConfig(t, withDB(cfg, "/nonexistent/throne.db"))}, 1, "throne db"},
		{"run error", []string{"run", "-c", writeConfig(t, withXray(cfg, "/nonexistent/xray"))}, 1, "xray_path"},
		{"tui error", []string{"tui", "-c", writeConfig(t, withXray(noAPI, "/nonexistent/xray"))}, 1, "xray_path"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			code, out := mainProcess(t, c.args...)
			if code != c.code || !strings.Contains(out, c.want) {
				t.Fatalf("exit %d, output:\n%s", code, out)
			}
		})
	}
}

func withDB(cfg config.Config, db string) config.Config {
	cfg.ThroneDB = db
	return cfg
}

func withXray(cfg config.Config, bin string) config.Config {
	cfg.XrayPath = bin
	return cfg
}
