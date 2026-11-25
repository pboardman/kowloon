package app

import (
	"encoding/json"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"
)

func TestClaudeStatusHooks(t *testing.T) {
	for _, tc := range []struct {
		hook          claudeHook
		state, reason string
	}{
		{claudeHook{Event: "SessionStart"}, "idle", ""},
		{claudeHook{Event: "UserPromptSubmit"}, "working", ""},
		{claudeHook{Event: "PermissionRequest", Tool: "Bash"}, "blocked", "permission"},
		{claudeHook{Event: "PreToolUse", Tool: "AskUserQuestion"}, "waiting", "question"},
		{claudeHook{Event: "PermissionRequest", Tool: "AskUserQuestion"}, "waiting", "question"},
		{claudeHook{Event: "Elicitation"}, "waiting", "question"},
		{claudeHook{Event: "ElicitationResult"}, "working", ""},
		{claudeHook{Event: "StopFailure", Error: "authentication_failed"}, "blocked", "auth"},
		{claudeHook{Event: "StopFailure", Error: "rate_limit"}, "error", ""},
		{claudeHook{Event: "Stop"}, "done", ""},
		{claudeHook{Event: "PostToolUse", Agent: "child"}, "", ""},
		{claudeHook{Event: "SubagentStop"}, "", ""},
	} {
		state, reason := tc.hook.status()
		if state != tc.state || reason != tc.reason {
			t.Errorf("%+v returned (%q, %q), want (%q, %q)", tc.hook, state, reason, tc.state, tc.reason)
		}
	}
}

func TestClaudeHookCommand(t *testing.T) {
	base := t.TempDir()
	t.Setenv("XDG_RUNTIME_DIR", base)
	t.Setenv("XDG_CONFIG_HOME", base)
	t.Setenv("TMUX_PANE", "%7")
	p, err := paths()
	if err != nil {
		t.Fatal(err)
	}
	exe := filepath.Join(base, "Kowloon's test binary")
	if err := os.Symlink(p.exe, exe); err != nil {
		t.Fatal(err)
	}
	settings, err := claudeSettings(exe)
	if err != nil {
		t.Fatal(err)
	}
	var config struct {
		Hooks map[string][]struct{ Hooks []struct{ Command string } }
	}
	if err := json.Unmarshal([]byte(settings), &config); err != nil {
		t.Fatal(err)
	}
	socket := &net.UnixAddr{Name: filepath.Join(p.dir, "events"), Net: "unixgram"}
	c, err := net.ListenUnixgram(socket.Net, socket)
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	cmd := exec.Command("sh", "-c", config.Hooks["UserPromptSubmit"][0].Hooks[0].Command)
	cmd.Stdin = strings.NewReader(`{"hook_event_name":"UserPromptSubmit","prompt":"private prompt"}`)
	if out, err := cmd.CombinedOutput(); err != nil || len(out) != 0 {
		t.Fatalf("hook output %q, error %v", out, err)
	}
	c.SetReadDeadline(time.Now().Add(time.Second))
	var buf [2048]byte
	n, _, err := c.ReadFromUnix(buf[:])
	if err != nil {
		t.Fatal(err)
	}
	var event statusEvent
	if err := json.Unmarshal(buf[:n], &event); err != nil || event != (statusEvent{Pane: "%7", State: "working", Program: "claude"}) || strings.Contains(string(buf[:n]), "private prompt") {
		t.Fatalf("unexpected status datagram %q", buf[:n])
	}
}

func TestClaudeTerminalStatus(t *testing.T) {
	var p statusParser
	var got []statusEvent
	for _, b := range []byte("\x1b]7501;state=blocked:app=claude-code:kind=question\a\x1b]7501;state=clear:id=child\a\x1b]7501;state=done:app=claude-code\x1b\\") {
		p.feed([]byte{b}, func(e statusEvent) { got = append(got, e) })
	}
	want := []statusEvent{{Program: "claude", State: "waiting", Reason: "question"}, {Program: "claude", State: "done"}}
	if !slices.Equal(got, want) {
		t.Fatal(got)
	}
}

func TestClaudeConversation(t *testing.T) {
	w := newWorkspace(runtimePaths{}, "%0")
	repo := Checkout{Path: "/work", Repo: "/work"}
	term := Terminal{ID: "%3", Checkout: repo, Program: "claude", Status: Status{State: "starting"}}
	w.terms[term.ID] = &tracked{term: term, claudePane: true}
	if w.applyEvent(statusEvent{Pane: "%3", Program: "codex", State: "done"}) {
		t.Fatal("nested Codex turn rang the Claude bell")
	}
	conv := w.listConversations().Conversations
	if len(conv) != 1 || conv[0].Program != "claude" || conv[0].State != "starting" {
		t.Fatal(conv)
	}
	if !w.applyEvent(statusEvent{Pane: "%3", Program: "claude", State: "waiting", Reason: "question"}) {
		t.Fatal("Claude question did not ring the bell")
	}
	snap := w.snapshot()
	for _, rows := range [][]row{repositoryRows(snap), agentRows(snap)} {
		r := rows[len(rows)-1]
		if r.id != term.ID || !strings.Contains(r.label, "Claude Code") || !strings.Contains(r.label, "?") {
			t.Fatal(r)
		}
	}
	event, err := w.nextConversationEvent(t.Context(), term.ID, time.Second)
	if err != nil || event.Event == nil || event.Event.Type != "terminal.waiting" || event.Event.Reason != "question" {
		t.Fatal(event, err)
	}
}
