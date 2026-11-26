package app

import (
	"context"
	"encoding/json"
	"os"
	"os/exec"
	"regexp"
	"strconv"
	"strings"
	"time"
)

// Native installs can appear in tmux under the versioned executable name.
var claudeCommand = regexp.MustCompile(`^(claude|node|[0-9]+\.[0-9]+\.[0-9]+)$`)

type claudeSession struct {
	PID        int
	Kind       string
	Status     string
	WaitingFor string
}

func (s claudeSession) event() statusEvent {
	e := statusEvent{Program: "claude"}
	switch s.Status {
	case "busy":
		e.State = "working"
	case "idle":
		e.State = "idle"
	case "waiting":
		e.State = "blocked"
		switch s.WaitingFor {
		case "input needed":
			e.State, e.Reason = "waiting", "question"
		case "permission prompt", "sandbox request":
			e.Reason = "permission"
		}
	}
	return e
}

// A plain shell launch has no Kowloon hooks, and Claude's terminal status
// handshake normally fails inside tmux. Match its public session list to the
// foreground processes on our PTYs. Several sessions can share a directory.
func claudeEventsByTTY(ctx context.Context, dir string) map[string]statusEvent {
	cmd := exec.CommandContext(ctx, "claude", "agents", "--json")
	cmd.Dir = dir
	data, err := cmd.Output()
	var sessions []claudeSession
	if err != nil || json.Unmarshal(data, &sessions) != nil {
		return nil
	}
	byPID := make(map[string]statusEvent)
	var pids []string
	for _, s := range sessions {
		e := s.event()
		if s.Kind == "interactive" && s.PID > 0 && e.State != "" {
			pid := strconv.Itoa(s.PID)
			byPID[pid] = e
			pids = append(pids, pid)
		}
	}
	if len(pids) == 0 {
		return nil
	}
	output, err := exec.CommandContext(ctx, "ps", "-p", strings.Join(pids, ","), "-o", "pid=,tty=,stat=").Output()
	if err != nil {
		return nil
	}
	byTTY := make(map[string]statusEvent)
	for _, line := range strings.Split(string(output), "\n") {
		fields := strings.Fields(line)
		if len(fields) == 3 && strings.Contains(fields[2], "+") {
			byTTY["/dev/"+fields[1]] = byPID[fields[0]]
		}
	}
	return byTTY
}

func (w *workspace) watchClaudeSessions(ctx context.Context) {
	ticker := time.NewTicker(2 * time.Second)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			w.pollClaudeSessions(ctx)
		}
	}
}

func (w *workspace) pollClaudeSessions(ctx context.Context) {
	w.mu.Lock()
	panes := make(map[*tracked]uint64)
	for _, t := range w.terms {
		if t.claudeTTY != "" && !t.claudeReported {
			panes[t] = t.term.Revision
		}
	}
	w.mu.Unlock()
	if len(panes) == 0 {
		return
	}
	ctx, cancel := context.WithTimeout(ctx, 3*time.Second)
	defer cancel()
	events := claudeEventsByTTY(ctx, w.p.dir)

	w.mu.Lock()
	defer w.mu.Unlock()
	for t, rev := range panes {
		e, ok := events[t.claudeTTY]
		if !ok || w.terms[t.term.ID] != t || t.term.Revision != rev || t.claudeReported {
			continue
		}
		e.Pane = t.term.ID
		if w.applyEventLocked(e, false) {
			_, _ = os.Stdout.WriteString("\a")
		}
	}
}
