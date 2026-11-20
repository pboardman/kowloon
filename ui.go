package main

import (
	"path/filepath"
	"strings"
	"time"
	"unicode"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/x/ansi"
)

const (
	muted  = "\x1b[38;5;246m"
	accent = "\x1b[1;38;5;79m"
	title  = "\x1b[1;38;5;252m"
	amber  = "\x1b[1;38;5;221m"
	red    = "\x1b[1;38;5;210m"
	blue   = "\x1b[1;38;5;117m"
	reset  = "\x1b[0m"
)

type row struct{ key, path, id, label, style string }
type tick time.Time
type ui struct {
	w                                        *workspace
	width, height, cursor, offset, menuIndex int
	rows                                     []row
	active, mode, entry, notice, path, id    string
	reveal                                   bool
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
func badge(t Terminal) (string, string) {
	switch t.State {
	case "working":
		return "● WORKING", blue
	case "waiting":
		return "? QUESTION", amber
	case "blocked":
		if t.Reason == "auth" {
			return "! SIGN IN", red
		}
		if t.Reason == "permission" {
			return "! APPROVAL", amber
		}
		return "! BLOCKED", red
	case "done":
		return "✓ DONE", accent
	case "error":
		return "! ERROR", red
	case "idle":
		return "○ IDLE", muted
	case "starting":
		return "◌ STARTING", blue
	case "unknown":
		return "◌ UNKNOWN", muted
	}
	return "", muted
}
func (u *ui) rebuild() {
	s := u.w.snapshot()
	activeChanged := u.active != s.Active
	u.active = s.Active
	old := ""
	if u.cursor >= 0 && u.cursor < len(u.rows) {
		old = u.rows[u.cursor].key
	}
	rows := []row{}
	last := ""
	for _, c := range s.Checkouts {
		if c.Repo != last {
			last = c.Repo
			label := "▾ " + filepath.Base(c.Repo)
			if c.Path == c.Repo && c.Branch != "" {
				label += "  · " + c.Branch
			}
			rows = append(rows, row{"repo:" + c.Repo, c.Repo, "", label, title})
		}
		indent := "  "
		if c.Path != c.Repo {
			rows = append(rows, row{"tree:" + c.Path, c.Path, "", "  └ " + c.Branch, accent})
			indent = "    "
		}
		for _, t := range s.Terminals {
			if t.State == "exited" || t.Checkout.Path != c.Path {
				continue
			}
			status, color := badge(t)
			name := t.Program
			if status != "" {
				name = "Codex"
			}
			marker := " "
			if t.ID == s.Active {
				marker = "›"
			}
			label := marker + indent
			if status != "" {
				label += status + "  "
			}
			label += name
			if status != "" && t.Title != "" {
				// Codex places the project last in its default title. The tree already names it.
				task := strings.TrimSuffix(t.Title, " | "+filepath.Base(c.Path))
				if task != filepath.Base(c.Path) && task != "" {
					label += " · " + task
				}
			}
			rows = append(rows, row{t.ID, c.Path, t.ID, label, color})
		}
	}
	u.rows = rows
	for i, r := range rows {
		if (activeChanged && r.id != "" && r.id == s.Active) || (!activeChanged && r.key == old) {
			u.cursor = i
			break
		}
	}
	u.scroll()
}
func (u *ui) scroll() {
	u.cursor = max(0, min(u.cursor, len(u.rows)-1))
	count := max(1, u.height-7)
	u.offset = max(0, min(u.offset, len(u.rows)-count))
	if u.cursor < u.offset {
		u.offset = u.cursor
	}
	if u.cursor >= u.offset+count {
		u.offset = u.cursor - count + 1
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
	_, err := u.w.open("~", false, false)
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
	u.menuIndex = 0
	u.notice = ""
}
func (u *ui) choices() []string {
	choices := []string{"New terminal", "Create worktree"}
	if u.id != "" {
		choices = append(choices, "Close terminal")
	}
	return choices
}
func (u *ui) action(i int) {
	u.mode = ""
	var err error
	switch i {
	case 0:
		_, err = u.w.open(u.path, false, false)
	case 1:
		u.promptWorktree(u.path)
	case 2:
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
			_, err := u.w.open(r.path, false, false)
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
	if key == "esc" {
		u.mode = ""
		u.notice = ""
		u.reveal = false
		return
	}
	switch u.mode {
	case "settings":
		switch key {
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
	case "tree":
		switch key {
		case "ctrl+u":
			u.entry = ""
		case "backspace", "ctrl+h":
			r := []rune(u.entry)
			if len(r) > 0 {
				u.entry = string(r[:len(r)-1])
			}
		case "enter":
			_, err := u.w.addTree(u.path, u.entry)
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
	case "tree":
		if m.Y == u.height-1 {
			u.mode = ""
		}
		return
	}
	if m.Y == 0 && m.Button == tea.MouseButtonLeft && m.X >= u.newButtonColumn() {
		u.newTerminal()
		return
	}
	if m.Y == u.height-1 {
		u.mode = "settings"
		u.notice = ""
		u.reveal = false
		return
	}
	i := u.offset + m.Y - 3
	if m.Y >= 3 && m.Y < u.height-4 && i >= 0 && i < len(u.rows) {
		u.cursor = i
		if m.Button == tea.MouseButtonRight {
			u.menu(u.rows[i])
		} else {
			u.activate()
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
func (u *ui) View() string {
	if u.height < 1 || u.width < 1 {
		return ""
	}
	lines := make([]string, u.height)
	put := func(y int, s, color string) {
		if y >= 0 && y < len(lines) {
			lines[y] = color + ansi.Truncate(clean(s), u.width, "…") + reset
		}
	}
	put(0, " KOWLOON"+strings.Repeat(" ", u.newButtonColumn()-len(" KOWLOON"))+"+ New ", accent)
	put(1, strings.Repeat("─", u.width), muted)
	footer := " s  Settings"
	put(u.height-1, footer, muted)
	switch u.mode {
	case "settings":
		put(0, " Settings", title)
		put(2, " MCP access key", accent)
		show := "Reveal key"
		if u.reveal {
			show = "Hide key"
		}
		put(3, " [r] "+show, title)
		put(4, " [c] Copy key", title)
		y := 6
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
	case "tree":
		put(0, " Create worktree", accent)
		put(2, " Branch name", muted)
		width := max(1, u.width-4)
		value := u.entry
		if ansi.StringWidth(value) > width {
			value = "…" + ansi.TruncateLeft(value, ansi.StringWidth(value)-width+1, "")
		}
		put(4, " › "+value+"▏", title)
		put(6, " Enter  Open", accent)
		put(7, " Ctrl+u  Clear", muted)
		put(u.height-1, " Esc  Back", muted)
	case "close":
		put(0, " Close terminal "+u.id, title)
		put(2, " Stop its running program?", amber)
		put(4, " [y] Stop terminal", red)
		put(5, " [n] Cancel", muted)
		put(u.height-1, " Esc  Back", muted)
	default:
		put(2, " REPOSITORIES", muted)
		for i, r := range u.rows {
			y := 3 + i - u.offset
			if y < 3 || y >= u.height-4 {
				continue
			}
			style := r.style
			if i == u.cursor {
				style += "\x1b[48;5;237m"
			}
			put(y, r.label, style)
		}
		put(u.height-3, " n New   m Actions", muted)
		put(u.height-2, " w Worktree", muted)
	}
	if u.notice != "" {
		put(u.height-2, " "+u.notice, amber)
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
