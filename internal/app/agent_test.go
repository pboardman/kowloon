package app

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

func TestSetupRequiresCurrentExplicitChoice(t *testing.T) {
	dir := t.TempDir()
	if err := chooseSetup(dir, "hook_review", "continue_without_hooks"); err == nil {
		t.Fatal("accepted out-of-order decision")
	}
	if err := chooseSetup(dir, "folder_trust", ""); err == nil {
		t.Fatal("trusted an empty input")
	}
	results := make(chan error, 2)
	for _, choice := range []string{"trust_folder", "open_untrusted"} {
		go func() { results <- chooseSetup(dir, "folder_trust", choice) }()
	}
	if (<-results == nil) == (<-results == nil) {
		t.Fatal("expected exactly one winning decision")
	}
	if err := chooseSetup(dir, "hook_review", "continue_without_hooks"); err != nil {
		t.Fatal(err)
	}
	b, values, err := setupState(dir)
	if err != nil || b != nil || values[1] != "continue_without_hooks" {
		t.Fatal(b, values, err)
	}
}
func TestTurnStateRequiresMatchingCodexOutcome(t *testing.T) {
	var th codexThread
	th.Status.Type = "idle"
	for _, tc := range []struct{ wire, flag, want string }{{"inProgress", "", "running"}, {"inProgress", "waitingOnApproval", "waiting_for_approval"}, {"inProgress", "waitingOnUserInput", "waiting_for_input"}, {"completed", "", "finished"}, {"failed", "", "failed"}, {"interrupted", "", "interrupted"}} {
		th.Status.ActiveFlags = []string{tc.flag}
		got, err := turnState(codexTurn{Status: tc.wire}, th)
		if got != tc.want || err != nil {
			t.Fatal(got, err)
		}
	}
	if _, err := turnState(codexTurn{Status: "idle"}, th); err == nil {
		t.Fatal("terminal idle completed a run")
	}
}

func startTestModel(t *testing.T, base string) func() {
	t.Helper()
	release := make(chan struct{})
	unblock := sync.OnceFunc(func() { close(release) })
	mock := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != "POST" {
			fmt.Fprint(w, `{"data":[]}`)
			return
		}
		body, err := io.ReadAll(r.Body)
		if err != nil {
			return
		}
		var req struct {
			Input []struct {
				Type    string
				Role    string
				Content json.RawMessage
			}
		}
		if err = json.Unmarshal(body, &req); err != nil {
			t.Error(err)
			return
		}
		kind := ""
		for _, item := range req.Input {
			if item.Role == "user" && strings.Contains(string(item.Content), "fixture-approval") {
				kind = "approval"
			}
			if item.Role == "user" && strings.Contains(string(item.Content), "fixture-question") {
				kind = "question"
			}
			if item.Type == "function_call_output" {
				kind = ""
			}
		}
		receivedGreen := strings.Contains(string(body), "Choose the forest green shade.")
		w.Header().Set("Content-Type", "text/event-stream")
		fmt.Fprint(w, "event: response.created\ndata: {\"type\":\"response.created\",\"response\":{\"id\":\"resp_1\",\"status\":\"in_progress\"}}\n\n")
		w.(http.Flusher).Flush()
		select {
		case <-release:
		case <-r.Context().Done():
			return
		case <-time.After(20 * time.Second):
			return
		}
		item := `{"id":"msg_1","type":"message","role":"assistant","status":"completed","content":[{"type":"output_text","text":"Local test reply."}]}`
		if receivedGreen {
			item = strings.Replace(item, "Local test reply.", "Received the requested shade.", 1)
		}
		if kind == "approval" {
			item = `{"id":"fc_approval","type":"function_call","call_id":"call_approval","name":"exec_command","arguments":"{\"cmd\":\"pwd\",\"sandbox_permissions\":\"require_escalated\",\"justification\":\"Print the test directory?\"}"}`
		}
		if kind == "question" {
			args := `{"questions":[{"id":"pick","header":"Color","question":"Which color?","options":[{"label":"Blue","description":"Choose blue."},{"label":"Green","description":"Choose green."}]}]}`
			data, _ := json.Marshal(map[string]string{"id": "fc_question", "type": "function_call", "call_id": "call_question", "name": "request_user_input", "arguments": args})
			item = string(data)
		}
		fmt.Fprintf(w, "event: response.output_item.done\ndata: {\"type\":\"response.output_item.done\",\"output_index\":0,\"item\":%s}\n\n", item)
		fmt.Fprintf(w, "event: response.completed\ndata: {\"type\":\"response.completed\",\"response\":{\"id\":\"resp_1\",\"status\":\"completed\",\"output\":[%s],\"usage\":{\"input_tokens\":1,\"output_tokens\":1,\"total_tokens\":2}}}\n\n", item)
	}))
	t.Cleanup(func() { unblock(); mock.Close() })
	config := fmt.Sprintf("features.default_mode_request_user_input=true\napproval_policy='on-request'\nsandbox_mode='read-only'\napprovals_reviewer='user'\nmodel='gpt-5.4'\nmodel_provider='kowloon_test'\n[model_providers.kowloon_test]\nname='Kowloon test'\nbase_url='%s/v1'\nwire_api='responses'\n", mock.URL)
	if err := os.WriteFile(filepath.Join(base, "codex", "config.toml"), []byte(config), 0600); err != nil {
		t.Fatal(err)
	}
	return unblock
}

func TestLostTerminalDoesNotFinishRun(t *testing.T) {
	w := newWorkspace(runtimePaths{}, "")
	a := newCodexAgent(t.TempDir(), "", "")
	r := run{ID: "run-one", Terminal: "%1", Thread: "thread-one", Status: "accepted", Revision: 1}
	w.runs[r.ID] = r
	a.active = r.ID
	a.close()
	go w.monitorRun(a, r)
	ctx, cancel := context.WithTimeout(t.Context(), 2*time.Second)
	defer cancel()
	out, err := w.waitRun(ctx, runWaitArgs{RunID: r.ID})
	if err != nil || !out.Matched || out.Run.Status != "unavailable" {
		t.Fatal(out, err)
	}
}

func testManagedRun(t *testing.T, tool func(string, any, any), agent Terminal, release func()) {
	t.Helper()
	for _, pair := range [][2]string{{"folder_trust", "open_untrusted"}, {"hook_review", "continue_without_hooks"}} {
		var st agentStatus
		tool("get_agent_status", termArgs{agent.ID}, &st)
		if st.CanSubmit || st.Blocker == nil || st.Blocker.Kind != pair[0] || st.Blocker.Path != agent.Checkout.Path {
			t.Fatal(st)
		}
		tool("answer_setup", decisionArgs{agent.ID, pair[0], pair[1]}, nil)
	}
	var st agentStatus
	deadline := time.Now().Add(12 * time.Second)
	for time.Now().Before(deadline) {
		tool("get_agent_status", termArgs{agent.ID}, &st)
		if st.CanSubmit {
			break
		}
		time.Sleep(100 * time.Millisecond)
	}
	if !st.CanSubmit {
		var view screen
		tool("read_terminal", readArgs{ID: agent.ID}, &view)
		t.Fatalf("agent status %+v; screen %s", st, view.Text)
	}
	// Check the effective Codex configuration after both decisions.
	runtimeDir := filepath.Join(os.Getenv("XDG_RUNTIME_DIR"), "kowloon")
	dirs, err := filepath.Glob(filepath.Join(runtimeDir, "a-*", "rpc"))
	if err != nil || len(dirs) != 1 {
		t.Fatal(dirs, err)
	}
	client, err := dialCodex(t.Context(), filepath.Dir(dirs[0]))
	if err != nil {
		t.Fatal(err)
	}
	var effective struct {
		Config struct {
			Features map[string]bool
			Projects map[string]struct {
				TrustLevel string `json:"trust_level"`
			}
		}
	}
	err = client.call(t.Context(), "config/read", map[string]string{"cwd": agent.CWD}, &effective)
	client.conn.Close()
	hooks, present := effective.Config.Features["hooks"]
	if err != nil || !present || hooks || effective.Config.Projects[agent.CWD].TrustLevel != "untrusted" {
		t.Fatalf("wrong effective trust or hook choice in Codex: %+v %v", effective.Config, err)
	}
	var r run
	tool("submit_prompt", promptArgs{ID: agent.ID, Prompt: "Reply with a test message.", RequestID: "smoke-1"}, &r)
	if r.ID == "" || r.Status != "accepted" {
		t.Fatal(r)
	}
	var replay run
	tool("submit_prompt", promptArgs{ID: agent.ID, Prompt: "Reply with a test message.", RequestID: "smoke-1"}, &replay)
	if replay.ID != r.ID {
		t.Fatal("retry submitted another run")
	}
	var waited runWaitResult
	tool("wait_for_run", runWaitArgs{RunID: r.ID, States: []string{"running"}, Seconds: 5}, &waited)
	if !waited.Matched {
		t.Fatalf("Codex did not report a running turn: %+v", waited)
	}
	if err := dispatch([]string{"--restart"}); err != nil {
		t.Fatal(err)
	}
	var restoredRun run
	tool("get_run", runArgs{r.ID}, &restoredRun)
	if restoredRun.ID != r.ID || restoredRun.Status != "running" {
		t.Fatal("lost in-flight run during upgrade", restoredRun)
	}
	// Reproduce an independent terminal idle signal while the provider holds the turn open.
	events, err := net.Dial("unixgram", filepath.Join(runtimeDir, "events"))
	if err != nil {
		t.Fatal(err)
	}
	err = json.NewEncoder(events).Encode(statusEvent{Pane: agent.ID, State: "idle"})
	events.Close()
	if err != nil {
		t.Fatal(err)
	}
	var terminalWait waitResult
	tool("wait_for_state", waitArgs{ID: agent.ID, States: []string{"idle"}, Seconds: 2}, &terminalWait)
	if !terminalWait.Matched {
		t.Fatal(terminalWait)
	}
	tool("wait_for_run", runWaitArgs{RunID: r.ID, States: []string{"finished"}, Seconds: 1}, &waited)
	if waited.Matched {
		t.Fatal("quiet provider completed a running turn")
	}
	release()
	tool("wait_for_run", runWaitArgs{RunID: r.ID, Seconds: 8}, &waited)
	if !waited.Matched || waited.Run.Status != "finished" {
		t.Fatal(waited)
	}
	var view screen
	tool("read_terminal", readArgs{ID: agent.ID, Lines: 100}, &view)
	if !strings.Contains(view.Text, "Local test reply.") {
		t.Fatal(view.Text)
	}
	var approval run
	tool("submit_prompt", promptArgs{ID: agent.ID, Prompt: "fixture-approval"}, &approval)
	tool("wait_for_run", runWaitArgs{RunID: approval.ID, Seconds: 6}, &waited)
	if !waited.Matched || waited.Run.Status != "waiting_for_approval" {
		tool("read_terminal", readArgs{ID: agent.ID, Lines: 40}, &view)
		t.Fatalf("approval status %+v; screen %s", waited, view.Text)
	}
	tool("send_input", inputArgs{ID: agent.ID, Key: "Escape"}, nil)
	tool("wait_for_run", runWaitArgs{RunID: approval.ID, States: []string{"finished", "interrupted"}, Seconds: 5}, &waited)
	if !waited.Matched {
		t.Fatal("approval did not resolve", waited)
	}

	var conversations conversationList
	tool("list_conversations", noArgs{}, &conversations)
	var conversationID string
	for _, c := range conversations.Conversations {
		if c.ID == agent.ID {
			conversationID = c.ID
		}
	}
	if conversationID == "" {
		t.Fatal("managed agent missing from conversations", conversations)
	}
	var question run
	tool("submit_prompt", promptArgs{ID: agent.ID, Prompt: "fixture-question"}, &question)
	tool("wait_for_run", runWaitArgs{RunID: question.ID, Seconds: 6}, &waited)
	if !waited.Matched || waited.Run.Status != "waiting_for_input" {
		tool("read_terminal", readArgs{ID: agent.ID, Lines: 40}, &view)
		t.Fatalf("question status %+v; screen %s", waited, view.Text)
	}
	var next conversationEvent
	for range 8 {
		next = conversationEvent{}
		tool("wait_for_next_event", conversationArgs{ID: conversationID}, &next)
		if next.Question != nil {
			break
		}
	}
	var pending questionList
	tool("list_questions", questionFilter{RunID: question.ID}, &pending)
	if len(pending.Requests) != 1 {
		t.Fatal(pending)
	}
	q := pending.Requests[0]
	if q.RunID != question.ID || q.TerminalID != agent.ID || len(q.Questions) != 1 || q.Questions[0].Question != "Which color?" || len(q.Questions[0].Options) != 2 || q.Questions[0].Options[1].Label != "Green" {
		t.Fatal(q)
	}
	if next.Question == nil || next.Question.RequestID != q.RequestID || next.Event == nil || next.Event.Subject != question.ID {
		t.Fatal("missing structured question", next)
	}
	if err := dispatch([]string{"--restart"}); err != nil {
		t.Fatal(err)
	}
	deadline = time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		tool("list_questions", questionFilter{RunID: question.ID}, &pending)
		if len(pending.Requests) == 1 {
			break
		}
		time.Sleep(50 * time.Millisecond)
	}
	if len(pending.Requests) != 1 || pending.Requests[0].RequestID != q.RequestID {
		t.Fatal("lost the pending question during restart", pending)
	}
	id := q.RequestID
	var receipt answerReceipt
	tool("answer_question", answerArgs{id, map[string]string{"pick": "Choose the forest green shade."}}, &receipt)
	if receipt.Status != "sent" || receipt.RequestID != id {
		t.Fatal(receipt)
	}
	tool("wait_for_run", runWaitArgs{RunID: question.ID, States: []string{"finished"}, Seconds: 4}, &waited)
	if !waited.Matched {
		tool("read_terminal", readArgs{ID: agent.ID, Lines: 40}, &view)
		t.Fatalf("question did not resolve: %+v; screen %s", waited, view.Text)
	}
	tool("list_questions", questionFilter{RunID: question.ID}, &pending)
	if len(pending.Requests) != 0 {
		t.Fatal(pending)
	}
	deadline = time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		tool("read_terminal", readArgs{ID: agent.ID, Lines: 100}, &view)
		if strings.Contains(view.Text, "Received the requested shade.") {
			return
		}
		time.Sleep(50 * time.Millisecond)
	}
	t.Fatal("Codex did not pass the written answer to the model", view.Text)
}
