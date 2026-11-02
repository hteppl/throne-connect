// throne-connect is a failover VPN node for Throne: it keeps every server from a
// Throne subscription warm in xray-core and exposes one SOCKS5 port that always
// points at a working server.
package main

import (
	"bufio"
	"context"
	"errors"
	"flag"
	"fmt"
	"io/fs"
	"log/slog"
	"os"
	"os/signal"
	"path/filepath"
	"runtime"
	"strings"
	"syscall"
	"text/tabwriter"
	"time"

	"github.com/charmbracelet/x/term"

	"throne-connect/internal/config"
	"throne-connect/internal/logbuf"
	"throne-connect/internal/node"
	"throne-connect/internal/source"
	"throne-connect/internal/tui"
	"throne-connect/internal/xray"
)

var version = "dev"

const usage = `throne-connect — failover VPN node for Throne

Usage:
  throne-connect                 in a terminal: the TUI; otherwise the same as run
  throne-connect tui             the TUI: attaches to a running node or starts one
  throne-connect run             run the node without the TUI (services, autostart)
  throne-connect status          show the state of the running node
  throne-connect switch <N|name> switch to a server manually
  throne-connect list            list servers found in throne.db
  throne-connect init            create a config file
  throne-connect version

Flags:
  -c <path>   config file (default ./throne-connect.json or %s)
  -v          verbose logging (includes xray output)
`

// logBufferSize is how many recent log lines the node keeps for the TUI.
const logBufferSize = 500

func main() {
	cmd, args := "", os.Args[1:]
	if len(args) > 0 && !strings.HasPrefix(args[0], "-") {
		cmd, args = args[0], args[1:]
	}
	if cmd == "" {
		cmd = "run"
		if term.IsTerminal(os.Stdin.Fd()) && term.IsTerminal(os.Stdout.Fd()) {
			cmd = "tui"
		}
	}
	fset := flag.NewFlagSet(cmd, flag.ExitOnError)
	cfgPath := fset.String("c", "", "config file")
	verbose := fset.Bool("v", false, "verbose logging")
	fset.Usage = func() { fmt.Fprintf(os.Stderr, usage, config.DefaultPath()) }
	fset.Parse(args)

	level := slog.LevelInfo
	if *verbose {
		level = slog.LevelDebug
	}
	logs := logbuf.New(logBufferSize)

	if cmd == "init" {
		exit(initConfig(*cfgPath))
		return
	}
	if cmd == "version" {
		fmt.Println("throne-connect", version)
		return
	}

	cfg, used, err := config.Load(*cfgPath)
	if err != nil {
		exit(fmt.Errorf("config: %w", err))
	}

	switch cmd {
	case "run":
		// Log to stderr for journald / docker logs, and keep recent lines for the TUI.
		log := slog.New(slog.NewMultiHandler(stderrHandler(level), logs.Handler(level)))
		if used != "" {
			log.Info("config", "file", used)
		}
		exit(run(cfg, log, logs))
	case "tui":
		err := runTUI(cfg, used, level, logs)
		if err != nil && runtime.GOOS == "windows" {
			// Started by a double click, the console window would close before
			// the error can be read.
			fmt.Fprintln(os.Stderr, "error:", err)
			fmt.Fprint(os.Stderr, "Press Enter to close…")
			bufio.NewReader(os.Stdin).ReadString('\n')
			os.Exit(1)
		}
		exit(err)
	case "list":
		exit(list(cfg))
	case "status":
		exit(status(cfg))
	case "switch":
		if fset.NArg() != 1 {
			exit(errors.New("specify a server: its number from `status` or part of its name"))
		}
		exit(switchTo(cfg, fset.Arg(0)))
	default:
		fset.Usage()
		os.Exit(2)
	}
}

func exit(err error) {
	if err != nil {
		fmt.Fprintln(os.Stderr, "error:", err)
		os.Exit(1)
	}
}

func stderrHandler(level slog.Level) slog.Handler {
	return slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{
		Level: level,
		ReplaceAttr: func(_ []string, a slog.Attr) slog.Attr {
			if a.Key == slog.TimeKey {
				a.Value = slog.StringValue(a.Value.Time().Format("2006-01-02 15:04:05"))
			}
			return a
		},
	})
}

func newNode(cfg config.Config, log *slog.Logger, logs *logbuf.Buffer) (*node.Node, error) {
	bin, err := xray.FindBinary(cfg.XrayPath)
	if err != nil {
		return nil, err
	}
	if err := os.MkdirAll(cfg.DataDir, 0o700); err != nil {
		return nil, err
	}
	log.Info("xray", "path", bin)
	return node.New(cfg, xray.NewRunner(bin, cfg.DataDir, log), log, logs), nil
}

func run(cfg config.Config, log *slog.Logger, logs *logbuf.Buffer) error {
	n, err := newNode(cfg, log, logs)
	if err != nil {
		return err
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	return n.Run(ctx)
}

// runTUI attaches to a node that is already running (a service, a container,
// another terminal) or, if there is none, runs the node inside this process.
func runTUI(cfg config.Config, used string, level slog.Level, logs *logbuf.Buffer) error {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	if cfg.APIListen != "" {
		c := node.NewClient(cfg.APIListen)
		probe, cancel := context.WithTimeout(ctx, time.Second)
		_, err := c.Status(probe)
		cancel()
		if err == nil {
			return tui.Run(ctx, c, tui.Options{Source: "attached to the node at " + cfg.APIListen})
		}
	}

	// The screen belongs to the TUI, so the embedded node logs only into the buffer.
	log := slog.New(logs.Handler(level))
	if used != "" {
		log.Info("config", "file", used)
	}
	n, err := newNode(cfg, log, logs)
	if err != nil {
		return err
	}
	nodeCtx, stopNode := context.WithCancel(ctx)
	defer stopNode()
	done := make(chan struct{})
	var nodeErr error
	go func() {
		defer close(done)
		nodeErr = n.Run(nodeCtx)
	}()

	uiErr := tui.Run(ctx, n, tui.Options{
		Source:   "embedded node, quitting stops it",
		Embedded: true,
		NodeDone: done,
		NodeErr:  func() error { return nodeErr },
	})
	select {
	case <-done:
	default:
		fmt.Println("stopping the node…")
		stopNode()
		<-done
	}
	if uiErr != nil {
		return uiErr
	}
	return nodeErr
}

func initConfig(path string) error {
	if path == "" {
		path = config.DefaultPath()
	}
	if _, err := os.Stat(path); err == nil {
		return fmt.Errorf("%s already exists", path)
	} else if !errors.Is(err, fs.ErrNotExist) {
		return err
	}
	cfg := config.Default()
	cfg.ThroneDB = source.DefaultThroneDB()
	data, err := cfg.Marshal()
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return err
	}
	if err := os.WriteFile(path, append(data, '\n'), 0o600); err != nil {
		return err
	}
	fmt.Println("created", path)
	return nil
}

func list(cfg config.Config) error {
	servers, err := source.LoadThrone(cfg.ThroneDB, cfg.Groups, cfg.Include, cfg.Exclude)
	if err != nil {
		return err
	}
	w := tabwriter.NewWriter(os.Stdout, 0, 0, 2, ' ', 0)
	fmt.Fprintln(w, "#\tNAME\tGROUP\tKEY")
	for i, s := range servers {
		fmt.Fprintf(w, "%d\t%s\t%s\t%s\n", i+1, s.Name, s.Group, s.Key)
	}
	w.Flush()
	fmt.Printf("\ntotal: %d\n", len(servers))
	return nil
}

func apiClient(cfg config.Config) (*node.Client, error) {
	if cfg.APIListen == "" {
		return nil, errors.New("api_listen is disabled in the config")
	}
	return node.NewClient(cfg.APIListen), nil
}

func status(cfg config.Config) error {
	c, err := apiClient(cfg)
	if err != nil {
		return err
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	st, err := c.Status(ctx)
	if err != nil {
		return err
	}
	state := "OK"
	if st.Degraded {
		state = "NO CONNECTIVITY"
	}
	pin := ""
	if st.Pinned {
		pin = " (pinned manually)"
	}
	fmt.Printf("State:       %s\nActive:      %s%s, since %s\nListen:      socks5://%s\nUptime:      %s, switches: %d, xray restarts: %d\n",
		state, st.Active, pin, st.ActiveSince, st.Listen, st.Uptime, st.Switches, st.XrayRestarts)
	if st.LastSwitch != "" {
		fmt.Printf("Last switch: %s\n", st.LastSwitch)
	}
	fmt.Println()
	w := tabwriter.NewWriter(os.Stdout, 0, 0, 2, ' ', 0)
	fmt.Fprintln(w, " \t#\tSERVER\tPING\tSCORE\tOK/ERR\tCONNS\tERROR")
	for _, s := range st.Servers {
		mark, ping, score := " ", "-", "-"
		if s.Active {
			mark = "▶"
		}
		if !s.Healthy {
			ping = "✗"
		} else {
			ping = fmt.Sprintf("%d ms", s.LastMs)
		}
		if s.Score >= 0 {
			score = fmt.Sprintf("%.0f", s.Score)
		}
		fmt.Fprintf(w, "%s\t%d\t%s\t%s\t%s\t%d/%d\t%d\t%s\n", mark, s.Index, s.Name, ping, score, s.OK, s.Errors, s.Conns, s.LastErr)
	}
	return w.Flush()
}

func switchTo(cfg config.Config, target string) error {
	c, err := apiClient(cfg)
	if err != nil {
		return err
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	active, err := c.Switch(ctx, target)
	if err != nil {
		return err
	}
	fmt.Println("active server:", active)
	return nil
}
