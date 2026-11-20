package main

import (
	"context"
	"crypto/sha256"
	"errors"
	"fmt"
	"os"
	"slices"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"
)

type codexAgent struct {
	thread              string // protected by workspace.mu
	nextProbe           time.Time
	dir, cwd, trustRoot string
	stop                chan struct{}
	once                sync.Once
	submit              sync.Mutex // serialize readiness checks and turn/start for this pane
	active              string     // protected by workspace.mu
}

func newCodexAgent(dir, cwd, trustRoot string) *codexAgent {
	return &codexAgent{dir: dir, cwd: cwd, trustRoot: trustRoot, stop: make(chan struct{})}
}
func (a *codexAgent) close() { a.once.Do(func() { close(a.stop); os.RemoveAll(a.dir) }) }

type agentStatus struct {
	State     string        `json:"state"`
	CanSubmit bool          `json:"can_submit"`
	Blocker   *setupBlocker `json:"blocker,omitempty"`
	RunID     string        `json:"run_id,omitempty"`
	ThreadID  string        `json:"thread_id,omitempty"`
}
type decisionArgs struct {
	ID     string `json:"id"`
	Kind   string `json:"kind" jsonschema:"Current setup blocker kind from get_agent_status"`
	Choice string `json:"choice" jsonschema:"One of the listed choices"`
}
type promptArgs struct {
	ID        string `json:"id" jsonschema:"Terminal ID from launch_codex or create_or_open_workspace"`
	Prompt    string `json:"prompt"`
	RequestID string `json:"request_id,omitempty" jsonschema:"Reuse the same ID to retry this prompt without submitting it twice"`
}
type run struct {
	ID         string `json:"run_id"`
	Terminal   string `json:"terminal_id"`
	Thread     string `json:"thread_id"`
	Status     string `json:"status"`
	Revision   uint64 `json:"revision"`
	requestID  string
	promptHash [sha256.Size]byte
}
type runArgs struct {
	RunID string `json:"run_id"`
}
type runWaitArgs struct {
	RunID   string   `json:"run_id"`
	States  []string `json:"states,omitempty" jsonschema:"Defaults to a final state, an approval, a question, or unavailable tracking"`
	Seconds int      `json:"timeout_seconds,omitempty" jsonschema:"From 1 to 120 seconds. Defaults to 30."`
}
type runWaitResult struct {
	Run     run  `json:"run"`
	Matched bool `json:"matched"`
}

var attentionStates = []string{"finished", "failed", "interrupted", "waiting_for_approval", "waiting_for_input", "unavailable"}

func (w *workspace) agent(id string) (*codexAgent, error) {
	w.mu.Lock()
	defer w.mu.Unlock()
	t, err := w.lookup(id)
	if err != nil {
		return nil, err
	}
	if t.term.State == "exited" {
		return nil, errors.New("the terminal has exited")
	}
	if t.agent == nil {
		return nil, errors.New("open a terminal with launch_codex to use Codex run tracking")
	}
	return t.agent, nil
}
func (w *workspace) inspectAgent(ctx context.Context, a *codexAgent) (agentStatus, *codexRPC, error) {
	select {
	case <-a.stop:
		return agentStatus{State: "exited"}, nil, nil
	default:
	}
	b, choices, err := setupState(a.dir)
	if err != nil {
		return agentStatus{}, nil, err
	}
	if b != nil {
		blocker := *b
		blocker.Path = a.trustRoot
		return agentStatus{State: "needs_decision", Blocker: &blocker}, nil, nil
	}
	if slices.Contains(choices, "cancel") {
		return agentStatus{State: "cancelled"}, nil, nil
	}
	w.mu.Lock()
	active := a.active
	w.mu.Unlock()
	if active != "" {
		return agentStatus{State: "busy", RunID: active}, nil, nil
	}
	c, err := dialCodex(ctx, a.dir)
	if errors.Is(err, os.ErrNotExist) || errors.Is(err, syscall.ECONNREFUSED) {
		return agentStatus{State: "starting"}, nil, nil
	}
	if err != nil {
		return agentStatus{}, nil, err
	}
	t, err := c.rootThread(ctx, a.cwd)
	if err != nil {
		c.conn.Close()
		return agentStatus{}, nil, err
	}
	if threadUUID.MatchString(t.ID) {
		w.mu.Lock()
		if a.thread != t.ID {
			a.thread = t.ID
			w.saveLocked()
		}
		w.mu.Unlock()
	}
	state := agentStatus{State: "starting", ThreadID: t.ID}
	if t.ID != "" {
		state.State = t.Status.Type
		if t.Status.Type == "idle" {
			var auth struct {
				Account            *struct{}
				RequiresOpenaiAuth bool
			}
			if err := c.call(ctx, "account/read", map[string]any{"refreshToken": false}, &auth); err != nil {
				c.conn.Close()
				return agentStatus{}, nil, err
			}
			if auth.RequiresOpenaiAuth && auth.Account == nil {
				state.State = "needs_sign_in"
			} else {
				state.State = "ready"
				state.CanSubmit = true
			}
		}
	}
	return state, c, nil
}
func (w *workspace) agentStatus(ctx context.Context, id string) (agentStatus, error) {
	a, err := w.agent(id)
	if err != nil {
		return agentStatus{}, err
	}
	a.submit.Lock()
	defer a.submit.Unlock()
	state, c, err := w.inspectAgent(ctx, a)
	if c != nil {
		c.conn.Close()
	}
	return state, err
}

func (w *workspace) submitPrompt(ctx context.Context, in promptArgs) (run, error) {
	if strings.TrimSpace(in.Prompt) == "" {
		return run{}, errors.New("enter a prompt")
	}
	a, err := w.agent(in.ID)
	if err != nil {
		return run{}, err
	}
	a.submit.Lock()
	defer a.submit.Unlock()
	if len(in.RequestID) > 128 {
		return run{}, errors.New("request_id exceeds 128 bytes")
	}
	digest := sha256.Sum256([]byte(in.Prompt))
	if in.RequestID != "" {
		w.mu.Lock()
		for _, previous := range w.runs {
			if previous.Terminal == in.ID && previous.requestID == in.RequestID {
				w.mu.Unlock()
				if previous.promptHash != digest {
					return run{}, errors.New("this request_id belongs to a different prompt")
				}
				return previous, nil
			}
		}
		w.mu.Unlock()
	}
	state, c, err := w.inspectAgent(ctx, a)
	if c != nil {
		defer c.conn.Close()
	}
	if err != nil {
		return run{}, err
	}
	if !state.CanSubmit {
		return run{}, fmt.Errorf("Codex is %s; use get_agent_status", state.State)
	}
	var ack struct{ Turn codexTurn }
	params := map[string]any{"threadId": state.ThreadID, "input": []map[string]string{{"type": "text", "text": in.Prompt}}}
	// Retain the receipt if the MCP client disconnects after submission.
	submitCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
	defer cancel()
	if err := c.call(submitCtx, "turn/start", params, &ack); err != nil {
		return run{}, fmt.Errorf("no Codex acceptance receipt: %w", err)
	}
	if ack.Turn.ID == "" {
		return run{}, errors.New("Codex did not return a turn ID")
	}
	r := run{ID: ack.Turn.ID, Terminal: in.ID, Thread: state.ThreadID, Status: "accepted", Revision: 1, requestID: in.RequestID, promptHash: digest}
	w.mu.Lock()
	a.active = r.ID
	w.runs[r.ID] = r
	w.events.append(queueEvent{Type: "run.accepted", Subject: r.ID, Revision: 1})
	w.notify()
	w.saveLocked()
	w.mu.Unlock()
	go w.monitorRun(a, r)
	return r, nil
}

func (w *workspace) recordRun(a *codexAgent, r run, state string, final bool) {
	w.mu.Lock()
	defer w.mu.Unlock()
	old, ok := w.runs[r.ID]
	if !ok {
		return
	}
	changed := state != old.Status
	if changed {
		old.Status = state
		old.Revision++
		w.runs[r.ID] = old
		w.events.append(queueEvent{Type: "run." + state, Subject: r.ID, Revision: old.Revision})
		w.notify()
	}
	if final {
		if a.active == r.ID {
			a.active = ""
		}
		w.pastRuns = append(w.pastRuns, r.ID)
		if len(w.pastRuns) > eventCapacity {
			delete(w.runs, w.pastRuns[0])
			w.pastRuns = w.pastRuns[1:]
		}
	}
	if changed || final {
		w.saveLocked()
	}
}
func (w *workspace) monitorRun(a *codexAgent, r run) {
	var c *codexRPC
	defer func() {
		if c != nil {
			c.conn.Close()
		}
	}()
	misses := 0
	previousState := ""
	for {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		var err error
		if c == nil {
			c, err = dialCodex(ctx, a.dir)
		}
		var state string
		if err == nil {
			state, err = c.runState(ctx, r.Thread, r.ID)
		}
		cancel()
		if err == nil {
			misses = 0
			final := state == "finished" || state == "failed" || state == "interrupted"
			// Rollout history can briefly classify a new turn as interrupted
			// before Codex publishes its live state.
			if state != "interrupted" || previousState == state {
				w.recordRun(a, r, state, final)
				if final {
					return
				}
			}
		} else {
			if c != nil {
				c.conn.Close()
				c = nil
			}
			misses++
			if misses >= 3 {
				w.recordRun(a, r, "unavailable", false)
			}
		}
		previousState = state
		select {
		case <-a.stop:
			w.recordRun(a, r, "unavailable", true)
			return
		case <-time.After(500 * time.Millisecond):
		}
	}
}
func (w *workspace) getRun(id string) (run, error) {
	r, ok := w.runs[id]
	if !ok {
		return run{}, errors.New("unknown or expired Kowloon run ID")
	}
	return r, nil
}
func (w *workspace) waitRun(ctx context.Context, in runWaitArgs) (runWaitResult, error) {
	if len(in.States) == 0 {
		in.States = attentionStates
	}
	for _, s := range in.States {
		if s != "accepted" && s != "running" && !slices.Contains(attentionStates, s) {
			return runWaitResult{}, fmt.Errorf("unknown run state %q", s)
		}
	}
	if in.Seconds == 0 {
		in.Seconds = 30
	}
	if in.Seconds < 1 || in.Seconds > 120 {
		return runWaitResult{}, errors.New("timeout_seconds must be between 1 and 120")
	}
	timer := time.NewTimer(time.Duration(in.Seconds) * time.Second)
	defer timer.Stop()
	for {
		w.mu.Lock()
		r, err := w.getRun(in.RunID)
		changed := w.changed
		w.mu.Unlock()
		if err != nil {
			return runWaitResult{}, err
		}
		if slices.Contains(in.States, r.Status) {
			return runWaitResult{r, true}, nil
		}
		select {
		case <-ctx.Done():
			return runWaitResult{}, ctx.Err()
		case <-timer.C:
			w.mu.Lock()
			defer w.mu.Unlock()
			r, err = w.getRun(in.RunID)
			return runWaitResult{r, slices.Contains(in.States, r.Status)}, err
		case <-changed:
		}
	}
}

func (w *workspace) registerAgentTools(s *mcp.Server) {
	register(s, "create_or_open_workspace", "Create or open a Kowloon workspace directory. Set init_git to initialize Git. Set launch_codex to start an interactive agent.", w.createWorkspace)
	registerCtx(s, "get_agent_status", "Read Codex prompt readiness and any setup decision for a Kowloon terminal.", func(ctx context.Context, a termArgs) (agentStatus, error) { return w.agentStatus(ctx, a.ID) })
	register(s, "answer_setup", "Answer the current Kowloon folder trust or hook review decision with a listed choice.", func(in decisionArgs) (ack, error) {
		a, err := w.agent(in.ID)
		if err != nil {
			return ack{}, err
		}
		return done(chooseSetup(a.dir, in.Kind, in.Choice))
	})
	registerCtx(s, "submit_prompt", "Submit text to a ready Codex terminal in Kowloon and return the run ID that Codex accepted.", w.submitPrompt)
	register(s, "get_run", "Read the status of one Codex run submitted through Kowloon.", func(in runArgs) (run, error) { w.mu.Lock(); defer w.mu.Unlock(); return w.getRun(in.RunID) })
	registerCtx(s, "wait_for_run", "Wait for a status on one Codex run submitted through Kowloon. A terminal idle event cannot finish a run.", w.waitRun)
}
