package app

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/charmbracelet/x/ansi"
	"golang.org/x/term"
)

// Store only the information needed to recover. Terminal output, command lines,
// prompts, environment variables, and setup approvals are not part of this file.
type savedPane struct {
	Name         string `json:",omitempty"`
	ID           string
	CWD          string
	Status       Status
	History      []Status `json:",omitempty"`
	AgentDir     string   `json:",omitempty"` // basename in the private runtime directory
	AgentCWD     string   `json:",omitempty"`
	Thread       string   `json:",omitempty"`
	ManualClaude bool     `json:",omitempty"`
	ClaudePane   bool     `json:",omitempty"`
	ManualCodex  bool     `json:",omitempty"`
}
type savedRun struct {
	Run        run
	RequestID  string `json:",omitempty"`
	PromptHash [32]byte
}
type savedSession struct {
	Version                int
	Instance               string
	Panes                  []savedPane
	Pending                []savedPane `json:",omitempty"`
	Active                 string
	AgentMode, ManualWidth bool
	Width                  int
	Runs                   map[string]savedRun `json:",omitempty"`
	PastRuns               []string            `json:",omitempty"`
}

var agentDirName = regexp.MustCompile(`^a-[0-9]+$`)

const resumePicker = "picker"

var threadUUID = regexp.MustCompile(`^[[:xdigit:]]{8}-[[:xdigit:]]{4}-[[:xdigit:]]{4}-[[:xdigit:]]{4}-[[:xdigit:]]{12}$`)

func (p runtimePaths) sessionPath() string { return filepath.Join(p.config, "session.json") }

func (p runtimePaths) readSession() (savedSession, error) {
	var s savedSession
	data, err := os.ReadFile(p.sessionPath())
	if errors.Is(err, os.ErrNotExist) {
		return s, nil
	}
	if err != nil {
		return s, err
	}
	if err = json.Unmarshal(data, &s); err != nil {
		return s, fmt.Errorf("read saved Kowloon workspace: %w", err)
	}
	if s.Version != 1 {
		return s, fmt.Errorf("unsupported Kowloon session version %d", s.Version)
	}
	for _, group := range [][]savedPane{s.Panes, s.Pending} {
		for _, pane := range group {
			if !filepath.IsAbs(pane.CWD) || (pane.AgentDir != "" && (!agentDirName.MatchString(pane.AgentDir) || !filepath.IsAbs(pane.AgentCWD))) || (pane.Thread != "" && pane.Thread != resumePicker && !threadUUID.MatchString(pane.Thread)) {
				return s, errors.New("invalid saved Kowloon terminal")
			}
		}
	}
	return s, nil
}

// Caller owns w.mu, or is the sole owner during startup. The empty instance
// disables writes until all panes have been recovered successfully.
func (w *workspace) saveLocked() error {
	if w.instance == "" {
		return nil
	}
	s := savedSession{Version: 1, Instance: w.instance, Active: w.active, Width: w.width, AgentMode: w.agentMode, ManualWidth: w.manualWidth, PastRuns: w.pastRuns, Pending: w.pending}
	for _, t := range w.terms {
		if t.term.State == "exited" {
			continue
		}
		p := savedPane{
			ID: t.term.ID, Name: t.term.Name, CWD: t.term.CWD, Status: t.term.Status, History: t.history,
			ClaudePane:   t.claudePane,
			ManualClaude: !t.claudePane && t.term.hasAgent() && t.term.Program == "claude",
			ManualCodex:  t.term.Program == "codex",
		}
		if a := t.agent; a != nil {
			p.AgentDir = filepath.Base(a.dir)
			p.AgentCWD = a.cwd
			p.Thread = a.thread
			p.ManualCodex = false
		}
		s.Panes = append(s.Panes, p)
	}
	sort.Slice(s.Panes, func(i, j int) bool {
		a, _ := strconv.Atoi(strings.TrimPrefix(s.Panes[i].ID, "%"))
		b, _ := strconv.Atoi(strings.TrimPrefix(s.Panes[j].ID, "%"))
		return a < b
	})
	if len(w.runs) > 0 {
		s.Runs = make(map[string]savedRun, len(w.runs))
		for id, r := range w.runs {
			s.Runs[id] = savedRun{r, r.requestID, r.promptHash}
		}
	}
	raw, err := json.Marshal(s)
	if err != nil || bytes.Equal(raw, w.lastSave) {
		return err
	}
	err = publishPrivate(w.p.sessionPath(), raw, func(from, to string) error {
		f, err := os.OpenFile(from, os.O_RDWR, 0)
		if err != nil {
			return err
		}
		err = f.Sync()
		f.Close()
		if err != nil {
			return err
		}
		if err = os.Rename(from, to); err != nil {
			return err
		}
		d, err := os.Open(filepath.Dir(to))
		if err != nil {
			return err
		}
		defer d.Close()
		return d.Sync()
	})
	if err != nil {
		return fmt.Errorf("save Kowloon workspace: %w", err)
	}
	w.lastSave = raw
	return nil
}

func (w *workspace) discoverThread(a *codexAgent) {
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	c, err := dialCodex(ctx, a.dir)
	if err != nil {
		return
	}
	defer c.conn.Close()
	t, err := c.rootThread(ctx, a.cwd)
	w.mu.Lock()
	defer w.mu.Unlock()
	select {
	case <-a.stop:
		return
	default:
	}
	thread := t.ID
	if errors.Is(err, errAmbiguousThread) {
		thread = resumePicker
	} else if err != nil || !threadUUID.MatchString(thread) {
		return
	}
	if a.thread != thread {
		a.thread = thread
		w.saveLocked()
	}
}

// Reconnect to panes in the surviving tmux session. Their real locations
// determine the visible terminal even if a swap raced the last save.
func (w *workspace) adoptPanes(s savedSession) (map[string]string, error) {
	layout, err := w.p.tm("list-panes", "-s", "-t", "=kowloon", "-F", "#{pane_id}\t#{window_id}\t#{pane_dead}\t#{pane_start_command}\t#{pane_current_path}")
	if err != nil {
		return nil, err
	}
	window, err := w.p.tm("display-message", "-p", "-t", w.sidebar, "#{window_id}")
	if err != nil {
		return nil, err
	}
	home, err := w.p.tm("show-options", "-qv", "-t", "kowloon", "@kowloon_home")
	if err != nil {
		return nil, err
	}
	w.home = ""
	old := map[string]savedPane{}
	for _, p := range s.Panes {
		old[p.ID] = p
	}
	ids := map[string]string{}
	for _, line := range strings.Split(layout, "\n") {
		f := strings.SplitN(line, "\t", 5)
		if len(f) != 5 || f[0] == w.sidebar {
			continue
		}
		id, cmd, cwd := f[0], f[3], f[4]
		if id == home || (home == "" && strings.HasSuffix(cmd, " _empty")) {
			w.home = id
			continue
		}
		p := old[id]
		t := &tracked{term: Terminal{ID: id, Name: p.Name, CWD: cwd, Checkout: w.resolve(cwd), Status: p.Status}, history: p.History}
		if p.ClaudePane || p.ManualClaude {
			t.term.Program = "claude"
			t.claudePane = p.ClaudePane
		}
		if t.term.State == "" {
			t.term.State = "shell"
		}
		if f[2] == "1" {
			t.term.State = "exited"
		} else {
			// Older versions did not save a manifest. Their managed pane launch
			// command still identifies the private socket directory.
			if p.AgentDir == "" && strings.Contains(cmd, " _codex ") {
				dirs, _ := filepath.Glob(filepath.Join(w.p.dir, "a-*"))
				for _, dir := range dirs {
					if strings.Contains(cmd, dir+" ") || strings.Contains(cmd, dir+"\" ") {
						p.AgentDir = filepath.Base(dir)
						p.AgentCWD = cwd
						break
					}
				}
			}
			if p.AgentDir != "" && agentDirName.MatchString(p.AgentDir) {
				t.agent = newCodexAgent(filepath.Join(w.p.dir, p.AgentDir), p.AgentCWD, w.resolve(p.AgentCWD).Path)
				t.agent.thread = p.Thread
				t.term.Program = "codex"
				if t.term.State == "shell" {
					t.term.State = "unknown"
				}
			}
			if _, err = w.p.tm("pipe-pane", "-O", "-t", id, watcherCommand(w.p.exe, id)); err != nil {
				return nil, err
			}
		}
		w.terms[id] = t
		ids[id] = id
		if f[1] == window {
			w.active = id
		}
	}
	if w.home == "" {
		return nil, errors.New("could not identify the Kowloon home pane in tmux")
	}
	return ids, nil
}

func (w *workspace) restorePanes(s savedSession) (map[string]string, error) {
	ids := map[string]string{}
	for _, p := range append(s.Panes, s.Pending...) {
		path := p.CWD
		if p.AgentDir != "" {
			path = p.AgentCWD
		}
		if _, err := directory(path); err != nil {
			p.ID = "" // a new tmux server can reuse the old pane number
			w.pending = append(w.pending, p)
			continue
		}
		program := launchShell
		if p.AgentDir != "" {
			program = launchCodex
		} else if p.ClaudePane || p.ManualClaude {
			program = launchClaude
		}
		term, err := w.spawnTerminal(path, program, p.Thread, p.ManualCodex || program == launchClaude)
		if errors.Is(err, exec.ErrNotFound) {
			p.ID = ""
			w.pending = append(w.pending, p)
			continue
		}
		if err != nil {
			return nil, err
		}
		w.terms[term.ID].term.Name = p.Name
		if p.ID != "" {
			ids[p.ID] = term.ID
		}
	}
	if id := ids[s.Active]; id != "" {
		if err := w.selectLocked(id, true); err != nil {
			return nil, err
		}
	}
	return ids, nil
}

func (w *workspace) restoreRuns(s savedSession, ids map[string]string, live bool) {
	w.pastRuns = append(w.pastRuns, s.PastRuns...)
	finished := map[string]bool{}
	for _, id := range w.pastRuns {
		finished[id] = true
	}
	for id, entry := range s.Runs {
		r := entry.Run
		r.requestID, r.promptHash = entry.RequestID, entry.PromptHash
		if !live {
			r.Terminal = ids[r.Terminal]
		}
		w.runs[id] = r
		if finished[id] {
			continue
		}
		t := w.terms[r.Terminal]
		if live && t != nil && t.agent != nil && t.term.State != "exited" {
			t.agent.active = id
			continue
		}
		r.Status = "unavailable"
		r.Revision++
		w.runs[id] = r
		w.pastRuns = append(w.pastRuns, id)
	}
	for len(w.pastRuns) > maxPastRuns {
		delete(w.runs, w.pastRuns[0])
		w.pastRuns = w.pastRuns[1:]
	}
}

// Restore into an empty tmux workspace or take over a surviving controller pane.
func (w *workspace) recoverSession(path, mode string) error {
	p := w.p
	width, _, err := term.GetSize(int(os.Stdout.Fd()))
	if err != nil {
		return err
	}
	// Size the initial sidebar for the repository and branch while keeping the terminal larger.
	desired := 28
	if path != "" {
		c := w.resolve(path)
		desired = max(desired, ansi.StringWidth(filepath.Base(c.Repo))+ansi.StringWidth(c.Branch)+7)
	}
	target := min(desired, max(12, width*2/5))
	w.width = target
	var saved savedSession
	if mode != "fresh" {
		saved, err = p.readSession()
		if err != nil {
			return fmt.Errorf("%w (use --no-restore for a fresh workspace)", err)
		}
	}
	instance, err := p.tm("show-options", "-qv", "-t", "kowloon", "@kowloon_instance")
	if err != nil {
		return err
	}
	live := mode == "live"
	if live && instance != saved.Instance {
		saved = savedSession{}
	}
	if saved.Version != 0 {
		w.agentMode = saved.AgentMode
		w.manualWidth = saved.ManualWidth
	}
	var ids map[string]string
	if live {
		w.pending = saved.Pending
		ids, err = w.adoptPanes(saved)
	} else {
		if saved.ManualWidth {
			w.width = min(max(12, saved.Width), max(12, width-12))
		}
		w.home, err = p.tm("split-window", "-d", "-h", "-l", strconv.Itoa(max(1, width-w.width-1)), "-P", "-F", "#{pane_id}", "-t", w.sidebar, p.exe, "_empty")
		if err == nil {
			ids, err = w.restorePanes(saved)
		}
	}
	if err != nil {
		return err
	}
	w.restoreRuns(saved, ids, live)
	if path != "" {
		if _, err = w.open(path, launchShell, false); err != nil {
			return err
		}
	}
	if instance == "" {
		instance = randomID(16)
	}
	if _, err = p.tm("set-option", "-t", "kowloon", "@kowloon_instance", instance); err != nil {
		return err
	}
	if _, err = p.tm("set-option", "-t", "kowloon", "@kowloon_home", w.home); err != nil {
		return err
	}
	w.instance = instance
	if err = w.refresh(); err != nil {
		return err
	}
	w.mu.Lock()
	for _, t := range w.terms {
		if t.agent != nil && t.agent.active != "" {
			go w.monitorRun(t.agent, w.runs[t.agent.active])
		}
	}
	w.mu.Unlock()
	return nil
}
