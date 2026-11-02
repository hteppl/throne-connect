package xray

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"throne-connect/internal/source"
)

// FindBinary locates xray: explicit path, next to our executable, ./bin, then PATH.
func FindBinary(explicit string) (string, error) {
	name := "xray"
	if runtime.GOOS == "windows" {
		name = "xray.exe"
	}
	if explicit != "" {
		if _, err := os.Stat(explicit); err != nil {
			return "", fmt.Errorf("xray_path: %w", err)
		}
		return explicit, nil
	}
	var dirs []string
	if exe, err := os.Executable(); err == nil {
		dirs = append(dirs, filepath.Dir(exe), filepath.Join(filepath.Dir(exe), "bin"))
	}
	dirs = append(dirs, "bin")
	for _, d := range dirs {
		p := filepath.Join(d, name)
		if st, err := os.Stat(p); err == nil && !st.IsDir() {
			return filepath.Abs(p)
		}
	}
	if p, err := exec.LookPath(name); err == nil {
		return p, nil
	}
	return "", errors.New("xray not found: put it next to throne-connect or in ./bin, or set xray_path (see scripts/get-xray)")
}

// Runner keeps one xray process alive with the current config.
type Runner struct {
	bin      string
	cfgPath  string
	log      *slog.Logger
	restart  chan struct{}
	Restarts atomic.Int64
	mu       sync.Mutex
}

func NewRunner(bin, dataDir string, log *slog.Logger) *Runner {
	return &Runner{
		bin:     bin,
		cfgPath: filepath.Join(dataDir, "xray.json"),
		log:     log,
		restart: make(chan struct{}, 1),
	}
}

// Apply validates cfg with `xray run -test`, installs it and asks the running
// process to restart. On a validation failure the previous config stays in place.
func (r *Runner) Apply(cfg []byte) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	if err := r.test(cfg); err != nil {
		return err
	}
	tmp := r.cfgPath + ".new"
	if err := os.WriteFile(tmp, cfg, 0o600); err != nil {
		return err
	}
	if err := os.Rename(tmp, r.cfgPath); err != nil {
		return err
	}
	select {
	case r.restart <- struct{}{}:
	default:
	}
	return nil
}

func (r *Runner) test(cfg []byte) error {
	f, err := os.CreateTemp(filepath.Dir(r.cfgPath), "xray-test-*.json")
	if err != nil {
		return err
	}
	defer os.Remove(f.Name())
	if _, err := f.Write(cfg); err != nil {
		f.Close()
		return err
	}
	f.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	out, err := exec.CommandContext(ctx, r.bin, "run", "-test", "-c", f.Name()).CombinedOutput()
	if err != nil {
		return fmt.Errorf("xray rejected config: %v: %s", err, lastLines(string(out), 3))
	}
	return nil
}

// FilterValid drops servers xray cannot load, testing them one by one. It is only
// needed when the full config fails validation.
func (r *Runner) FilterValid(servers []source.Server, build func([]source.Server) ([]byte, error)) []source.Server {
	var ok []source.Server
	for _, s := range servers {
		cfg, err := build([]source.Server{s})
		if err == nil {
			err = r.test(cfg)
		}
		if err != nil {
			r.log.Warn("server skipped: xray rejects its config", "server", s.Name, "err", err)
			continue
		}
		ok = append(ok, s)
	}
	return ok
}

// Run supervises xray until ctx is cancelled, restarting it on exit with backoff
// and on every Apply.
func (r *Runner) Run(ctx context.Context) {
	backoff := time.Second
	// Drain the signal from the initial Apply; we are about to start with that config.
	select {
	case <-r.restart:
	default:
	}
	for ctx.Err() == nil {
		cmd := exec.Command(r.bin, "run", "-c", r.cfgPath)
		setProcAttrs(cmd)
		pr, pw := io.Pipe()
		cmd.Stdout, cmd.Stderr = pw, pw
		go r.pump(pr)
		started := time.Now()
		if err := cmd.Start(); err != nil {
			pw.Close()
			r.log.Error("failed to start xray", "err", err)
		} else {
			r.log.Info("xray started", "pid", cmd.Process.Pid)
			done := make(chan error, 1)
			go func() { done <- cmd.Wait(); pw.Close() }()
			select {
			case <-ctx.Done():
				cmd.Process.Kill()
				<-done
				return
			case <-r.restart:
				cmd.Process.Kill()
				<-done
				r.Restarts.Add(1)
				backoff = time.Second
				continue
			case err := <-done:
				r.Restarts.Add(1)
				r.log.Error("xray exited", "err", err, "uptime", time.Since(started).Round(time.Second))
			}
		}
		if time.Since(started) > 30*time.Second {
			backoff = time.Second
		}
		select {
		case <-ctx.Done():
			return
		case <-r.restart:
		case <-time.After(backoff):
		}
		backoff = min(backoff*2, 30*time.Second)
	}
}

func (r *Runner) pump(rd io.Reader) {
	sc := bufio.NewScanner(rd)
	for sc.Scan() {
		if line := strings.TrimSpace(sc.Text()); line != "" {
			r.log.Debug("xray: " + line)
		}
	}
}

// WaitReady waits until xray accepts connections on addr.
func WaitReady(ctx context.Context, addr string, timeout time.Duration) error {
	deadline := time.Now().Add(timeout)
	for {
		c, err := net.DialTimeout("tcp", addr, 300*time.Millisecond)
		if err == nil {
			c.Close()
			return nil
		}
		if time.Now().After(deadline) {
			return fmt.Errorf("xray did not open %s within %s", addr, timeout)
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(100 * time.Millisecond):
		}
	}
}

func lastLines(s string, n int) string {
	lines := strings.Split(strings.TrimSpace(s), "\n")
	if len(lines) > n {
		lines = lines[len(lines)-n:]
	}
	return strings.Join(lines, " | ")
}
