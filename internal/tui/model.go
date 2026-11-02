package tui

import (
	"cmp"
	"slices"
	"strings"
	"time"

	tea "github.com/charmbracelet/bubbletea"

	"throne-connect/internal/logbuf"
	"throne-connect/internal/node"
)

type sortMode int

const (
	sortIndex sortMode = iota
	sortScore
	sortName
)

func (s sortMode) String() string {
	return [...]string{"#", "score", "name"}[s]
}

type model struct {
	c    Client
	opts Options

	st      node.Status
	stErr   error
	loaded  bool
	polling bool

	logs    []logbuf.Line
	logNext uint64

	cursorKey string // the selected server, tracked by key so it survives re-sorting
	offset    int    // first visible table row
	sort      sortMode
	filter    string
	filtering bool // typing into the filter
	showLogs  bool
	showHelp  bool

	switching   string // name of the server being switched to
	msg         string
	msgErr      bool
	msgAt       time.Time
	confirmQuit bool

	nodeDone bool
	nodeErr  error

	width, height int
}

func newModel(c Client, opts Options) model {
	return model{c: c, opts: opts, showLogs: true, polling: true}
}

func (m model) Init() tea.Cmd {
	return tea.Batch(poll(m.c, 0), tick())
}

func (m *model) setMsg(s string, isErr bool) {
	m.msg, m.msgErr, m.msgAt = s, isErr, time.Now()
}

// rows returns the servers to show: filtered, then sorted.
func (m model) rows() []node.ServerStatus {
	var out []node.ServerStatus
	f := strings.ToLower(m.filter)
	for _, s := range m.st.Servers {
		if f == "" || strings.Contains(strings.ToLower(s.Name), f) || strings.Contains(strings.ToLower(s.Group), f) {
			out = append(out, s)
		}
	}
	switch m.sort {
	case sortScore:
		slices.SortStableFunc(out, func(a, b node.ServerStatus) int {
			// Servers without a score (no successful checks) go last.
			if (a.Score < 0) != (b.Score < 0) {
				if a.Score < 0 {
					return 1
				}
				return -1
			}
			return cmp.Compare(a.Score, b.Score)
		})
	case sortName:
		slices.SortStableFunc(out, func(a, b node.ServerStatus) int {
			return cmp.Compare(strings.ToLower(a.Name), strings.ToLower(b.Name))
		})
	}
	return out
}

func (m model) cursor(rows []node.ServerStatus) int {
	for i, s := range rows {
		if s.Key == m.cursorKey {
			return i
		}
	}
	return 0
}

func (m *model) moveCursor(rows []node.ServerStatus, to int) {
	if len(rows) == 0 {
		return
	}
	to = max(0, min(to, len(rows)-1))
	m.cursorKey = rows[to].Key
	m.ensureVisible(rows)
}

// ensureVisible scrolls the table so the cursor row is on screen.
func (m *model) ensureVisible(rows []node.ServerStatus) {
	h := m.layout().tableRows
	cur := m.cursor(rows)
	if cur < m.offset {
		m.offset = cur
	}
	if cur >= m.offset+h {
		m.offset = cur - h + 1
	}
	m.offset = max(0, min(m.offset, len(rows)-h))
}

func (m *model) selectActive(rows []node.ServerStatus) {
	for i, s := range rows {
		if s.Active {
			m.moveCursor(rows, i)
			return
		}
	}
}

func (m model) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	switch msg := msg.(type) {
	case tea.WindowSizeMsg:
		m.width, m.height = msg.Width, msg.Height
		m.ensureVisible(m.rows())
		return m, nil

	case tickMsg:
		if !m.msgAt.IsZero() && time.Since(m.msgAt) > msgLifetime && m.switching == "" {
			m.msg = ""
		}
		cmds := []tea.Cmd{tick()}
		if !m.polling {
			m.polling = true
			cmds = append(cmds, poll(m.c, m.logNext))
		}
		return m, tea.Batch(cmds...)

	case pollMsg:
		m.polling = false
		m.stErr = msg.stErr
		if msg.stErr == nil {
			first := !m.loaded || len(m.st.Servers) == 0
			m.st, m.loaded = msg.st, true
			if msg.logNext < m.logNext {
				m.logs = nil // the node restarted and its log sequence began again
			}
			m.logs = append(m.logs, msg.lines...)
			if len(m.logs) > maxLogLines {
				m.logs = slices.Clone(m.logs[len(m.logs)-maxLogLines:])
			}
			m.logNext = msg.logNext
			rows := m.rows()
			if first {
				m.selectActive(rows)
			}
			m.ensureVisible(rows)
		}
		return m, nil

	case switchedMsg:
		m.switching = ""
		if msg.err != nil {
			m.setMsg("Switch to "+msg.name+" failed: "+msg.err.Error(), true)
		} else {
			m.setMsg("Active: "+msg.active+" (pinned while it works)", false)
		}
		m.polling = true
		return m, poll(m.c, m.logNext)

	case nodeExitedMsg:
		m.nodeDone, m.nodeErr = true, msg.err
		m.switching = ""
		if msg.err != nil {
			m.setMsg("The node stopped: "+msg.err.Error()+". Press q to exit.", true)
		} else {
			m.setMsg("The node stopped. Press q to exit.", true)
		}
		return m, nil

	case tea.MouseMsg:
		return m.handleMouse(msg)

	case tea.KeyMsg:
		return m.handleKey(msg)
	}
	return m, nil
}

func (m model) handleMouse(msg tea.MouseMsg) (tea.Model, tea.Cmd) {
	if m.showHelp || msg.Action != tea.MouseActionPress {
		return m, nil
	}
	rows := m.rows()
	cur := m.cursor(rows)
	switch msg.Button {
	case tea.MouseButtonWheelUp:
		m.moveCursor(rows, cur-1)
	case tea.MouseButtonWheelDown:
		m.moveCursor(rows, cur+1)
	case tea.MouseButtonLeft:
		l := m.layout()
		if i := msg.Y - l.tableTop; i >= 0 && i < l.tableRows && m.offset+i < len(rows) {
			m.moveCursor(rows, m.offset+i)
		}
	}
	return m, nil
}

func (m model) handleKey(msg tea.KeyMsg) (tea.Model, tea.Cmd) {
	key := msg.String()

	if m.confirmQuit {
		m.confirmQuit = false
		if key == "y" || key == "Y" || key == "q" || key == "ctrl+c" {
			return m, tea.Quit
		}
		m.setMsg("Cancelled, the node keeps running.", false)
		return m, nil
	}

	if m.filtering {
		rows := m.rows()
		switch msg.Type {
		case tea.KeyEsc:
			m.filtering, m.filter = false, ""
		case tea.KeyEnter:
			m.filtering = false
		case tea.KeyBackspace:
			if r := []rune(m.filter); len(r) > 0 {
				m.filter = string(r[:len(r)-1])
			}
		case tea.KeyCtrlC:
			return m.quit()
		case tea.KeyUp, tea.KeyDown:
			// fall through to navigation below
			m.filtering = false
			mm, cmd := m.handleKey(msg)
			n := mm.(model)
			n.filtering = true
			return n, cmd
		case tea.KeyRunes, tea.KeySpace:
			m.filter += string(msg.Runes)
		}
		rows = m.rows()
		m.moveCursor(rows, m.cursor(rows))
		return m, nil
	}

	if m.showHelp {
		switch key {
		case "q", "ctrl+c":
			return m.quit()
		}
		m.showHelp = false
		return m, nil
	}

	rows := m.rows()
	cur := m.cursor(rows)
	page := max(1, m.layout().tableRows-1)
	switch key {
	case "q", "ctrl+c":
		return m.quit()
	case "up", "k":
		m.moveCursor(rows, cur-1)
	case "down", "j":
		m.moveCursor(rows, cur+1)
	case "pgup", "ctrl+u":
		m.moveCursor(rows, cur-page)
	case "pgdown", "ctrl+d":
		m.moveCursor(rows, cur+page)
	case "home", "g":
		m.moveCursor(rows, 0)
	case "end", "G":
		m.moveCursor(rows, len(rows)-1)
	case "a":
		m.selectActive(rows)
	case "enter", " ":
		return m.switchSelected(rows, cur)
	case "/":
		m.filtering = true
	case "esc":
		m.filter = ""
		m.moveCursor(m.rows(), m.cursor(m.rows()))
	case "s":
		m.sort = (m.sort + 1) % 3
		m.ensureVisible(m.rows())
		m.setMsg("Sorted by "+m.sort.String(), false)
	case "l":
		m.showLogs = !m.showLogs
		m.ensureVisible(rows)
	case "r":
		if !m.polling {
			m.polling = true
			return m, poll(m.c, m.logNext)
		}
	case "?", "h", "f1":
		m.showHelp = true
	}
	return m, nil
}

func (m model) switchSelected(rows []node.ServerStatus, cur int) (tea.Model, tea.Cmd) {
	switch {
	case m.nodeDone:
		m.setMsg("The node is not running.", true)
		return m, nil
	case m.switching != "":
		m.setMsg("Still switching to "+m.switching+"…", false)
		return m, nil
	case len(rows) == 0:
		return m, nil
	}
	s := rows[cur]
	if s.Active && m.st.Pinned {
		m.setMsg(s.Name+" is already active.", false)
		return m, nil
	}
	m.switching = s.Name
	m.setMsg("Checking "+s.Name+" and switching…", false)
	return m, switchCmd(m.c, s.Key, s.Name)
}

func (m model) quit() (tea.Model, tea.Cmd) {
	if m.opts.Embedded && !m.nodeDone {
		m.confirmQuit = true
		m.showHelp = false
		return m, nil
	}
	return m, tea.Quit
}
