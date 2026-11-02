package tui

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/x/ansi"

	"throne-connect/internal/logbuf"
	"throne-connect/internal/node"
)

type fakeClient struct {
	st       node.Status
	switched string
}

func (f *fakeClient) Status(context.Context) (node.Status, error) { return f.st, nil }

func (f *fakeClient) Switch(_ context.Context, target string) (string, error) {
	for _, s := range f.st.Servers {
		if s.Key == target {
			f.switched = target
			return s.Name, nil
		}
	}
	return "", errors.New("not found")
}

func (f *fakeClient) Logs(context.Context, uint64) ([]logbuf.Line, uint64, error) {
	return []logbuf.Line{{Seq: 1, Time: time.Now(), Level: "INFO", Text: "node started"}}, 1, nil
}

func fakeStatus(n int) node.Status {
	st := node.Status{Listen: "127.0.0.1:10808", Uptime: "1m", Active: "srv 3"}
	for i := 1; i <= n; i++ {
		s := node.ServerStatus{Index: i, Key: fmt.Sprint("k", i), Name: fmt.Sprintf("srv %d 🇳🇱 Нидерланды", i), Healthy: i%4 != 0, LastMs: 50 * i, Score: float64(50 * i), OK: 10}
		if i == 3 {
			s.Active = true
		}
		if !s.Healthy {
			s.Score, s.Errors, s.LastErr = -1, 3, "timeout"
		}
		st.Servers = append(st.Servers, s)
	}
	return st
}

func send(t *testing.T, m tea.Model, msgs ...tea.Msg) tea.Model {
	t.Helper()
	for _, msg := range msgs {
		var cmd tea.Cmd
		m, cmd = m.Update(msg)
		// Run synchronous follow-ups (poll, switch), skipping ticks and quit.
		if cmd != nil {
			if out := cmd(); out != nil {
				switch out.(type) {
				case pollMsg, switchedMsg:
					m = send(t, m, out)
				}
			}
		}
	}
	return m
}

func checkView(t *testing.T, m tea.Model, w, h int) string {
	t.Helper()
	v := m.View()
	lines := strings.Split(v, "\n")
	if len(lines) != h {
		t.Fatalf("view has %d lines, want %d:\n%s", len(lines), h, v)
	}
	for i, l := range lines {
		if ansi.StringWidth(l) > w {
			t.Fatalf("line %d is %d cells wide, terminal is %d: %q", i, ansi.StringWidth(l), w, ansi.Strip(l))
		}
	}
	return v
}

func TestViewFitsTerminal(t *testing.T) {
	for _, n := range []int{0, 3, 40} {
		for _, size := range [][2]int{{50, 14}, {80, 24}, {200, 60}} {
			c := &fakeClient{st: fakeStatus(n)}
			m := send(t, newModel(c, Options{Source: "test"}), tea.WindowSizeMsg{Width: size[0], Height: size[1]}, poll(c, 0)())
			checkView(t, m, size[0], size[1])
			for _, k := range []string{"l", "?"} {
				m = send(t, m, tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune(k)})
				checkView(t, m, size[0], size[1])
			}
		}
	}
}

func TestSelectAndSwitch(t *testing.T) {
	c := &fakeClient{st: fakeStatus(40)}
	var m tea.Model = newModel(c, Options{Source: "test"})
	m = send(t, m, tea.WindowSizeMsg{Width: 100, Height: 30}, poll(c, 0)())
	if got := m.(model).cursorKey; got != "k3" {
		t.Fatalf("cursor starts on %q, want the active server k3", got)
	}
	m = send(t, m, tea.KeyMsg{Type: tea.KeyEnd}, tea.KeyMsg{Type: tea.KeyEnter})
	if c.switched != "k40" {
		t.Fatalf("switched to %q, want k40", c.switched)
	}
	if v := checkView(t, m, 100, 30); !strings.Contains(ansi.Strip(v), "srv 40") {
		t.Fatalf("last row not scrolled into view:\n%s", ansi.Strip(v))
	}

	// Filter narrows the table, sorting by score keeps the selection.
	m = send(t, m, tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("/")}, tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("srv 1")}, tea.KeyMsg{Type: tea.KeyEnter})
	if rows := m.(model).rows(); len(rows) != 11 {
		t.Fatalf("filter left %d rows, want 11", len(rows))
	}
	m = send(t, m, tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("s")})
	rows := m.(model).rows()
	if rows[len(rows)-1].Healthy {
		t.Fatalf("score sort must put servers without a score last")
	}
}

func TestEmbeddedQuitAsks(t *testing.T) {
	c := &fakeClient{st: fakeStatus(3)}
	var m tea.Model = newModel(c, Options{Embedded: true})
	m, cmd := m.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("q")})
	if cmd != nil || !m.(model).confirmQuit {
		t.Fatal("q must ask for confirmation when the node is embedded")
	}
	m, _ = m.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("n")})
	if m.(model).confirmQuit {
		t.Fatal("any other key must cancel")
	}
	m, _ = m.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("q")})
	_, cmd = m.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("y")})
	if cmd == nil {
		t.Fatal("y must quit")
	}
}
