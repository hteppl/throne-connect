package tui

import (
	"context"
	"errors"
	"fmt"
	"reflect"
	"strings"
	"testing"
	"time"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"
	"github.com/charmbracelet/x/ansi"

	"throne-connect/internal/logbuf"
	"throne-connect/internal/node"
)

type fakeClient struct {
	st        node.Status
	switched  string
	statusErr error
	switchErr error
	logsErr   error
	lines     []logbuf.Line
	logNext   uint64
}

func (f *fakeClient) Status(context.Context) (node.Status, error) { return f.st, f.statusErr }

func (f *fakeClient) Switch(_ context.Context, target string) (string, error) {
	if f.switchErr != nil {
		return "", f.switchErr
	}
	for _, s := range f.st.Servers {
		if s.Key == target {
			f.switched = target
			return s.Name, nil
		}
	}
	return "", errors.New("not found")
}

func (f *fakeClient) Logs(context.Context, uint64) ([]logbuf.Line, uint64, error) {
	if f.logsErr != nil {
		return nil, 0, f.logsErr
	}
	if f.lines != nil {
		return f.lines, f.logNext, nil
	}
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

func key(s string) tea.KeyMsg {
	switch s {
	case "esc":
		return tea.KeyMsg{Type: tea.KeyEsc}
	case "enter":
		return tea.KeyMsg{Type: tea.KeyEnter}
	case "backspace":
		return tea.KeyMsg{Type: tea.KeyBackspace}
	case "ctrl+c":
		return tea.KeyMsg{Type: tea.KeyCtrlC}
	case "up":
		return tea.KeyMsg{Type: tea.KeyUp}
	case "down":
		return tea.KeyMsg{Type: tea.KeyDown}
	case "pgup":
		return tea.KeyMsg{Type: tea.KeyPgUp}
	case "pgdown":
		return tea.KeyMsg{Type: tea.KeyPgDown}
	case "home":
		return tea.KeyMsg{Type: tea.KeyHome}
	case " ":
		return tea.KeyMsg{Type: tea.KeySpace, Runes: []rune(" ")}
	}
	return tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune(s)}
}

func loaded(t *testing.T, c *fakeClient, opts Options) model {
	t.Helper()
	return send(t, newModel(c, opts), tea.WindowSizeMsg{Width: 100, Height: 30}, poll(c, 0)()).(model)
}

func isQuit(cmd tea.Cmd) bool {
	if cmd == nil {
		return false
	}
	_, ok := cmd().(tea.QuitMsg)
	return ok
}

func TestInit(t *testing.T) {
	if newModel(&fakeClient{}, Options{}).Init() == nil {
		t.Fatal("Init must start polling and the ticker")
	}
}

func TestUnknownMsg(t *testing.T) {
	m := newModel(&fakeClient{}, Options{})
	if mm, cmd := m.Update(struct{}{}); cmd != nil || !reflect.DeepEqual(mm, m) {
		t.Fatal("an unknown message must change nothing")
	}
}

func TestTick(t *testing.T) {
	if _, ok := tick()().(tickMsg); !ok {
		t.Fatal("tick must produce a tickMsg")
	}
}

func TestTickClearsMessageAndPolls(t *testing.T) {
	c := &fakeClient{st: fakeStatus(3)}
	m := loaded(t, c, Options{})
	m.msg, m.msgAt = "old", time.Now().Add(-msgLifetime-time.Second)
	mm, cmd := m.Update(tickMsg(time.Now()))
	m = mm.(model)
	if m.msg != "" || !m.polling || cmd == nil {
		t.Fatalf("msg = %q, polling = %v", m.msg, m.polling)
	}

	m.msg, m.msgAt, m.switching = "busy", time.Now().Add(-msgLifetime-time.Second), "srv 1"
	mm, _ = m.Update(tickMsg(time.Now()))
	if mm.(model).msg != "busy" {
		t.Fatal("the message must stay while a switch is running")
	}
}

func TestPoll(t *testing.T) {
	c := &fakeClient{statusErr: errors.New("down")}
	if m := poll(c, 7)().(pollMsg); m.stErr == nil || m.logNext != 7 || m.lines != nil {
		t.Fatalf("status error: %+v", m)
	}
	c = &fakeClient{st: fakeStatus(1), logsErr: errors.New("no logs")}
	if m := poll(c, 7)().(pollMsg); m.stErr != nil || m.logNext != 7 || m.lines != nil {
		t.Fatalf("logs error: %+v", m)
	}
}

func TestPollMsg(t *testing.T) {
	c := &fakeClient{st: fakeStatus(3)}
	m := loaded(t, c, Options{})

	mm, _ := m.Update(pollMsg{stErr: errors.New("connection refused")})
	m = mm.(model)
	if m.stErr == nil || !m.loaded || len(m.st.Servers) != 3 {
		t.Fatal("a failed poll must keep the last status")
	}

	var lines []logbuf.Line
	for i := range maxLogLines + 20 {
		lines = append(lines, logbuf.Line{Seq: uint64(i + 2), Text: fmt.Sprint(i)})
	}
	mm, _ = m.Update(pollMsg{st: fakeStatus(3), lines: lines, logNext: 600})
	m = mm.(model)
	if len(m.logs) != maxLogLines || m.logs[len(m.logs)-1].Text != fmt.Sprint(maxLogLines+19) {
		t.Fatalf("kept %d log lines", len(m.logs))
	}

	mm, _ = m.Update(pollMsg{st: fakeStatus(3), lines: []logbuf.Line{{Seq: 1, Text: "restarted"}}, logNext: 1})
	m = mm.(model)
	if len(m.logs) != 1 || m.logs[0].Text != "restarted" || m.logNext != 1 {
		t.Fatalf("logs after a node restart: %+v", m.logs)
	}
}

func TestSwitchedAndNodeExited(t *testing.T) {
	c := &fakeClient{st: fakeStatus(3)}
	m := loaded(t, c, Options{})
	m.switching = "srv 1"
	mm, cmd := m.Update(switchedMsg{name: "srv 1", err: errors.New("unreachable")})
	m = mm.(model)
	if m.switching != "" || !m.msgErr || !strings.Contains(m.msg, "unreachable") || cmd == nil {
		t.Fatalf("msg = %q", m.msg)
	}

	mm, _ = m.Update(nodeExitedMsg{err: errors.New("xray died")})
	m = mm.(model)
	if !m.nodeDone || !strings.Contains(m.msg, "xray died") {
		t.Fatalf("msg = %q", m.msg)
	}
	mm, _ = m.Update(nodeExitedMsg{})
	if got := mm.(model).msg; got != "The node stopped. Press q to exit." {
		t.Fatalf("msg = %q", got)
	}
}

func TestMouse(t *testing.T) {
	c := &fakeClient{st: fakeStatus(10)}
	m := loaded(t, c, Options{})
	press := func(b tea.MouseButton, y int) tea.MouseMsg {
		return tea.MouseMsg{Button: b, Action: tea.MouseActionPress, Y: y}
	}
	steps := []struct {
		msg  tea.MouseMsg
		want string
	}{
		{press(tea.MouseButtonWheelDown, 0), "k4"},
		{press(tea.MouseButtonWheelUp, 0), "k3"},
		{press(tea.MouseButtonLeft, m.layout().tableTop+6), "k7"},
		{press(tea.MouseButtonLeft, 0), "k7"},
		{press(tea.MouseButtonLeft, m.layout().tableTop+20), "k7"},
		{tea.MouseMsg{Button: tea.MouseButtonWheelDown, Action: tea.MouseActionRelease}, "k7"},
	}
	var tm tea.Model = m
	for i, s := range steps {
		tm, _ = tm.Update(s.msg)
		if got := tm.(model).cursorKey; got != s.want {
			t.Fatalf("step %d: cursor %q, want %q", i, got, s.want)
		}
	}
	tm = send(t, tm, key("?"), press(tea.MouseButtonWheelDown, 0))
	if got := tm.(model).cursorKey; got != "k7" {
		t.Fatalf("the mouse must be ignored on the help screen, cursor %q", got)
	}
}

func TestNavigationKeys(t *testing.T) {
	c := &fakeClient{st: fakeStatus(40)}
	var m tea.Model = loaded(t, c, Options{})
	page := m.(model).layout().tableRows - 1
	steps := []struct {
		key  string
		want int
	}{
		{"j", 4}, {"down", 5}, {"k", 4}, {"up", 3},
		{"pgdown", 3 + page}, {"ctrl+d", 3 + 2*page}, {"pgup", 3 + page}, {"ctrl+u", 3},
		{"G", 40}, {"home", 1}, {"end", 40}, {"g", 1}, {"a", 3},
	}
	for _, s := range steps {
		msg := key(s.key)
		switch s.key {
		case "ctrl+d":
			msg = tea.KeyMsg{Type: tea.KeyCtrlD}
		case "ctrl+u":
			msg = tea.KeyMsg{Type: tea.KeyCtrlU}
		case "end":
			msg = tea.KeyMsg{Type: tea.KeyEnd}
		}
		m, _ = m.Update(msg)
		if got, want := m.(model).cursorKey, fmt.Sprint("k", s.want); got != want {
			t.Fatalf("after %q cursor %q, want %q", s.key, got, want)
		}
	}
}

func TestOtherKeys(t *testing.T) {
	c := &fakeClient{st: fakeStatus(5)}
	m := loaded(t, c, Options{})

	mm, _ := m.Update(key("l"))
	if mm.(model).showLogs {
		t.Fatal("l must hide the log")
	}

	mm, _ = mm.Update(key("s"))
	mm, _ = mm.Update(key("s"))
	if got := mm.(model); got.sort != sortName || got.msg != "Sorted by name" {
		t.Fatalf("sort = %v, msg = %q", got.sort, got.msg)
	}
	mm, _ = mm.Update(key("s"))
	if mm.(model).sort != sortIndex {
		t.Fatal("sort must wrap around")
	}

	m = mm.(model)
	m.polling = false
	mm, cmd := m.Update(key("r"))
	if !mm.(model).polling || cmd == nil {
		t.Fatal("r must poll now")
	}
	if _, cmd = mm.Update(key("r")); cmd != nil {
		t.Fatal("r must not poll twice")
	}

	m = mm.(model)
	m.filter = "srv 2"
	mm, _ = m.Update(key("esc"))
	if mm.(model).filter != "" {
		t.Fatal("esc must clear the filter")
	}

	for _, k := range []string{"?", "h", "f1"} {
		msg := key(k)
		if k == "f1" {
			msg = tea.KeyMsg{Type: tea.KeyF1}
		}
		mm, _ = mm.Update(msg)
		if !mm.(model).showHelp {
			t.Fatalf("%s must show help", k)
		}
		mm, _ = mm.Update(key("x"))
		if mm.(model).showHelp {
			t.Fatal("any key must close help")
		}
	}

	mm, _ = mm.Update(key("?"))
	if _, cmd = mm.Update(key("q")); !isQuit(cmd) {
		t.Fatal("q on the help screen must quit")
	}
	if _, cmd = m.Update(tea.KeyMsg{Type: tea.KeyCtrlC}); !isQuit(cmd) {
		t.Fatal("ctrl+c must quit")
	}
}

func TestFilterKeys(t *testing.T) {
	c := &fakeClient{st: fakeStatus(40)}
	var m tea.Model = loaded(t, c, Options{})
	m = send(t, m, key("/"), key("s"), key("r"), key("v"), key(" "), key("2"), key("x"), key("backspace"))
	if got := m.(model); !got.filtering || got.filter != "srv 2" {
		t.Fatalf("filter = %q, filtering = %v", got.filter, got.filtering)
	}
	if got := m.(model).cursorKey; got != "k2" {
		t.Fatalf("cursor %q must move into the filtered rows", got)
	}
	m = send(t, m, key("down"), key("down"), key("up"))
	if got := m.(model); !got.filtering || got.cursorKey != "k20" {
		t.Fatalf("arrows while filtering: cursor %q, filtering = %v", got.cursorKey, got.filtering)
	}
	m = send(t, m, key("esc"))
	if got := m.(model); got.filtering || got.filter != "" {
		t.Fatal("esc must clear and leave the filter")
	}

	m = send(t, m, key("/"), key("backspace"))
	if m.(model).filter != "" {
		t.Fatal("backspace on an empty filter")
	}
	if _, cmd := m.Update(key("ctrl+c")); !isQuit(cmd) {
		t.Fatal("ctrl+c must quit while filtering")
	}
}

func TestSwitchSelected(t *testing.T) {
	c := &fakeClient{st: fakeStatus(3)}
	m := loaded(t, c, Options{})

	m.st.Pinned = true
	mm, cmd := m.Update(key("enter"))
	if cmd != nil || mm.(model).msg != fakeStatus(3).Servers[2].Name+" is already active." {
		t.Fatalf("msg = %q", mm.(model).msg)
	}

	m.st.Pinned = false
	mm, cmd = m.Update(key(" "))
	if got := mm.(model); got.switching == "" || cmd == nil {
		t.Fatal("space must start a switch")
	}
	mm, cmd = mm.Update(key("enter"))
	if cmd != nil || !strings.HasPrefix(mm.(model).msg, "Still switching") {
		t.Fatalf("msg = %q", mm.(model).msg)
	}

	m.nodeDone = true
	mm, cmd = m.Update(key("enter"))
	if cmd != nil || mm.(model).msg != "The node is not running." {
		t.Fatalf("msg = %q", mm.(model).msg)
	}

	empty := loaded(t, &fakeClient{}, Options{})
	if _, cmd = empty.Update(key("enter")); cmd != nil {
		t.Fatal("enter on an empty table must do nothing")
	}
	empty.moveCursor(nil, 3)
	if empty.cursorKey != "" {
		t.Fatal("moveCursor on no rows")
	}
}

func TestQuit(t *testing.T) {
	m := newModel(&fakeClient{}, Options{})
	if _, cmd := m.Update(key("q")); !isQuit(cmd) {
		t.Fatal("q must quit a remote UI at once")
	}
	m = newModel(&fakeClient{}, Options{Embedded: true})
	m.nodeDone = true
	if _, cmd := m.Update(key("q")); !isQuit(cmd) {
		t.Fatal("q must quit at once when the embedded node has stopped")
	}
	m.nodeDone, m.showHelp = false, true
	mm, cmd := m.Update(key("q"))
	if cmd != nil || !mm.(model).confirmQuit || mm.(model).showHelp {
		t.Fatal("q on the help screen must ask first")
	}
	for _, k := range []string{"Y", "q", "ctrl+c"} {
		m.confirmQuit = true
		if _, cmd := m.Update(key(k)); !isQuit(cmd) {
			t.Fatalf("%s must confirm", k)
		}
	}
}

func TestSortScoreUnscoredFirstInInput(t *testing.T) {
	m := newModel(&fakeClient{}, Options{})
	m.st.Servers = []node.ServerStatus{{Key: "a", Score: -1}, {Key: "b", Score: 20}, {Key: "c", Score: -1}, {Key: "d", Score: 10}}
	m.sort = sortScore
	var keys []string
	for _, s := range m.rows() {
		keys = append(keys, s.Key)
	}
	if got := strings.Join(keys, ""); got != "dbac" {
		t.Fatalf("order %q, want dbac", got)
	}
}

func TestViewStates(t *testing.T) {
	plain := func(m model) string { return ansi.Strip(checkView(t, m, 100, 30)) }
	c := &fakeClient{st: fakeStatus(3)}
	base := loaded(t, c, Options{Source: "test"})

	small := base
	small.width = 40
	if !strings.Contains(small.View(), "Terminal too small") {
		t.Fatal("small terminal")
	}

	cases := []struct {
		name string
		edit func(*model)
		want []string
	}{
		{"ok", func(m *model) {}, []string{"● OK", "3/3 servers alive", "no switches yet", "Sort: #"}},
		{"stopped", func(m *model) { m.nodeDone = true }, []string{"● STOPPED"}},
		{"no connection", func(m *model) { m.stErr = errors.New("dial refused") }, []string{"● NO CONNECTION", "dial refused", "Retrying"}},
		{"starting", func(m *model) { m.st.Starting = true }, []string{"● STARTING"}},
		{"loading", func(m *model) { m.loaded = false; m.st = node.Status{} }, []string{"● STARTING", "Loading…"}},
		{"degraded", func(m *model) { m.st.Degraded = true }, []string{"● NO CONNECTIVITY"}},
		{"pinned", func(m *model) {
			m.st.Pinned, m.st.ActiveSince, m.st.LastSwitch = true, "12:00:01", "srv 1 → srv 3"
		}, []string{"[pinned manually]", "since 12:00:01", "srv 1 → srv 3", "150 ms"}},
		{"waiting", func(m *model) { m.st.Servers = nil }, []string{"Waiting for xray"}},
		{"node gone", func(m *model) { m.st.Servers, m.nodeDone = nil, true }, []string{"The node is not running"}},
		{"filtered out", func(m *model) { m.filter = "zzz" }, []string{"No servers match", `filter "zzz": 0 of 3 servers`}},
		{"unchecked", func(m *model) { m.st.Servers[0].OK, m.st.Servers[0].Healthy = 0, false }, []string{" … "}},
		{"confirm", func(m *model) { m.confirmQuit = true }, []string{"Stop the node and exit?"}},
		{"filtering", func(m *model) { m.filtering, m.filter = true, "nl" }, []string{"Filter: nl█"}},
		{"error msg", func(m *model) { m.setMsg("boom", true) }, []string{" boom"}},
		{"info msg", func(m *model) { m.setMsg("fine", false) }, []string{" fine"}},
		{"logs", func(m *model) {
			m.logs = []logbuf.Line{
				{Time: time.Now(), Level: "WARN", Text: "slow\nserver"},
				{Time: time.Now(), Level: "ERROR", Text: "dead"},
				{Time: time.Now(), Level: "DEBUG", Text: "noise"},
			}
		}, []string{"WARN  slow server", "ERROR dead", "DEBUG noise"}},
	}
	for _, tc := range cases {
		m := base
		m.st = fakeStatus(3)
		tc.edit(&m)
		v := plain(m)
		for _, w := range tc.want {
			if !strings.Contains(v, w) {
				t.Errorf("%s: view lacks %q:\n%s", tc.name, w, v)
			}
		}
	}
}

func TestPingStyle(t *testing.T) {
	for ms, want := range map[int]lipgloss.Style{100: greenStyle, 500: yellowStyle, 900: redStyle} {
		if got := pingStyle(ms); got.GetForeground() != want.GetForeground() {
			t.Errorf("pingStyle(%d) = %v", ms, got.GetForeground())
		}
	}
}
