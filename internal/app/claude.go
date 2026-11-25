package app

import (
	"encoding/json"
	"io"
	"net"
	"path/filepath"
	"time"
)

// --settings adds hooks only to this Claude invocation. Each hook observes an
// event without returning any text or permission decision to Claude.
func claudeSettings(exe string) (string, error) {
	type commandHook struct {
		Type    string `json:"type"`
		Command string `json:"command"`
	}
	type group struct {
		Matcher string        `json:"matcher,omitempty"`
		Hooks   []commandHook `json:"hooks"`
	}
	command := []commandHook{{"command", quote(exe) + " _claude_status"}}
	hooks := map[string][]group{}
	for event, matcher := range map[string]string{
		"SessionStart": "startup|resume|clear", "UserPromptSubmit": "",
		"PermissionRequest": "", "PreToolUse": "AskUserQuestion",
		"PostToolUse": "", "PostToolUseFailure": "",
		"Elicitation": "", "ElicitationResult": "",
		"Stop": "", "StopFailure": "", "Notification": "idle_prompt",
	} {
		hooks[event] = []group{{matcher, command}}
	}
	settings, err := json.Marshal(map[string]any{"hooks": hooks})
	return string(settings), err
}

type claudeHook struct {
	Event string `json:"hook_event_name"`
	Tool  string `json:"tool_name"`
	Agent string `json:"agent_id"`
	Error string `json:"error"`
}

func (h claudeHook) status() (state, reason string) {
	if h.Agent != "" {
		return "", "" // A subagent does not own the interactive pane.
	}
	switch h.Event {
	case "SessionStart", "Notification":
		return "idle", ""
	case "UserPromptSubmit", "PostToolUse", "PostToolUseFailure", "ElicitationResult":
		return "working", ""
	case "PreToolUse", "PermissionRequest":
		if h.Tool == "AskUserQuestion" {
			return "waiting", "question"
		}
		if h.Event == "PermissionRequest" {
			return "blocked", "permission"
		}
	case "Elicitation":
		return "waiting", "question"
	case "Stop":
		return "done", ""
	case "StopFailure":
		if h.Error == "authentication_failed" {
			return "blocked", "auth"
		}
		return "error", ""
	}
	return "", ""
}

func claudeStatus(p runtimePaths, pane string, input io.Reader) {
	var hook claudeHook
	// Tool results can be large. Bound memory use and drain the writer even
	// when a record exceeds the limit. Only forward terminal status.
	err := json.NewDecoder(io.LimitReader(input, 8<<20)).Decode(&hook)
	_, _ = io.Copy(io.Discard, input)
	state, reason := hook.status()
	if err != nil || state == "" || pane == "" {
		return
	}
	c, err := net.Dial("unixgram", filepath.Join(p.dir, "events"))
	if err != nil {
		return // The sidebar can be offline during a restart.
	}
	defer c.Close()
	c.SetWriteDeadline(time.Now().Add(time.Second))
	_ = json.NewEncoder(c).Encode(statusEvent{Pane: pane, State: state, Reason: reason, Program: "claude"})
}
