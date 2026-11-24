package main

import (
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"
)

type Terminal struct {
	ID        string   `json:"id"`
	CWD       string   `json:"cwd"`
	Checkout  Checkout `json:"checkout"`
	Program   string   `json:"program"`
	Title     string   `json:"title,omitempty"`
	ActiveRun *run     `json:"active_run,omitempty"`
	Status
}

func (t Terminal) hasAgent() bool {
	return t.State != "" && t.State != "shell" && t.State != "exited"
}

type Snapshot struct {
	AgentBell bool       `json:"agent_bell"`
	AgentMode bool       `json:"agent_mode"`
	Active    string     `json:"active"`
	Terminals []Terminal `json:"terminals"`
	Checkouts []Checkout `json:"checkouts"`
	Width     int        `json:"sidebar_width"`
	Endpoint  string     `json:"mcp_endpoint"`
}
type tracked struct {
	term               Terminal
	history            []Status
	claudePane         bool
	agent              *codexAgent
	notices            []conversationNotice
	noticeRevision     uint64
	missedNotices      bool
	conversationListed bool
}
type gitCache struct {
	value Checkout
	at    time.Time
}
type workspace struct {
	instance                             string
	lastSave                             []byte
	pending                              []savedPane
	mu                                   sync.Mutex
	p                                    runtimePaths
	sidebar, home, active, key, endpoint string
	width, windowWidth                   int
	manualWidth, agentMode, agentBell    bool
	terms                                map[string]*tracked
	runs                                 map[string]run
	pastRuns                             []string
	cache                                map[string]gitCache
	conversationWaiters                  int
	questions                            map[string]pendingQuestion
	changed                              chan struct{}
}

func newWorkspace(p runtimePaths, side string) *workspace {
	return &workspace{runs: map[string]run{}, agentBell: true, p: p, sidebar: side, terms: map[string]*tracked{}, cache: map[string]gitCache{}, changed: make(chan struct{})}
}

// The workspace lock serializes pane swaps, paste buffers and Git mutations.
func (w *workspace) notify() { close(w.changed); w.changed = make(chan struct{}) }
func (w *workspace) status(t *tracked, state, reason string) bool {
	if t.term.State == state && t.term.Reason == reason {
		return false
	}
	s := Status{state, reason, t.term.Revision + 1}
	hadAgent := t.term.hasAgent()
	t.term.Status = s
	if !hadAgent && t.term.hasAgent() {
		t.notices = nil
		t.missedNotices = false
		t.conversationListed = false
	}
	ended := hadAgent && !t.term.hasAgent()
	if ended || ((t.agent == nil || t.agent.active == "" || reason == "auth") && (statusEvent{State: state}).needsAttention()) {
		t.recordNotice(conversationNotice{Type: "terminal." + state, Subject: t.term.ID, Revision: s.Revision, Reason: reason})
	}
	t.history = append(t.history, s)
	if len(t.history) > 64 {
		t.history = slices.Clone(t.history[len(t.history)-64:])
	}
	w.notify()
	return true
}

// applyEvent records the transition and reports whether to ring the sidebar bell.
func (w *workspace) applyEvent(e statusEvent) bool {
	w.mu.Lock()
	defer w.mu.Unlock()
	if !slices.Contains(states, e.State) || !slices.Contains([]string{"", "auth", "permission", "question"}, e.Reason) {
		return false
	}
	if t := w.terms[e.Pane]; t != nil && t.term.State != "exited" {
		// A nested program cannot relabel the agent that owns this pane.
		if e.Program != "" {
			if e.Program != "claude" && e.Program != "codex" {
				return false
			}
			if (t.agent != nil || t.claudePane) && e.Program != t.term.Program {
				return false
			}
			t.term.Program = e.Program
		}
		changed := w.status(t, e.State, e.Reason)
		if changed {
			w.saveLocked()
		}
		return changed && w.agentBell && e.needsAttention()
	}
	return false
}
func (w *workspace) resolve(path string) Checkout {
	if c, ok := w.cache[path]; ok && time.Since(c.at) < 2*time.Second {
		return c.value
	}
	c, err := checkout(path)
	if err != nil {
		c = Checkout{Path: path, Repo: path}
	}
	w.cache[path] = gitCache{c, time.Now()}
	return c
}
func (w *workspace) refresh() error {
	w.mu.Lock()
	defer w.mu.Unlock()
	// The path is last so spaces and tabs in directory names stay intact.
	raw, err := w.p.tm("list-panes", "-a", "-F", "#{pane_id}\t#{pane_dead}\t#{pane_width}\t#{window_width}\t#{pane_current_command}\t#{s/[[:cntrl:]]/ /g:pane_title}\t#{pane_current_path}")
	if err != nil {
		return err
	}
	for _, line := range strings.Split(raw, "\n") {
		f := strings.SplitN(line, "\t", 7)
		if len(f) != 7 {
			continue
		}
		if f[0] == w.sidebar {
			width, _ := strconv.Atoi(f[2])
			window, _ := strconv.Atoi(f[3])
			if w.windowWidth == window && width != w.width {
				w.manualWidth = true
			}
			w.width, w.windowWidth = width, window
			continue
		}
		t := w.terms[f[0]]
		if t == nil {
			continue
		}
		if f[1] == "1" {
			w.status(t, "exited", "")
			if t.agent != nil {
				t.agent.close()
			}
			continue
		}
		if t.agent != nil && time.Now().After(t.agent.nextProbe) {
			t.agent.nextProbe = time.Now().Add(5 * time.Second)
			go w.discoverThread(t.agent)
		}
		if f[6] != "" {
			t.term.CWD = f[6]
			t.term.Checkout = w.resolve(f[6])
		}
		t.term.Title = clean(f[5])
		if t.agent == nil && !t.claudePane {
			command := f[4]
			switch {
			case isShell(command):
				// A killed process cannot send its final clear record.
				t.term.Program = command
				w.status(t, "shell", "")
			case command == "claude" || command == "codex":
				t.term.Program = command
				if t.term.State == "shell" {
					w.status(t, "unknown", "")
				}
			case !t.term.hasAgent():
				t.term.Program = command
			}
		}
	}
	if active := w.terms[w.active]; active != nil && active.term.State == "exited" {
		if err := w.focusNextLocked(active.term); err != nil {
			return err
		}
	}
	if !w.manualWidth && w.windowWidth >= 30 {
		desired := autoSidebarWidth(w.snapshotLocked(), w.windowWidth)
		if desired != w.width {
			if _, err := w.p.tm("resize-pane", "-t", w.sidebar, "-x", strconv.Itoa(desired)); err != nil {
				return err
			}
			w.width = desired
		}
	}
	return w.saveLocked()
}
func isShell(name string) bool {
	return slices.Contains([]string{"bash", "zsh", "fish", "sh", "dash", "ksh", "nu", "tcsh", "csh"}, name)
}

func (w *workspace) setAgentBell(enabled bool) error {
	w.mu.Lock()
	defer w.mu.Unlock()
	if err := writePrivate(w.p.agentBellPath(), []byte(strconv.FormatBool(enabled)+"\n")); err != nil {
		return err
	}
	w.agentBell = enabled
	return nil
}

func (w *workspace) setAgentMode(enabled bool) {
	w.mu.Lock()
	defer w.mu.Unlock()
	w.agentMode = enabled
	w.saveLocked()
}

func (w *workspace) snapshot() Snapshot { w.mu.Lock(); defer w.mu.Unlock(); return w.snapshotLocked() }
func (w *workspace) snapshotLocked() Snapshot {
	s := Snapshot{AgentBell: w.agentBell, AgentMode: w.agentMode, Active: w.active, Width: w.width, Endpoint: w.endpoint, Terminals: []Terminal{}, Checkouts: []Checkout{}}
	all := map[string]Checkout{}
	for _, t := range w.terms {
		term := t.term
		if t.agent != nil && t.agent.active != "" {
			r := w.runs[t.agent.active]
			term.ActiveRun = &r
		}
		s.Terminals = append(s.Terminals, term)
		if t.term.State != "exited" {
			all[t.term.Checkout.Path] = t.term.Checkout
		}
	}
	for _, c := range all {
		s.Checkouts = append(s.Checkouts, c)
	}
	sort.Slice(s.Terminals, func(i, j int) bool {
		a, _ := strconv.Atoi(s.Terminals[i].ID[1:])
		b, _ := strconv.Atoi(s.Terminals[j].ID[1:])
		return a < b
	})
	sort.Slice(s.Checkouts, func(i, j int) bool {
		a, b := s.Checkouts[i], s.Checkouts[j]
		if a.Repo != b.Repo {
			return a.Repo < b.Repo
		}
		if a.Path == b.Path {
			return false
		}
		if a.Path == a.Repo {
			return true
		}
		if b.Path == b.Repo {
			return false
		}
		return a.Path < b.Path
	})
	return s
}
func (w *workspace) lookup(id string) (*tracked, error) {
	t := w.terms[id]
	if t == nil {
		return nil, fmt.Errorf("unknown terminal %q", id)
	}
	return t, nil
}

func (w *workspace) selectLocked(id string, focus bool) error {
	if id != w.home {
		t, err := w.lookup(id)
		if err != nil {
			return err
		}
		if t.term.State == "exited" {
			return errors.New("terminal has exited")
		}
	}
	current := w.active
	if current == "" {
		current = w.home
	}
	if current != id {
		if _, err := w.p.tm("swap-pane", "-d", "-s", id, "-t", current); err != nil {
			return err
		}
	}
	if id == w.home {
		w.active = ""
	} else {
		w.active = id
	}
	if focus {
		_, err := w.p.tm("select-pane", "-t", id)
		return err
	}
	return nil
}

type launchProgram string

const (
	launchShell  launchProgram = ""
	launchCodex  launchProgram = "codex"
	launchClaude launchProgram = "claude"
)

func (w *workspace) open(path string, program launchProgram, repo bool) (Terminal, error) {
	w.mu.Lock()
	defer w.mu.Unlock()
	path, err := directory(path)
	if err != nil {
		return Terminal{}, err
	}
	if repo {
		c, err := checkout(path)
		if err != nil {
			return Terminal{}, err
		}
		path = c.Path
	}
	return w.spawn(path, program)
}
func (w *workspace) spawn(path string, program launchProgram) (Terminal, error) {
	return w.spawnTerminal(path, program, "", false)
}
func (w *workspace) spawnTerminal(path string, program launchProgram, thread string, picker bool) (Terminal, error) {
	prog := os.Getenv("SHELL")
	if prog == "" {
		prog = "/bin/sh"
	}
	if program != launchShell {
		prog = string(program)
	} else if picker {
		prog = "codex"
	}
	exe, err := exec.LookPath(prog)
	if err != nil {
		return Terminal{}, err
	}
	c := w.resolve(path)
	// Install the status reader before releasing the process to write its first byte.
	gate := "start-" + randomID(10)
	args := []string{"new-window", "-d", "-P", "-F", "#{pane_id}", "-t", "kowloon:", w.p.exe, "_run", gate}
	var agent *codexAgent
	if program == launchCodex {
		dir, err := os.MkdirTemp(w.p.dir, "a-")
		if err != nil {
			return Terminal{}, err
		}
		agent = newCodexAgent(dir, path, c.Path)
		agent.thread = thread
		args = append(args, w.p.exe, "_codex", dir, exe, c.Path, thread)
	} else {
		args = append(args, exe)
		if program == launchClaude {
			settings, err := claudeSettings(w.p.exe)
			if err != nil {
				return Terminal{}, err
			}
			args = append(args, "--settings", settings)
			if picker {
				args = append(args, "--resume")
			}
		} else if picker {
			args = append(args, "resume")
		}
	}
	id, err := w.p.tmInDir(path, args...)
	if err != nil {
		if agent != nil {
			agent.close()
		}
		return Terminal{}, err
	}
	failed := true
	defer func() {
		if failed {
			if agent != nil {
				agent.close()
			}
			delete(w.terms, id)
			w.p.tm("kill-pane", "-t", id)
		}
	}()
	if _, err = w.p.tm("select-pane", "-t", id, "-T", ""); err != nil {
		return Terminal{}, err
	}
	_, err = w.p.tm("pipe-pane", "-O", "-t", id, watcherCommand(w.p.exe, id))
	if err != nil {
		return Terminal{}, err
	}
	t := &tracked{term: Terminal{ID: id, CWD: path, Checkout: c, Program: filepath.Base(exe)}, agent: agent, claudePane: program == launchClaude}
	w.terms[id] = t
	s := "shell"
	if program != launchShell {
		s = "starting"
	}
	if _, err = w.p.tm("wait-for", "-S", gate); err != nil {
		return Terminal{}, err
	}
	if err = w.selectLocked(id, true); err != nil {
		return Terminal{}, err
	}
	w.status(t, s, "")
	failed = false
	return t.term, w.saveLocked()
}
func (w *workspace) addTree(path, branch string) (Terminal, error) {
	w.mu.Lock()
	defer w.mu.Unlock()
	c, err := createTree(w.p.data, path, branch)
	if err != nil {
		return Terminal{}, err
	}
	return w.spawn(c.Path, launchShell)
}
func (w *workspace) selectTerm(id string) error {
	w.mu.Lock()
	defer w.mu.Unlock()
	if err := w.selectLocked(id, true); err != nil {
		return err
	}
	return w.saveLocked()
}

// Prefer another terminal in the same repository when the visible shell ends.
func (w *workspace) focusNextLocked(leaving Terminal) error {
	next := w.home
	for _, t := range w.snapshotLocked().Terminals {
		if t.ID == leaving.ID || t.State == "exited" {
			continue
		}
		if t.Checkout.Repo == leaving.Checkout.Repo {
			next = t.ID
			break
		}
		if next == w.home {
			next = t.ID
		}
	}
	if err := w.selectLocked(next, next != w.home); err != nil {
		return err
	}
	if next == w.home {
		_, err := w.p.tm("select-pane", "-t", w.sidebar)
		return err
	}
	return nil
}

func (w *workspace) closeTerm(id string) error {
	w.mu.Lock()
	defer w.mu.Unlock()
	t, err := w.lookup(id)
	if err != nil {
		return err
	}
	if w.active == id {
		if err := w.focusNextLocked(t.term); err != nil {
			return err
		}
	}
	if _, err := w.p.tm("kill-pane", "-t", id); err != nil {
		return err
	}
	w.status(t, "exited", "")
	if t.agent != nil {
		t.agent.close()
	}
	delete(w.terms, id)
	w.notify()
	return w.saveLocked()
}
func (w *workspace) read(id string, lines int) (string, error) {
	w.mu.Lock()
	defer w.mu.Unlock()
	if _, err := w.lookup(id); err != nil {
		return "", err
	}
	if lines < 0 || lines > 20000 {
		return "", errors.New("lines must be between 0 and 20000")
	}
	args := []string{"capture-pane", "-p", "-t", id}
	if lines > 0 {
		args = append(args, "-S", strconv.Itoa(-lines))
	}
	return w.p.tm(args...)
}

var keys = []string{"Enter", "Escape", "Up", "Down", "Left", "Right", "Tab", "BTab", "Space", "BSpace", "C-c", "C-d", "C-l", "Home", "End", "PageUp", "PageDown"}

func (w *workspace) input(id, text, key string) error {
	w.mu.Lock()
	defer w.mu.Unlock()
	t, err := w.lookup(id)
	if err != nil {
		return err
	}
	if t.term.State == "exited" {
		return errors.New("terminal has exited")
	}
	if key != "" && !slices.Contains(keys, key) {
		return fmt.Errorf("choose a key from %s", strings.Join(keys, ", "))
	}
	if text == "" && key == "" {
		return errors.New("supply text or a key")
	}
	if text != "" {
		buf := "input-" + randomID(8)
		if _, err := w.p.tmInput(text, "load-buffer", "-b", buf, "-"); err != nil {
			return err
		}
		if _, err := w.p.tm("paste-buffer", "-p", "-d", "-b", buf, "-t", id); err != nil {
			w.p.tm("delete-buffer", "-b", buf)
			return err
		}
	}
	if key != "" {
		_, err = w.p.tm("send-keys", "-t", id, key)
	}
	return err
}
func (w *workspace) resize(width int) error {
	w.mu.Lock()
	defer w.mu.Unlock()
	total, err := w.p.tm("display-message", "-p", "-t", w.sidebar, "#{window_width}")
	if err != nil {
		return err
	}
	n, _ := strconv.Atoi(total)
	if width < 12 || width > n-12 {
		return fmt.Errorf("choose a width between 12 and %d", n-12)
	}
	_, err = w.p.tm("resize-pane", "-t", w.sidebar, "-x", strconv.Itoa(width))
	if err == nil {
		w.width = width
		w.manualWidth = true
		w.width = width
		return w.saveLocked()
	}
	return err
}
