package app

import (
	"context"
	"crypto/subtle"
	"errors"
	"fmt"
	"net/http"
	"slices"
	"strings"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"
)

type noArgs struct{}
type pathArgs struct {
	Path string `json:"path" jsonschema:"Absolute repository or directory path"`
}
type treeArgs struct {
	Path   string `json:"path" jsonschema:"Source checkout path"`
	Branch string `json:"branch" jsonschema:"Worktree branch name"`
}
type termArgs struct {
	ID string `json:"id" jsonschema:"Terminal ID from list_workspace"`
}
type readArgs struct {
	ID    string `json:"id"`
	Lines int    `json:"lines,omitempty" jsonschema:"Extra scrollback lines from 0 to 20000. Default is the visible screen."`
}
type inputArgs struct {
	ID   string `json:"id"`
	Text string `json:"text,omitempty" jsonschema:"Literal text to paste"`
	Key  string `json:"key,omitempty" jsonschema:"Optional key after text. Use Enter to submit or arrows to choose an answer."`
}
type waitArgs struct {
	ID      string   `json:"id"`
	States  []string `json:"states" jsonschema:"Match any listed state"`
	After   *uint64  `json:"after_revision,omitempty" jsonschema:"Match only transitions newer than this revision, including recent history."`
	Seconds int      `json:"timeout_seconds,omitempty" jsonschema:"Timeout from 1 to 120 seconds. Defaults to 30."`
}
type waitResult struct {
	Status  Status `json:"status"`
	Matched bool   `json:"matched"`
}
type enabledArgs struct {
	Enabled bool `json:"enabled"`
}
type widthArgs struct {
	Columns int `json:"columns"`
}
type screen struct {
	Text string `json:"text"`
}
type ack struct {
	OK bool `json:"ok"`
}

var states = []string{"shell", "starting", "unknown", "idle", "working", "waiting", "blocked", "done", "error", "exited"}

func (w *workspace) wait(ctx context.Context, a waitArgs) (waitResult, error) {
	if len(a.States) == 0 {
		return waitResult{}, errors.New("supply at least one state")
	}
	for _, s := range a.States {
		if !slices.Contains(states, s) {
			return waitResult{}, fmt.Errorf("unknown state %q", s)
		}
	}
	if a.Seconds == 0 {
		a.Seconds = 30
	}
	if a.Seconds < 1 || a.Seconds > 120 {
		return waitResult{}, errors.New("timeout_seconds must be between 1 and 120")
	}
	timer := time.NewTimer(time.Duration(a.Seconds) * time.Second)
	defer timer.Stop()
	cursor := a.After
	for {
		w.mu.Lock()
		t, err := w.lookup(a.ID)
		if err != nil {
			w.mu.Unlock()
			return waitResult{}, err
		}
		current := t.term.Status
		changed := w.changed
		if cursor == nil {
			if slices.Contains(a.States, current.State) {
				w.mu.Unlock()
				return waitResult{current, true}, nil
			}
		} else {
			for _, s := range t.history {
				if s.Revision > *cursor && slices.Contains(a.States, s.State) {
					w.mu.Unlock()
					return waitResult{s, true}, nil
				}
			}
		}
		rev := current.Revision
		if cursor == nil || rev > *cursor {
			cursor = &rev
		}
		w.mu.Unlock()
		select {
		case <-ctx.Done():
			return waitResult{}, ctx.Err()
		case <-timer.C:
			w.mu.Lock()
			defer w.mu.Unlock()
			t, err := w.lookup(a.ID)
			if err != nil {
				return waitResult{}, err
			}
			return waitResult{t.term.Status, false}, nil
		case <-changed:
		}
	}
}

func registerCtx[I, O any](s *mcp.Server, name, desc string, fn func(context.Context, I) (O, error)) {
	mcp.AddTool(s, &mcp.Tool{Name: name, Description: desc}, func(ctx context.Context, _ *mcp.CallToolRequest, in I) (*mcp.CallToolResult, O, error) {
		out, err := fn(ctx, in)
		return nil, out, err
	})
}
func register[I, O any](s *mcp.Server, name, desc string, fn func(I) (O, error)) {
	registerCtx(s, name, desc, func(_ context.Context, in I) (O, error) { return fn(in) })
}
func done(err error) (ack, error) { return ack{err == nil}, err }

func (w *workspace) handler() http.Handler {
	s := mcp.NewServer(&mcp.Implementation{Name: "kowloon", Version: "0.1.0"}, nil)
	w.registerAgentTools(s)
	register(s, "save_session", "Save the current Kowloon workspace for the next start.", func(noArgs) (ack, error) { return done(w.refresh()) })
	register(s, "list_workspace", "List Kowloon repositories, worktrees, terminals, branches, sidebar width and live agent states.", func(noArgs) (Snapshot, error) { return w.snapshot(), nil })
	register(s, "list_conversations", "List live Kowloon agent conversations, their current state and any pending questions. Pass a conversation_id to wait_for_next_event.", func(noArgs) (conversationList, error) { return w.listConversations(), nil })
	registerCtx(s, "wait_for_next_event", "Wait for the next question, blocker, outcome, or exit in one listed Kowloon conversation. A structured question includes its choices and request_id for answer_question. On timeout, call again with the same conversation_id.", func(ctx context.Context, a conversationArgs) (conversationEvent, error) {
		return w.nextConversationEvent(ctx, a.ID, 30*time.Second)
	})
	register(s, "open_repo", "Open a Kowloon shell at the root of a Git checkout.", func(a pathArgs) (Terminal, error) { return w.open(a.Path, launchShell, true) })
	register(s, "new_terminal", "Open a Kowloon shell in a directory.", func(a pathArgs) (Terminal, error) { return w.open(a.Path, launchShell, false) })
	register(s, "create_worktree", "Create a named Git worktree and open its terminal. Reuse a matching Kowloon worktree.", func(a treeArgs) (Terminal, error) { return w.addTree(a.Path, a.Branch) })
	register(s, "launch_codex", "Launch interactive Codex in Kowloon. Use get_agent_status for setup decisions and prompt readiness.", func(a pathArgs) (Terminal, error) { return w.open(a.Path, launchCodex, false) })
	register(s, "launch_claude", "Launch interactive Claude Code in Kowloon. Use read_terminal and send_input to interact with it.", func(a pathArgs) (Terminal, error) { return w.open(a.Path, launchClaude, false) })
	register(s, "select_terminal", "Focus a terminal in the right pane of Kowloon.", func(a termArgs) (ack, error) { return done(w.selectTerm(a.ID)) })
	register(s, "close_terminal", "Stop the selected Kowloon terminal and its running program.", func(a termArgs) (ack, error) { return done(w.closeTerm(a.ID)) })
	register(s, "read_terminal", "Read the visible Kowloon terminal, including questions and answer choices. Optionally include scrollback.", func(a readArgs) (screen, error) { out, err := w.read(a.ID, a.Lines); return screen{out}, err })
	register(s, "send_input", "Send literal text, one named key, or both to a Kowloon terminal. Supported keys: "+strings.Join(keys, ", ")+".", func(a inputArgs) (ack, error) { return done(w.input(a.ID, a.Text, a.Key)) })
	register(s, "set_agent_mode", "Show Kowloon agent terminals in a flat list when enabled. Disable to show repositories.", func(a enabledArgs) (ack, error) {
		w.setAgentMode(a.Enabled)
		return ack{OK: true}, nil
	})
	register(s, "set_agent_bell", "Enable or mute the Kowloon agent bell and save the preference.", func(a enabledArgs) (ack, error) { return done(w.setAgentBell(a.Enabled)) })
	register(s, "resize_sidebar", "Set the width of the Kowloon sidebar in terminal columns.", func(a widthArgs) (ack, error) { return done(w.resize(a.Columns)) })
	mcp.AddTool(s, &mcp.Tool{Name: "wait_for_state", Description: "Wait for Kowloon terminal activity. For a submitted prompt, use wait_for_run. Terminal states: " + strings.Join(states, ", ") + ". Each terminal retains its last 64 transitions."}, func(ctx context.Context, _ *mcp.CallToolRequest, a waitArgs) (*mcp.CallToolResult, waitResult, error) {
		out, err := w.wait(ctx, a)
		return nil, out, err
	})
	handler := mcp.NewStreamableHTTPHandler(func(*http.Request) *mcp.Server { return s }, &mcp.StreamableHTTPOptions{Stateless: true, JSONResponse: true})
	return http.HandlerFunc(func(rw http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/mcp" {
			http.NotFound(rw, r)
			return
		}
		if r.Header.Get("Origin") != "" {
			http.Error(rw, "Browser origins are not supported", http.StatusForbidden)
			return
		}
		token, ok := strings.CutPrefix(r.Header.Get("Authorization"), "Bearer ")
		if !ok || token == "" || subtle.ConstantTimeCompare([]byte(token), []byte(w.key)) != 1 {
			rw.Header().Set("WWW-Authenticate", "Bearer")
			http.Error(rw, "MCP key required", http.StatusUnauthorized)
			return
		}
		r.Body = http.MaxBytesReader(rw, r.Body, 1<<20)
		handler.ServeHTTP(rw, r)
	})
}
