package app

import (
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"
)

func testSessionRecovery(t *testing.T, p runtimePaths, repo string, tool func(string, any, any)) {
	t.Helper()
	var shell Terminal
	tool("new_terminal", pathArgs{repo}, &shell)
	tool("send_input", inputArgs{ID: shell.ID, Text: "printf 'retained-process-%d\\n' $$", Key: "Enter"}, nil)
	tool("set_agent_mode", enabledArgs{true}, nil)
	tool("resize_sidebar", widthArgs{36}, nil)
	side := "kowloon:workspace.0"
	pid, err := p.tm("display-message", "-p", "-t", side, "#{pane_pid}")
	if err != nil {
		t.Fatal(err)
	}
	sidePID, err := strconv.Atoi(pid)
	if err != nil {
		t.Fatal(err)
	}
	if err = syscall.Kill(sidePID, syscall.SIGKILL); err != nil {
		t.Fatal(err)
	}
	for range 100 {
		v, _ := p.tm("display-message", "-p", "-t", side, "#{pane_dead}")
		if v == "1" {
			break
		}
		time.Sleep(20 * time.Millisecond)
	}
	if err = dispatch([]string{"--detach"}); err != nil {
		t.Fatal(err)
	}
	var snap Snapshot
	tool("list_workspace", noArgs{}, &snap)
	if snap.Active != shell.ID || !snap.AgentMode || snap.Width != 36 {
		t.Fatalf("lost live layout: %+v", snap)
	}
	pid, err = p.tm("display-message", "-p", "-t", shell.ID, "#{pane_pid}")
	if err != nil {
		t.Fatal(err)
	}
	var view screen
	tool("read_terminal", readArgs{ID: shell.ID}, &view)
	if !strings.Contains(view.Text, "retained-process-"+pid) {
		t.Fatal("shell or scrollback lost", view.Text)
	}
	var agent Terminal
	var state agentStatus
	missing := filepath.Join(repo, "temporarily-offline")
	if err := os.Mkdir(missing, 0700); err != nil {
		t.Fatal(err)
	}
	tool("new_terminal", pathArgs{missing}, nil)
	tool("select_terminal", termArgs{shell.ID}, nil)
	var receipt, unfinished run
	if os.Getenv("KOWLOON_TEST_CODEX") == "1" {
		tool("launch_codex", pathArgs{repo}, &agent)
		readyRecoveredAgent(t, tool, agent.ID, &state)
		tool("submit_prompt", promptArgs{ID: agent.ID, Prompt: "Remember this recovery test.", RequestID: "restart-test"}, &receipt)
		var done runWaitResult
		tool("wait_for_run", runWaitArgs{RunID: receipt.ID, Seconds: 8}, &done)
		if !done.Matched || done.Run.Status != "finished" {
			var v screen
			tool("read_terminal", readArgs{ID: agent.ID}, &v)
			t.Fatalf("unexpected recovery fixture run: %+v, screen %s", done, v.Text)
		}
		tool("submit_prompt", promptArgs{ID: agent.ID, Prompt: "fixture-question"}, &unfinished)
		tool("wait_for_run", runWaitArgs{RunID: unfinished.ID, Seconds: 8}, &done)
		if done.Run.Status != "waiting_for_input" {
			t.Fatal(done)
		}
	}
	if _, err = p.tm("kill-server"); err != nil {
		t.Fatal(err)
	}
	if err = os.Remove(missing); err != nil {
		t.Fatal(err)
	}
	if err = dispatch([]string{"--detach"}); err != nil {
		t.Fatal(err)
	}
	tool("list_workspace", noArgs{}, &snap)
	expected := 1
	if receipt.ID != "" {
		expected++
	}
	if len(snap.Terminals) != expected || snap.Terminals[0].CWD != repo || snap.Width != 36 || !snap.AgentMode {
		t.Fatalf("lost saved layout: %+v", snap)
	}
	if receipt.ID != "" {
		var lost run
		tool("get_run", runArgs{unfinished.ID}, &lost)
		if lost.Status != "unavailable" {
			t.Fatalf("lost turn reported %s", lost.Status)
		}
		var restored agentStatus
		readyRecoveredAgent(t, tool, snap.Active, &restored)
		if restored.ThreadID != state.ThreadID {
			t.Fatalf("restored wrong thread %s, wanted %s", restored.ThreadID, state.ThreadID)
		}
		var replay run
		tool("submit_prompt", promptArgs{ID: snap.Active, Prompt: "Remember this recovery test.", RequestID: "restart-test"}, &replay)
		if replay.ID != receipt.ID || replay.Status != "finished" {
			t.Fatal(replay)
		}
	}
	info, err := os.Stat(p.sessionPath())
	if err != nil || info.Mode().Perm() != 0600 {
		t.Fatal(info, err)
	}
	if err = dispatch([]string{"--stop"}); err != nil {
		t.Fatal(err)
	}
	if err = os.Mkdir(missing, 0700); err != nil {
		t.Fatal(err)
	}
	if err = dispatch([]string{"--detach"}); err != nil {
		t.Fatal(err)
	}
	tool("list_workspace", noArgs{}, &snap)
	if len(snap.Terminals) != expected+1 || snap.Terminals[len(snap.Terminals)-1].CWD != missing {
		t.Fatalf("did not recover returning directory: %+v", snap)
	}
	if err = dispatch([]string{"--stop"}); err != nil {
		t.Fatal(err)
	}
	if err = dispatch([]string{"--detach", "--no-restore"}); err != nil {
		t.Fatal(err)
	}
	tool("list_workspace", noArgs{}, &snap)
	if len(snap.Terminals) != 0 || snap.Active != "" || snap.AgentMode {
		t.Fatalf("no-restore reopened a workspace: %+v", snap)
	}
}
func readyRecoveredAgent(t *testing.T, tool func(string, any, any), id string, st *agentStatus) {
	t.Helper()
	for _, answer := range []decisionArgs{{id, "folder_trust", "open_untrusted"}, {id, "hook_review", "continue_without_hooks"}} {
		tool("answer_setup", answer, nil)
	}
	for range 100 {
		*st = agentStatus{}
		tool("get_agent_status", termArgs{id}, st)
		if st.CanSubmit {
			return
		}
		time.Sleep(100 * time.Millisecond)
	}
	var v screen
	tool("read_terminal", readArgs{ID: id}, &v)
	t.Fatalf("restored agent unavailable: %+v %s", st, v.Text)
}

func TestRejectCorruptSession(t *testing.T) {
	p := runtimePaths{config: t.TempDir()}
	for _, raw := range []string{`{"Version":8}`, `{"Version":1,"Panes":[{"CWD":"/tmp","AgentDir":"../elsewhere"}]}`, `{"Version":1,"Panes":[{"CWD":"/tmp","Thread":"--last"}]}`, "{"} {
		if err := os.WriteFile(filepath.Join(p.config, "session.json"), []byte(raw), 0600); err != nil {
			t.Fatal(err)
		}
		if _, err := p.readSession(); err == nil {
			t.Fatal("accepted invalid checkpoint", raw)
		}
	}
}
