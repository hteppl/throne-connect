package tui

import (
	"fmt"
	"strconv"
	"strings"

	"github.com/charmbracelet/lipgloss"
	"github.com/charmbracelet/x/ansi"

	"throne-connect/internal/logbuf"
	"throne-connect/internal/node"
)

var (
	green  = lipgloss.AdaptiveColor{Light: "28", Dark: "42"}
	yellow = lipgloss.AdaptiveColor{Light: "136", Dark: "220"}
	red    = lipgloss.AdaptiveColor{Light: "160", Dark: "203"}
	accent = lipgloss.AdaptiveColor{Light: "25", Dark: "75"}
	faint  = lipgloss.AdaptiveColor{Light: "245", Dark: "243"}

	titleStyle  = lipgloss.NewStyle().Bold(true).Foreground(lipgloss.Color("231")).Background(accent).Padding(0, 1)
	boldStyle   = lipgloss.NewStyle().Bold(true)
	dimStyle    = lipgloss.NewStyle().Foreground(faint)
	greenStyle  = lipgloss.NewStyle().Foreground(green)
	yellowStyle = lipgloss.NewStyle().Foreground(yellow)
	redStyle    = lipgloss.NewStyle().Foreground(red)
	accentStyle = lipgloss.NewStyle().Foreground(accent)
	headStyle   = lipgloss.NewStyle().Bold(true).Foreground(faint)
	selStyle    = lipgloss.NewStyle().Bold(true).
			Foreground(lipgloss.AdaptiveColor{Light: "0", Dark: "231"}).
			Background(lipgloss.AdaptiveColor{Light: "153", Dark: "24"})
)

const headerLines = 5 // title, active, info, last switch, blank

type layout struct {
	tableTop  int // screen row of the first table row
	tableRows int
	logRows   int
}

func (m model) layout() layout {
	l := layout{tableTop: headerLines + 1}
	free := m.height - l.tableTop - 2 // message line + key bar
	if m.showLogs {
		l.logRows = max(3, min(10, m.height/4))
		free -= l.logRows + 1 // + separator
	}
	l.tableRows = max(1, free)
	return l
}

func (m model) View() string {
	if m.width < 50 || m.height < 14 {
		return "Terminal too small for throne-connect.\nMake it at least 50×14, or press q."
	}
	l := m.layout()
	var lines []string
	lines = append(lines, m.viewHeader()...)
	if m.showHelp {
		lines = append(lines, fit(helpLines(), m.height-headerLines-2)...)
	} else {
		lines = append(lines, m.viewTable(l)...)
		if m.showLogs {
			lines = append(lines, m.viewLogs(l.logRows)...)
		}
	}
	lines = append(lines, m.viewMessage(), m.viewKeys())
	for i, s := range lines {
		lines[i] = ansi.Truncate(s, m.width, "…")
	}
	return strings.Join(lines, "\n")
}

func (m model) viewHeader() []string {
	var badge string
	switch {
	case m.nodeDone:
		badge = redStyle.Bold(true).Render("● STOPPED")
	case m.stErr != nil:
		badge = redStyle.Bold(true).Render("● NO CONNECTION TO THE NODE")
	case !m.loaded || m.st.Starting:
		badge = yellowStyle.Bold(true).Render("● STARTING")
	case m.st.Degraded:
		badge = redStyle.Bold(true).Render("● NO CONNECTIVITY")
	default:
		badge = greenStyle.Bold(true).Render("● OK")
	}
	alive, off := 0, 0
	for _, s := range m.st.Servers {
		switch {
		case s.Disabled:
			off++
		case s.Healthy:
			alive++
		}
	}
	title := titleStyle.Render("throne-connect") + "  " + badge
	if m.loaded {
		title += dimStyle.Render(fmt.Sprintf("  %d/%d servers alive", alive, len(m.st.Servers)-off))
		if off > 0 {
			title += dimStyle.Render(fmt.Sprintf(", %d disabled", off))
		}
	}
	title += dimStyle.Render("  ·  " + m.opts.Source)

	if m.stErr != nil {
		return []string{title, redStyle.Render(m.stErr.Error()), dimStyle.Render("Retrying every second…"), "", ""}
	}
	if !m.loaded {
		return []string{title, dimStyle.Render("Loading…"), "", "", ""}
	}

	active := dimStyle.Render("Active  ") + boldStyle.Render(m.st.Active)
	for _, s := range m.st.Servers {
		if s.Active && s.Healthy {
			active += "  " + pingStyle(s.LastMs).Render(fmt.Sprintf("%d ms", s.LastMs))
		}
	}
	if m.st.Pinned {
		active += "  " + accentStyle.Render("[pinned manually]")
	}
	if m.st.ActiveSince != "" {
		active += dimStyle.Render("  since " + m.st.ActiveSince)
	}

	info := dimStyle.Render("Listen  ") + "socks5://" + m.st.Listen +
		dimStyle.Render("   TUN ") + tunLabel(m.st.TUN) +
		dimStyle.Render("   Uptime ") + m.st.Uptime +
		dimStyle.Render("   Switches ") + strconv.Itoa(m.st.Switches) +
		dimStyle.Render("   xray restarts ") + strconv.FormatInt(m.st.XrayRestarts, 10)

	last := dimStyle.Render("Last    ")
	if m.st.LastSwitch != "" {
		last += m.st.LastSwitch
	} else {
		last += dimStyle.Render("no switches yet")
	}
	return []string{title, active, info, last, ""}
}

func tunLabel(on bool) string {
	if on {
		return accentStyle.Bold(true).Render("on")
	}
	return dimStyle.Render("off")
}

func pingStyle(ms int) lipgloss.Style {
	switch {
	case ms < 300:
		return greenStyle
	case ms < 800:
		return yellowStyle
	}
	return redStyle
}

type column struct {
	title string
	width int
	right bool
}

func (m model) viewTable(l layout) []string {
	rows := m.rows()
	idxW := max(1, len(strconv.Itoa(len(m.st.Servers))))
	cols := []column{
		{"", 1, false},
		{"#", idxW, true},
		{"SERVER", 0, false},
		{"PING", 7, true},
		{"SCORE", 6, true},
		{"OK/ERR", 9, true},
		{"CONNS", 5, true},
		{"LAST ERROR", 0, false},
	}
	fixed := 1 // left margin
	for i, c := range cols {
		fixed += c.width
		if i > 0 {
			fixed += 2
		}
	}
	avail := m.width - fixed
	nameW := 6
	for _, s := range rows {
		nameW = max(nameW, ansi.StringWidth(s.Name))
	}
	nameW = min(nameW, max(10, avail-14))
	cols[2].width = nameW
	cols[7].width = max(0, avail-nameW)

	render := func(cells []string) string {
		var sb strings.Builder
		sb.WriteString(" ")
		for i, c := range cols {
			if i > 0 {
				sb.WriteString("  ")
			}
			sb.WriteString(pad(cells[i], c.width, c.right))
		}
		return sb.String()
	}

	titles := make([]string, len(cols))
	for i, c := range cols {
		titles[i] = c.title
	}
	out := []string{headStyle.Render(render(titles))}

	cur := m.cursor(rows)
	for i := m.offset; i < m.offset+l.tableRows; i++ {
		if i >= len(rows) {
			if i == 0 {
				out = append(out, dimStyle.Render(m.emptyTableText()))
			} else {
				out = append(out, "")
			}
			continue
		}
		s := rows[i]
		cells, styles := rowCells(s)
		if i == cur && !m.showHelp {
			out = append(out, selStyle.Render(pad(render(cells), m.width, false)))
			continue
		}
		for j := range cells {
			if styles[j] != nil {
				cells[j] = styles[j].Render(pad(cells[j], cols[j].width, cols[j].right))
			}
		}
		out = append(out, render(cells))
	}
	return out
}

func (m model) emptyTableText() string {
	switch {
	case !m.loaded || m.stErr != nil:
		return " "
	case m.nodeDone && len(m.st.Servers) == 0:
		return " The node is not running, see the log below."
	case len(m.st.Servers) == 0:
		return " Waiting for xray and the first server list…"
	}
	return " No servers match the filter. Press esc to clear it."
}

// rowCells returns the plain cell texts of a server row and, where it differs
// from the default, the style of each cell.
func rowCells(s node.ServerStatus) ([]string, []*lipgloss.Style) {
	mark, ping, score := " ", "", "-"
	styles := make([]*lipgloss.Style, 8)
	if s.Disabled && !s.Active {
		for i := range styles {
			styles[i] = &dimStyle
		}
		return []string{"○", strconv.Itoa(s.Index), s.Name, "off", score, fmt.Sprintf("%d/%d", s.OK, s.Errors), strconv.FormatInt(s.Conns, 10), "disabled"}, styles
	}
	if s.Active {
		mark = "▶"
		styles[0] = &greenStyle
		b := boldStyle
		styles[2] = &b
	}
	switch {
	case s.Healthy:
		ping = fmt.Sprintf("%d ms", s.LastMs)
		ps := pingStyle(s.LastMs)
		styles[3] = &ps
	case s.OK+s.Errors == 0:
		ping = "…"
		styles[3] = &dimStyle
	default:
		ping = "✗"
		styles[3] = &redStyle
	}
	if s.Score >= 0 {
		score = fmt.Sprintf("%.0f", s.Score)
	} else {
		styles[4] = &dimStyle
	}
	okErr := fmt.Sprintf("%d/%d", s.OK, s.Errors)
	conns := strconv.FormatInt(s.Conns, 10)
	if s.Conns == 0 {
		styles[6] = &dimStyle
	}
	if s.LastErr != "" {
		styles[7] = &redStyle
	}
	return []string{mark, strconv.Itoa(s.Index), s.Name, ping, score, okErr, conns, s.LastErr}, styles
}

func (m model) viewLogs(n int) []string {
	title := " Log "
	sep := dimStyle.Render("──" + title + strings.Repeat("─", max(0, m.width-2-len(title))))
	out := []string{sep}
	start := max(0, len(m.logs)-n)
	for _, l := range m.logs[start:] {
		out = append(out, logLine(l))
	}
	for len(out) < n+1 {
		out = append(out, "")
	}
	return out
}

func logLine(l logbuf.Line) string {
	lvl := l.Level
	style := dimStyle
	switch lvl {
	case "INFO":
		style = accentStyle
	case "WARN":
		style = yellowStyle
	case "ERROR":
		style = redStyle
	}
	text := strings.ReplaceAll(l.Text, "\n", " ")
	return " " + dimStyle.Render(l.Time.Local().Format("15:04:05")) + " " + style.Render(pad(lvl, 5, false)) + " " + text
}

func (m model) viewMessage() string {
	switch {
	case m.confirmQuit:
		return yellowStyle.Bold(true).Render(" Stop the node and exit? Throne loses its proxy until the node runs again.") +
			dimStyle.Render("  y = stop · any other key = cancel")
	case m.filtering:
		return " " + boldStyle.Render("Filter: ") + m.filter + "█" + dimStyle.Render("   enter = keep · esc = clear")
	case m.msg != "":
		if m.msgErr {
			return " " + redStyle.Render(m.msg)
		}
		return " " + greenStyle.Render(m.msg)
	}
	info := fmt.Sprintf(" Sort: %s", m.sort)
	if m.filter != "" {
		info += fmt.Sprintf(" · filter %q: %d of %d servers (esc to clear)", m.filter, len(m.rows()), len(m.st.Servers))
	}
	return dimStyle.Render(info)
}

func (m model) viewKeys() string {
	keys := [][2]string{
		{"↑↓", "select"}, {"enter", "switch"}, {"d", "disable"}, {"t", "tun"}, {"a", "active"}, {"/", "filter"},
		{"s", "sort"}, {"l", "log"}, {"?", "help"}, {"q", "quit"},
	}
	if m.showHelp {
		keys = [][2]string{{"any key", "back"}, {"q", "quit"}}
	}
	var parts []string
	for _, k := range keys {
		parts = append(parts, accentStyle.Bold(true).Render(k[0])+" "+dimStyle.Render(k[1]))
	}
	return " " + strings.Join(parts, dimStyle.Render(" · "))
}

func helpLines() []string {
	key := func(k, d string) string { return "   " + accentStyle.Bold(true).Render(pad(k, 16, false)) + d }
	return []string{
		boldStyle.Render(" Keys"),
		key("↑ ↓  k j", "move the selection (mouse wheel and click work too)"),
		key("PgUp PgDn", "page up / down;  Home End (g G) jump to the first / last server"),
		key("Enter  Space", "switch to the selected server; it is checked first, then pinned"),
		key("d", "disable / enable the selected server: a disabled one is not checked or used"),
		key("t", "turn the built-in TUN on / off (Linux, needs root; routes all traffic through the VPN)"),
		key("a", "jump to the active server"),
		key("/", "filter by name or group;  Esc clears the filter"),
		key("s", "sort by #, score or name"),
		key("l", "show / hide the log"),
		key("r", "refresh now"),
		key("q  Ctrl+C", "quit (for an embedded node: stop it, after confirmation)"),
		"",
		boldStyle.Render(" Columns"),
		key("PING", "latency of the last successful check; ✗ = the last check failed, … = not checked yet,"),
		key("", "off = disabled (○)"),
		key("SCORE", "median latency + 1000 per failure in the last 10 checks; lower is better"),
		key("OK/ERR", "checks passed / failed since start"),
		key("CONNS", "connections opened to this server since start"),
		"",
		boldStyle.Render(" Pinned"),
		"   A manually chosen server is kept while it works. If it fails, the node fails over",
		"   as usual and the pin is dropped.",
	}
}

func fit(lines []string, n int) []string {
	if len(lines) > n {
		return lines[:n]
	}
	for len(lines) < n {
		lines = append(lines, "")
	}
	return lines
}

// pad truncates or pads s to exactly w cells.
func pad(s string, w int, right bool) string {
	if w <= 0 {
		return ""
	}
	s = ansi.Truncate(s, w, "…")
	gap := strings.Repeat(" ", max(0, w-ansi.StringWidth(s)))
	if right {
		return gap + s
	}
	return s + gap
}
