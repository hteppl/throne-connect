// Package tui is the interactive terminal UI: a live server table with manual
// switching and a log pane. It works the same over an in-process node and over
// the HTTP API of a node running elsewhere (a service, a container).
package tui

import (
	"context"
	"errors"
	"time"

	tea "github.com/charmbracelet/bubbletea"

	"throne-connect/internal/logbuf"
	"throne-connect/internal/node"
)

// Client is implemented by both *node.Node and *node.Client.
type Client interface {
	Status(ctx context.Context) (node.Status, error)
	Switch(ctx context.Context, target string) (string, error)
	SetDisabled(ctx context.Context, target string, off bool) (string, error)
	Logs(ctx context.Context, after uint64) ([]logbuf.Line, uint64, error)
}

type Options struct {
	// Source is shown in the header, e.g. "embedded node" or "service at 127.0.0.1:10809".
	Source string
	// Embedded means quitting the UI stops the node, so quitting asks for confirmation.
	Embedded bool
	// NodeDone, if set, is closed when the embedded node stops; NodeErr then
	// returns why.
	NodeDone <-chan struct{}
	NodeErr  func() error
}

// Run shows the UI until the user quits or ctx is cancelled.
func Run(ctx context.Context, c Client, opts Options) error {
	p := tea.NewProgram(newModel(c, opts), tea.WithAltScreen(), tea.WithMouseCellMotion(), tea.WithContext(ctx))
	if opts.NodeDone != nil {
		go func() {
			select {
			case <-opts.NodeDone:
				p.Send(nodeExitedMsg{err: opts.NodeErr()})
			case <-ctx.Done():
			}
		}()
	}
	_, err := p.Run()
	if errors.Is(err, tea.ErrProgramKilled) && ctx.Err() != nil {
		return nil
	}
	return err
}

const (
	pollEvery   = time.Second
	pollTimeout = 3 * time.Second
	maxLogLines = 500
	msgLifetime = 8 * time.Second
)

type tickMsg time.Time

type pollMsg struct {
	st      node.Status
	stErr   error
	lines   []logbuf.Line
	logNext uint64
}

type switchedMsg struct {
	name   string
	active string
	err    error
}

type toggledMsg struct {
	name string
	off  bool
	err  error
}

type nodeExitedMsg struct{ err error }

func tick() tea.Cmd {
	return tea.Tick(pollEvery, func(t time.Time) tea.Msg { return tickMsg(t) })
}

func poll(c Client, logAfter uint64) tea.Cmd {
	return func() tea.Msg {
		ctx, cancel := context.WithTimeout(context.Background(), pollTimeout)
		defer cancel()
		var m pollMsg
		m.st, m.stErr = c.Status(ctx)
		m.logNext = logAfter
		if m.stErr == nil {
			if lines, next, err := c.Logs(ctx, logAfter); err == nil {
				m.lines, m.logNext = lines, next
			}
		}
		return m
	}
}

func switchCmd(c Client, key, name string) tea.Cmd {
	return func() tea.Msg {
		// The node re-checks the server before switching, which takes up to `timeout`.
		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()
		active, err := c.Switch(ctx, key)
		return switchedMsg{name: name, active: active, err: err}
	}
}

func toggleCmd(c Client, key, name string, off bool) tea.Cmd {
	return func() tea.Msg {
		// Disabling the active server first finds and checks another one.
		ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
		defer cancel()
		_, err := c.SetDisabled(ctx, key, off)
		return toggledMsg{name: name, off: off, err: err}
	}
}
