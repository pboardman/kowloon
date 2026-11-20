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

	"github.com/charmbracelet/x/ansi"
)

type Terminal struct {
	ID       string   `json:"id"`
	CWD      string   `json:"cwd"`
	Checkout Checkout `json:"checkout"`
	Program  string   `json:"program"`
	Title    string   `json:"title,omitempty"`
	Status
}
type Snapshot struct {
	Active    string     `json:"active"`
	Terminals []Terminal `json:"terminals"`
	Checkouts []Checkout `json:"checkouts"`
	Width     int        `json:"sidebar_width"`
	Endpoint  string     `json:"mcp_endpoint"`
}
type tracked struct {
	term    Terminal
	history []Status
	managed bool
}
type gitCache struct {
	value Checkout
	at    time.Time
}
type workspace struct {
	mu                                   sync.Mutex
	p                                    runtimePaths
	sidebar, home, active, key, endpoint string
	width, windowWidth                   int
	manualWidth                          bool
	terms                                map[string]*tracked
	cache                                map[string]gitCache
	changed                              chan struct{}
}

func newWorkspace(p runtimePaths, side string) *workspace {
	return &workspace{p: p, sidebar: side, terms: map[string]*tracked{}, cache: map[string]gitCache{}, changed: make(chan struct{})}
}

// The workspace lock serializes pane swaps, paste buffers and Git mutations.
func (w *workspace) notify() { close(w.changed); w.changed = make(chan struct{}) }
func (w *workspace) status(t *tracked, state, reason string) {
	if t.term.State == state && t.term.Reason == reason {
		return
	}
	s := Status{state, reason, t.term.Revision + 1}
	t.term.Status = s
	t.history = append(t.history, s)
	if len(t.history) > 64 {
		t.history = slices.Clone(t.history[len(t.history)-64:])
	}
	w.notify()
}
func (w *workspace) event(e statusEvent) {
	w.mu.Lock()
	defer w.mu.Unlock()
	if t := w.terms[e.Pane]; t != nil && t.term.State != "exited" {
		w.status(t, e.State, e.Reason)
	}
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
			continue
		}
		if f[6] != "" {
			t.term.CWD = f[6]
			t.term.Checkout = w.resolve(f[6])
		}
		t.term.Program = f[4]
		t.term.Title = clean(f[5])
		// A killed process cannot send its final clear record.
		if t.term.State != "shell" && !t.managed && isShell(f[4]) {
			w.status(t, "shell", "")
		}
		if t.term.State == "shell" && f[4] == "codex" {
			w.status(t, "unknown", "")
		}
	}
	if active := w.terms[w.active]; active != nil && active.term.State == "exited" {
		if err := w.focusNextLocked(active.term); err != nil {
			return err
		}
	}
	if !w.manualWidth && w.windowWidth >= 30 {
		desired := 28
		for _, c := range w.snapshotLocked().Checkouts {
			label := c.Branch
			if c.Repo == c.Path {
				label = filepath.Base(c.Repo) + "  · " + label
			}
			desired = max(desired, ansi.StringWidth(label)+4)
		}
		desired = min(desired, w.windowWidth*2/5)
		if desired != w.width {
			if _, err := w.p.tm("resize-pane", "-t", w.sidebar, "-x", strconv.Itoa(desired)); err != nil {
				return err
			}
			w.width = desired
		}
	}
	return nil
}
func isShell(name string) bool {
	return slices.Contains([]string{"bash", "zsh", "fish", "sh", "dash", "ksh", "nu", "tcsh", "csh"}, name)
}

func (w *workspace) snapshot() Snapshot { w.mu.Lock(); defer w.mu.Unlock(); return w.snapshotLocked() }
func (w *workspace) snapshotLocked() Snapshot {
	s := Snapshot{Active: w.active, Width: w.width, Endpoint: w.endpoint, Terminals: []Terminal{}, Checkouts: []Checkout{}}
	all := map[string]Checkout{}
	for _, t := range w.terms {
		s.Terminals = append(s.Terminals, t.term)
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
func (w *workspace) open(path string, codex, repo bool) (Terminal, error) {
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
	return w.spawn(path, codex)
}
func (w *workspace) spawn(path string, codex bool) (Terminal, error) {
	prog := os.Getenv("SHELL")
	if prog == "" {
		prog = "/bin/sh"
	}
	if codex {
		prog = "codex"
	}
	exe, err := exec.LookPath(prog)
	if err != nil {
		return Terminal{}, err
	}
	c := w.resolve(path)
	// Install the status reader before releasing the process to write its first byte.
	gate := "start-" + randomID(10)
	id, err := w.p.tm("new-window", "-d", "-P", "-F", "#{pane_id}", "-t", "kowloon:", "-c", path, w.p.exe, "_run", gate, exe)
	if err != nil {
		return Terminal{}, err
	}
	failed := true
	defer func() {
		if failed {
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
	t := &tracked{term: Terminal{ID: id, CWD: path, Checkout: c, Program: filepath.Base(exe)}, managed: codex}
	w.terms[id] = t
	s := "shell"
	if codex {
		s = "starting"
	}
	w.status(t, s, "")
	if _, err = w.p.tm("wait-for", "-S", gate); err != nil {
		return Terminal{}, err
	}
	if err = w.selectLocked(id, true); err != nil {
		return Terminal{}, err
	}
	failed = false
	return t.term, nil
}
func (w *workspace) addTree(path, branch string) (Terminal, error) {
	w.mu.Lock()
	defer w.mu.Unlock()
	c, err := createTree(w.p.data, path, branch)
	if err != nil {
		return Terminal{}, err
	}
	return w.spawn(c.Path, false)
}
func (w *workspace) selectTerm(id string) error {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.selectLocked(id, true)
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
	delete(w.terms, id)
	w.notify()
	return nil
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
	}
	return err
}
