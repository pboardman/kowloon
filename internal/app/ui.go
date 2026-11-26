package app

import (
	"cmp"
	"iter"
	"path/filepath"
	"strings"
	"time"
	"unicode"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/x/ansi"
)

const (
	repoColor   = "\x1b[38;5;183m"
	headerColor = "\x1b[1;38;5;203m"
	divider     = "\x1b[38;5;239m"
	muted       = "\x1b[38;5;246m"
	accent      = "\x1b[1;38;5;79m"
	title       = "\x1b[1;38;5;252m"
	amber       = "\x1b[1;38;5;221m"
	red         = "\x1b[1;38;5;210m"
	blue        = "\x1b[1;38;5;117m"
	reset       = "\x1b[0m"
)

type row struct{ key, path, id, prefix, repo, label, style, detail, customName string }
type tick time.Time
type ui struct {
	w                                        *workspace
	width, height, cursor, offset, menuIndex int
	rows                                     []row
	active, mode, entry, notice, path, id    string
	reveal, agentMode, agentBell             bool
	pressed                                  tea.MouseButton
}

func clean(s string) string {
	return strings.Map(func(r rune) rune {
		if unicode.IsControl(r) {
			return ' '
		}
		return r
	}, s)
}
func newUI(w *workspace) *ui { u := &ui{w: w, width: w.width, height: 24}; u.rebuild(); return u }
func pulse() tea.Cmd {
	return tea.Tick(600*time.Millisecond, func(t time.Time) tea.Msg { return tick(t) })
}
func (u *ui) Init() tea.Cmd { return pulse() }
func badge(t Terminal) (icon, color string) {
	if t.ActiveRun != nil && t.State != "exited" {
		switch t.ActiveRun.Status {
		case "waiting_for_approval":
			return "!", amber
		case "waiting_for_input":
			return "?", amber
		case "unavailable":
			return "…", muted
		case "accepted", "running":
			// The TUI can see a prompt before the next app-server update.
			if t.State != "blocked" && t.State != "waiting" {
				return "●", blue
			}
		}
	}
	switch t.State {
	case "working":
		return "●", blue
	case "waiting":
		return "?", amber
	case "blocked":
		if t.Reason == "auth" {
			return "@", red
		}
		if t.Reason == "permission" {
			return "!", amber
		}
		return "⊘", red
	case "done":
		return "✓", accent
	case "error":
		return "×", red
	case "idle":
		return "○", muted
	case "starting":
		return "◌", blue
	case "unknown":
		return "…", muted
	}
	return " ", muted
}
func terminalRow(t Terminal, active, indent, name string) row {
	marker := " "
	if t.ID == active {
		marker = "›"
	}
	icon, color := badge(t)
	if icon == " " {
		name = cmp.Or(t.Name, t.Program)
	} else {
		task := t.Name
		if task == "" {
			dir := filepath.Base(t.Checkout.Path)
			task = strings.TrimSuffix(t.Title, " | "+dir)
			if task == dir {
				task = ""
			}
		}
		if name != "" && task != "" {
			name += " · "
		}
		name += task
	}
	return row{key: t.ID, path: t.Checkout.Path, id: t.ID, customName: t.Name, prefix: marker + indent + icon + " ", label: name, style: color}
}

func (r row) renderLabel() string {
	label := clean(r.label)
	if r.repo == "" {
		return r.prefix + label
	}
	if label != "" {
		label = " · " + label
	}
	return r.prefix + repoColor + clean(r.repo) + r.style + label
}

func agentRows(s Snapshot) []row {
	var rows []row
	for _, t := range s.Terminals {
		if !t.hasAgent() {
			continue
		}
		name := ""
		if t.Program == "claude" {
			name = "Claude Code"
		}
		r := terminalRow(t, s.Active, " ", name)
		r.repo = filepath.Base(t.Checkout.Repo)
		if t.Checkout.Branch != "" {
			r.detail = "    " + t.Checkout.Branch
		}
		rows = append(rows, r)
	}
	return rows
}

func autoSidebarWidth(s Snapshot, windowWidth int) int {
	width := 28
	if s.AgentMode {
		for _, r := range agentRows(s) {
			width = max(width, ansi.StringWidth(r.renderLabel())+1, ansi.StringWidth(r.detail)+1)
		}
	} else {
		for _, c := range s.Checkouts {
			label := c.Branch
			if c.Repo == c.Path {
				label = filepath.Base(c.Repo) + "  · " + label
			}
			width = max(width, ansi.StringWidth(label)+4)
		}
	}
	return min(width, windowWidth*2/5)
}

func repositoryRows(s Snapshot) []row {
	rows := []row{}
	last := ""
	for _, c := range s.Checkouts {
		if c.Repo != last {
			last = c.Repo
			label := "▾ " + filepath.Base(c.Repo)
			if c.Path == c.Repo && c.Branch != "" {
				label += "  · " + c.Branch
			}
			rows = append(rows, row{key: "repo:" + c.Repo, path: c.Repo, label: label, style: title})
		}
		indent := "  "
		if c.Path != c.Repo {
			rows = append(rows, row{key: "tree:" + c.Path, path: c.Path, label: "  └ " + c.Branch, style: accent})
			indent = "    "
		}
		for _, t := range s.Terminals {
			if t.State == "exited" || t.Checkout.Path != c.Path {
				continue
			}
			name := "Codex"
			if t.Program == "claude" {
				name = "Claude Code"
			}
			rows = append(rows, terminalRow(t, s.Active, indent, name))
		}
	}
	return rows
}

func (u *ui) rebuild() {
	s := u.w.snapshot()
	target := u.selected().key
	if u.active != s.Active || u.agentMode != s.AgentMode {
		target = s.Active
	}
	u.active = s.Active
	if u.agentMode != s.AgentMode {
		u.cursor, u.offset = 0, 0
	}
	u.agentMode = s.AgentMode
	u.agentBell = s.AgentBell
	if s.AgentMode {
		u.rows = agentRows(s)
	} else {
		u.rows = repositoryRows(s)
	}
	for i, r := range u.rows {
		if r.key == target {
			u.cursor = i
			break
		}
		if r.id != "" && r.id == s.Active {
			u.cursor = i
		}
	}
	u.scroll()
}
func (r row) height() int {
	if r.detail != "" {
		return 2
	}
	return 1
}

// Return the first entry that fits on a page ending at last.
func (u *ui) pageStart(last int) int {
	remaining := u.footerDividerY() - 3 - u.rows[last].height()
	for last > 0 && remaining >= u.rows[last-1].height() {
		last--
		remaining -= u.rows[last].height()
	}
	return last
}
func (u *ui) scroll() {
	if len(u.rows) == 0 {
		u.cursor, u.offset = 0, 0
		return
	}
	u.cursor = max(0, min(u.cursor, len(u.rows)-1))
	u.offset = min(u.offset, u.cursor, u.pageStart(len(u.rows)-1))
	u.offset = max(u.offset, u.pageStart(u.cursor))
}

// Rendering and mouse input use the same screen coordinates for each entry.
func (u *ui) visibleRows() iter.Seq2[int, int] {
	return func(yield func(index, y int) bool) {
		y := 3
		for i := u.offset; i < len(u.rows) && y < u.footerDividerY(); i++ {
			if !yield(i, y) {
				return
			}
			y += u.rows[i].height()
		}
	}
}
func (u *ui) selected() row {
	if len(u.rows) > 0 {
		return u.rows[u.cursor]
	}
	return row{}
}
func (u *ui) report(err error) {
	if err != nil {
		u.notice = clean(err.Error())
	} else {
		u.notice = ""
	}
	u.rebuild()
}
func (u *ui) currentPath() string {
	if path := u.selected().path; path != "" {
		return path
	}
	if path, err := directory("."); err == nil {
		return path
	}
	return "/"
}
func (u *ui) newTerminal() {
	_, err := u.w.open("~", launchShell, false)
	u.report(err)
}
func (u *ui) promptWorktree(path string) {
	u.mode = "tree"
	u.path = path
	u.entry = ""
	u.notice = ""
}
func (u *ui) menu(r row) {
	if r.path == "" {
		return
	}
	u.mode = "menu"
	u.path = r.path
	u.id = r.id
	u.entry = r.customName
	u.menuIndex = 0
	u.notice = ""
}
func (u *ui) choices() []string {
	choices := []string{"New terminal", "Create worktree"}
	if u.id != "" {
		choices = append(choices, "Rename terminal", "Close terminal")
	}
	return choices
}
func (u *ui) action(i int) {
	u.mode = ""
	var err error
	switch u.choices()[i] {
	case "New terminal":
		_, err = u.w.open(u.path, launchShell, false)
	case "Create worktree":
		u.promptWorktree(u.path)
	case "Rename terminal":
		u.mode = "rename"
	case "Close terminal":
		u.mode = "close"
	}
	u.report(err)
}
func (u *ui) activate() {
	r := u.selected()
	if r.id != "" {
		u.report(u.w.selectTerm(r.id))
		return
	}
	if r.path != "" {
		// Prefer the visible terminal when a checkout contains several shells.
		var id string
		for _, candidate := range u.rows {
			if candidate.path == r.path && candidate.id != "" {
				if id == "" || candidate.id == u.active {
					id = candidate.id
				}
			}
		}
		if id != "" {
			u.report(u.w.selectTerm(id))
		} else {
			_, err := u.w.open(r.path, launchShell, false)
			u.report(err)
		}
	}
}
func (u *ui) copyKey() {
	// -w forwards OSC 52 to the attached client if its terminal allows clipboard writes.
	_, err := u.w.p.tmInput(u.w.key, "load-buffer", "-w", "-b", "kowloon-key", "-")
	if err != nil {
		u.report(err)
		return
	}
	u.notice = "Copied to tmux clipboard"
}
func (u *ui) key(k tea.KeyMsg) {
	key := k.String()
	// The tmux prefix binding forwards F12 so it cannot type into a branch prompt.
	if key == "f12" || (key == "a" && u.mode == "") {
		u.mode = ""
		u.reveal = false
		u.notice = ""
		u.w.setAgentMode(!u.agentMode)
		u.rebuild()
		return
	}
	if key == "esc" {
		u.mode = ""
		u.notice = ""
		u.reveal = false
		return
	}
	switch u.mode {
	case "settings":
		switch key {
		case "b":
			u.report(u.w.setAgentBell(!u.agentBell))
		case "r":
			u.reveal = !u.reveal
		case "c":
			u.copyKey()
		case "q", "s":
			u.mode = ""
			u.reveal = false
		}
		return
	case "menu":
		switch key {
		case "up", "k":
			u.menuIndex = max(0, u.menuIndex-1)
		case "down", "j":
			u.menuIndex = min(len(u.choices())-1, u.menuIndex+1)
		case "enter":
			u.action(u.menuIndex)
		}
		return
	case "close":
		if key == "y" {
			u.mode = ""
			u.report(u.w.closeTerm(u.id))
		} else if key == "n" {
			u.mode = ""
		}
		return
	case "tree", "rename":
		switch key {
		case "ctrl+u":
			u.entry = ""
		case "backspace", "ctrl+h":
			r := []rune(u.entry)
			if len(r) > 0 {
				u.entry = string(r[:len(r)-1])
			}
		case "enter":
			var err error
			if u.mode == "rename" {
				err = u.w.renameTerm(u.id, u.entry)
			} else {
				_, err = u.w.addTree(u.path, u.entry)
			}
			if err == nil {
				u.mode = ""
			}
			u.report(err)
		default:
			if k.Type == tea.KeyRunes || k.Type == tea.KeySpace {
				u.entry += clean(string(k.Runes))
				if k.Type == tea.KeySpace && len(k.Runes) == 0 {
					u.entry += " "
				}
			}
		}
		return
	}
	switch key {
	case "up", "k":
		u.cursor--
		u.scroll()
	case "down", "j":
		u.cursor++
		u.scroll()
	case "enter":
		u.activate()
	case "n":
		u.newTerminal()
	case "w":
		u.promptWorktree(u.currentPath())
	case "m":
		u.menu(u.selected())
	case "s":
		u.mode = "settings"
		u.reveal = false
		u.notice = ""
	case "right", "tab":
		if u.active != "" {
			u.report(u.w.selectTerm(u.active))
		}
	}
}
func (u *ui) mouse(m tea.MouseMsg) {
	// tmux can consume the second press in a rapid click sequence. Dispatch its
	// release only when the sidebar did not already handle the press.
	switch m.Action {
	case tea.MouseActionPress:
		u.pressed = m.Button
	case tea.MouseActionRelease:
		handled := u.pressed == m.Button
		u.pressed = tea.MouseButtonNone
		if handled {
			return
		}
	default:
		return
	}
	if u.mode == "" && (m.Button == tea.MouseButtonWheelUp || m.Button == tea.MouseButtonWheelDown) {
		if m.Button == tea.MouseButtonWheelUp {
			u.cursor--
		} else {
			u.cursor++
		}
		u.scroll()
		return
	}
	if m.Button == tea.MouseButtonRight && u.mode != "" {
		return
	}
	if m.Button != tea.MouseButtonLeft && m.Button != tea.MouseButtonRight {
		return
	}
	switch u.mode {
	case "menu":
		if i := m.Y - 3; i >= 0 && i < len(u.choices()) {
			u.action(i)
		} else {
			u.mode = ""
		}
		return
	case "settings":
		switch m.Y {
		case 1:
			u.report(u.w.setAgentBell(!u.agentBell))
		case 3:
			u.reveal = !u.reveal
		case 4:
			u.copyKey()
		case u.height - 1:
			u.mode = ""
			u.reveal = false
		}
		return
	case "close":
		if m.Y == 4 {
			u.mode = ""
			u.report(u.w.closeTerm(u.id))
		} else {
			u.mode = ""
		}
		return
	case "tree", "rename":
		if m.Y == u.height-1 {
			u.mode = ""
		}
		return
	}
	if m.Y == 0 && m.Button == tea.MouseButtonLeft && m.X >= u.newButtonColumn() {
		u.newTerminal()
		return
	}
	if m.Y >= u.footerDividerY() {
		if m.Button == tea.MouseButtonLeft {
			for s := range u.shortcuts() {
				if m.Y == s.y && m.X >= s.x && m.X < s.x+2+len(s.label) {
					u.key(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{s.key}})
					break
				}
			}
		}
		return
	}
	if m.Y < 3 {
		return
	}
	for i, y := range u.visibleRows() {
		if m.Y >= y && m.Y < y+u.rows[i].height() {
			u.cursor = i
			u.scroll()
			if m.Button == tea.MouseButtonRight {
				u.menu(u.rows[i])
			} else {
				u.activate()
			}
			return
		}
	}
}
func (u *ui) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	switch v := msg.(type) {
	case tea.WindowSizeMsg:
		u.width = v.Width
		u.height = v.Height
		u.scroll()
	case tea.KeyMsg:
		u.key(v)
	case tea.MouseMsg:
		u.mouse(v)
	case tick:
		if err := u.w.refresh(); err != nil {
			u.notice = clean(err.Error())
		}
		u.rebuild()
		return u, pulse()
	}
	return u, nil
}
func (u *ui) newButtonColumn() int {
	return max(len(" KOWLOON")+1, u.width-len("+ New "))
}

type shortcut struct {
	key   rune
	label string
	x, y  int
}

// Two columns fit when each has room for a key, a space, and "Settings".
const twoColumnWidth = 22

func (u *ui) footerDividerY() int {
	rows := 3
	if u.width < twoColumnWidth {
		rows = 5
	}
	return max(4, u.height-rows-1)
}

func (u *ui) shortcuts() iter.Seq[shortcut] {
	return func(yield func(shortcut) bool) {
		toggle := "Agents"
		if u.agentMode {
			toggle = "Repos"
		}
		items := [...]shortcut{{key: 'n', label: "New"}, {key: 'm', label: "Actions"},
			{key: 'a', label: toggle}, {key: 'w', label: "Worktree"}, {key: 's', label: "Settings"}}
		top := u.footerDividerY() + 1
		for i, s := range items {
			s.x, s.y = 1, top+i
			if u.width >= twoColumnWidth {
				s.x += (i % 2) * min(14, u.width/2)
				s.y = top + i/2
			}
			if s.y >= u.height || !yield(s) {
				return
			}
		}
	}
}

func (u *ui) View() string {
	if u.height < 1 || u.width < 1 {
		return ""
	}
	lines := make([]string, u.height)
	putStyled := func(y int, s, color string) {
		if y >= 0 && y < len(lines) {
			lines[y] = color + ansi.Truncate(s, u.width, "…") + reset
		}
	}
	put := func(y int, s, color string) { putStyled(y, clean(s), color) }
	put(0, " KOWLOON"+strings.Repeat(" ", u.newButtonColumn()-len(" KOWLOON"))+"+ New ", headerColor)
	put(1, strings.Repeat("─", u.width), muted)
	noticeY := u.height - 2
	switch u.mode {
	case "settings":
		put(0, " Settings", title)
		bell := "Off"
		if u.agentBell {
			bell = "On"
		}
		put(1, " [b] Agent bell: "+bell, title)
		put(2, " MCP key", accent)
		show := "Reveal key"
		if u.reveal {
			show = "Hide key"
		}
		put(3, " [r] "+show, title)
		put(4, " [c] Copy key", title)
		y := 5
		if u.reveal {
			for _, part := range chunks(u.w.key, max(1, u.width-2)) {
				put(y, " "+part, title)
				y++
			}
			y++
		}
		put(y, " Local MCP endpoint", accent)
		for i, part := range chunks(u.w.endpoint, max(1, u.width-2)) {
			put(y+i+1, " "+part, muted)
		}
		put(u.height-1, " Esc  Back", muted)
	case "menu":
		put(0, " Actions", accent)
		put(2, " "+filepath.Base(u.path), muted)
		for i, name := range u.choices() {
			mark := "   "
			style := title
			if i == u.menuIndex {
				mark = " › "
				style = accent
			}
			put(3+i, mark+name, style)
		}
		put(u.height-1, " Esc  Back", muted)
	case "tree", "rename":
		heading, field, submit := " Create worktree", " Branch name", " Enter  Open"
		if u.mode == "rename" {
			heading, field, submit = " Rename terminal", " Terminal name", " Enter  Save"
		}
		put(0, heading, accent)
		put(2, field, muted)
		width := max(1, u.width-4)
		value := u.entry
		if ansi.StringWidth(value) > width {
			value = "…" + ansi.TruncateLeft(value, ansi.StringWidth(value)-width+1, "")
		}
		put(4, " › "+value+"▏", title)
		put(6, submit, accent)
		put(7, " Ctrl+u  Clear", muted)
		put(u.height-1, " Esc  Back", muted)
	case "close":
		put(0, " Close terminal "+cmp.Or(u.entry, filepath.Base(u.path)), title)
		put(2, " Stop its running program?", amber)
		put(4, " [y] Stop terminal", red)
		put(5, " [n] Cancel", muted)
		put(u.height-1, " Esc  Back", muted)
	default:
		heading := " REPOSITORIES"
		if u.agentMode {
			heading = " AGENTS"
			if len(u.rows) == 0 {
				put(3, " No agents running", muted)
			}
		}
		put(2, heading, muted)
		for i, y := range u.visibleRows() {
			r := u.rows[i]
			background := ""
			if i == u.cursor {
				background = "\x1b[48;5;237m"
			}
			putStyled(y, r.renderLabel(), r.style+background)
			if r.detail != "" && y+1 < u.footerDividerY() {
				put(y+1, r.detail, muted+background)
			}
		}
		noticeY = u.footerDividerY()
		put(noticeY, " "+strings.Repeat("─", max(0, u.width-2)), divider)
		for s := range u.shortcuts() {
			line := lines[s.y]
			gap := strings.Repeat(" ", s.x-ansi.StringWidth(line))
			line += gap + title + string(s.key) + reset + " " + muted + s.label + reset
			lines[s.y] = ansi.Truncate(line, u.width, "…")
		}
	}
	if u.notice != "" {
		put(noticeY, " "+u.notice, amber)
	}
	return strings.Join(lines, "\n")
}
func chunks(s string, n int) []string {
	var parts []string
	for len(s) > 0 {
		k := min(len(s), n)
		parts = append(parts, s[:k])
		s = s[k:]
	}
	return parts
}
