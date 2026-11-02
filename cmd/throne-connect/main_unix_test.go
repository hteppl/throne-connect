//go:build darwin || linux

package main

import (
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"

	"golang.org/x/sys/unix"

	"throne-connect/internal/logbuf"
)

// interruptAfterAPI sends SIGINT to this process once the node API answers;
// the command under test turns it into a clean shutdown.
func interruptAfterAPI(t *testing.T, addr string) {
	go func() {
		waitAPI(t, addr)
		syscall.Kill(os.Getpid(), syscall.SIGINT)
	}()
}

func TestRunUntilSignal(t *testing.T) {
	cfg, path := nodeConfig(t)
	stdin := os.Stdin
	os.Stdin, _ = os.Open(os.DevNull)
	defer func() { os.Stdin.Close(); os.Stdin = stdin }()

	stderr := captureStderr(t, func() {
		interruptAfterAPI(t, cfg.APIListen)
		// No command: not a terminal, so this is `run`.
		runMain(t, "-c", path, "-v")
	})
	for _, want := range []string{"config file=" + path, "xray path=", "node started", "shutting down"} {
		if !strings.Contains(stderr, want) {
			t.Fatalf("missing %q in:\n%s", want, stderr)
		}
	}
	if _, err := os.Stat(filepath.Join(cfg.DataDir, "xray.json")); err != nil {
		t.Fatal(err)
	}
}

// onPTY runs fn with a pseudo-terminal as stdin and stdout, waits for the UI
// to draw and, if ready is set, for the node API there, types keys and returns
// what fn returned.
func onPTY(t *testing.T, ready string, fn func() error, keys ...string) error {
	t.Helper()
	master, slave := openPTY(t)
	defer master.Close()
	// The slave stays open: bubbletea may query its size after Run returns.
	if err := unix.IoctlSetWinsize(int(slave.Fd()), unix.TIOCSWINSZ, &unix.Winsize{Row: 30, Col: 100}); err != nil {
		t.Fatal(err)
	}
	drawn := make(chan struct{})
	go func() {
		buf := make([]byte, 4096)
		if _, err := master.Read(buf); err != nil {
			return
		}
		close(drawn)
		io.Copy(io.Discard, master)
	}()

	stdin, stdout := os.Stdin, os.Stdout
	os.Stdin, os.Stdout = slave, slave
	defer func() { os.Stdin, os.Stdout = stdin, stdout }()

	done := make(chan error, 1)
	go func() { done <- fn() }()
	select {
	case <-drawn:
	case err := <-done:
		return err
	case <-time.After(10 * time.Second):
		t.Fatal("the UI did not draw")
	}
	if ready != "" {
		waitAPI(t, ready)
	}
	for _, k := range keys {
		time.Sleep(200 * time.Millisecond)
		if _, err := master.Write([]byte(k)); err != nil {
			t.Fatal(err)
		}
	}
	select {
	case err := <-done:
		return err
	case <-time.After(15 * time.Second):
		t.Fatal("did not return")
		return nil
	}
}

func TestTUIAttaches(t *testing.T) {
	cfg, _ := fakeAPI(t, sampleStatus())
	logs := logbuf.New(10)
	err := onPTY(t, "", func() error { return runTUI(cfg, "", slog.LevelInfo, logs) }, "q")
	if err != nil {
		t.Fatal(err)
	}
	if lines, _ := logs.Since(0); len(lines) != 0 {
		t.Fatalf("an attached UI must not start a node: %v", lines)
	}
}

func TestTUIEmbedded(t *testing.T) {
	cfg, path := nodeConfig(t)
	logs := logbuf.New(100)
	err := onPTY(t, cfg.APIListen, func() error { return runTUI(cfg, path, slog.LevelDebug, logs) }, "q", "q")
	if err != nil {
		t.Fatal(err)
	}
	lines, _ := logs.Since(0)
	var all []string
	for _, l := range lines {
		all = append(all, l.Text)
	}
	text := strings.Join(all, "\n")
	if !strings.Contains(text, "config file="+path) {
		t.Fatalf("logs:\n%s", text)
	}
}

func TestTUIViaMain(t *testing.T) {
	cfg, path := nodeConfig(t)
	err := onPTY(t, cfg.APIListen, func() error {
		orig := os.Args
		defer func() { os.Args = orig }()
		os.Args = []string{"throne-connect", "-c", path}
		main()
		return nil
	}, "q", "q")
	if err != nil {
		t.Fatal(err)
	}
}

func TestTUINodeFails(t *testing.T) {
	cfg, _ := nodeConfig(t)
	cfg.APIListen = ""
	cfg.ThroneDB = filepath.Join(t.TempDir(), "missing.db")
	err := onPTY(t, "", func() error { return runTUI(cfg, "", slog.LevelInfo, logbuf.New(10)) }, "q", "q")
	if err == nil || !strings.Contains(err.Error(), "loading servers from Throne") {
		t.Fatalf("err = %v", err)
	}
}
