//go:build darwin || linux

package tui

import (
	"context"
	"io"
	"os"
	"runtime"
	"strings"
	"testing"
	"time"

	"golang.org/x/sys/unix"
)

// runOnPTY runs the UI with a pseudo-terminal as stdin and stdout, waits for
// it to draw, then calls act with the terminal's master side.
func runOnPTY(t *testing.T, ctx context.Context, c Client, opts Options, act func(master *os.File)) error {
	t.Helper()
	master, slave := openPTY(t)
	defer master.Close()
	// The slave is left open: when the context kills the program, bubbletea may
	// still query its size from a goroutine it does not wait for.
	if err := unix.IoctlSetWinsize(int(slave.Fd()), unix.TIOCSWINSZ, &unix.Winsize{Row: 24, Col: 80}); err != nil {
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
	go func() { done <- Run(ctx, c, opts) }()
	select {
	case <-drawn:
	case err := <-done:
		return err
	case <-time.After(5 * time.Second):
		t.Fatal("the UI did not draw")
	}
	act(master)
	select {
	case err := <-done:
		return err
	case <-time.After(5 * time.Second):
		t.Fatal("Run did not return")
	}
	return nil
}

func TestRunQuits(t *testing.T) {
	c := &fakeClient{st: fakeStatus(3)}
	err := runOnPTY(t, context.Background(), c, Options{Source: "test"}, func(master *os.File) {
		if _, err := master.Write([]byte("q")); err != nil {
			t.Error(err)
		}
	})
	if err != nil {
		t.Fatalf("Run = %v", err)
	}
}

// waitInputLoop waits until bubbletea's input goroutine is running or gone.
func waitInputLoop(t *testing.T, running bool) {
	t.Helper()
	buf := make([]byte, 1<<20)
	for deadline := time.Now().Add(5 * time.Second); ; time.Sleep(time.Millisecond) {
		n := runtime.Stack(buf, true)
		if strings.Contains(string(buf[:n]), "bubbletea.(*Program).readLoop") == running {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("input loop running = %v, want %v", !running, running)
		}
	}
}

func TestRunStopsOnContext(t *testing.T) {
	if runtime.GOOS != "darwin" {
		t.Skip("reading a pty whose master is closed fails with EIO here, not EOF")
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	c := &fakeClient{st: fakeStatus(3)}
	err := runOnPTY(t, ctx, c, Options{Embedded: true, NodeDone: make(chan struct{})}, func(master *os.File) {
		// Stop the input loop with EOF first: when the program is killed while
		// that loop is still reading, bubbletea closes the reader under it,
		// which the race detector reports.
		waitInputLoop(t, true)
		master.Close()
		waitInputLoop(t, false)
		cancel()
	})
	if err != nil {
		t.Fatalf("Run = %v, want nil after the context is cancelled", err)
	}
}

func TestRunReportsNodeExit(t *testing.T) {
	nodeDone := make(chan struct{})
	close(nodeDone)
	asked := make(chan struct{})
	opts := Options{NodeDone: nodeDone, NodeErr: func() error { close(asked); return nil }}
	err := runOnPTY(t, context.Background(), &fakeClient{st: fakeStatus(3)}, opts, func(master *os.File) {
		<-asked
		if _, err := master.Write([]byte("q")); err != nil {
			t.Error(err)
		}
	})
	if err != nil {
		t.Fatalf("Run = %v", err)
	}
}
