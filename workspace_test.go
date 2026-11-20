package main

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"
)

// Exercise the exact same subprocess entry points as the installed binary.
func TestMain(m *testing.M) {
	if len(os.Args) > 1 && strings.HasPrefix(os.Args[1], "_") {
		main()
		return
	}
	os.Exit(m.Run())
}

func TestStatusFragments(t *testing.T) {
	stream := "unrelated\x1b]0;title\x07\x1b]7501;state=working:app=codex\x1b\\noise\x1b]7501;state=blocked:app=codex:kind=question\x07\x1b]7501;state=blocked:app=codex:kind=permission\x1b\\\x1b]7501;state=error:app=other\x07\x1b]7501;state=clear\x1b\\"
	for size := 1; size <= len(stream); size++ {
		var p statusParser
		var got []string
		for i := 0; i < len(stream); i += size {
			p.feed([]byte(stream[i:min(i+size, len(stream))]), func(s, k string) { got = append(got, s+"/"+k) })
		}
		if !slices.Equal(got, []string{"working/", "waiting/question", "blocked/permission", "shell/"}) {
			t.Fatalf("chunk %d: %v", size, got)
		}
	}
}
func TestUnbornWorktree(t *testing.T) {
	repo := t.TempDir()
	base := t.TempDir()
	if _, err := git(repo, "init", "-b", "main"); err != nil {
		t.Fatal(err)
	}
	tree, err := createTree(base, repo, "topic/one")
	if err != nil {
		t.Fatal(err)
	}
	if tree.Repo != repo || tree.Branch != "topic/one" {
		t.Fatal(tree)
	}
	retry, err := createTree(base, repo, "topic/one")
	if err != nil || retry != tree {
		t.Fatal(retry, err)
	}
	if _, err := createTree(base, repo, "../outside"); err == nil {
		t.Fatal("accepted invalid branch")
	}
}

func TestStateHistory(t *testing.T) {
	w := &workspace{terms: map[string]*tracked{"%1": {}}, changed: make(chan struct{})}
	for _, s := range []string{"working", "waiting", "working", "done"} {
		w.event(statusEvent{"%1", s, ""})
	}
	rev := uint64(1)
	result, err := w.wait(context.Background(), waitArgs{ID: "%1", States: []string{"waiting"}, After: &rev})
	if err != nil || !result.Matched || result.Status.Revision != 2 {
		t.Fatal(result, err)
	}
}
func TestMCPAuth(t *testing.T) {
	w := &workspace{key: strings.Repeat("k", 64)}
	h := w.handler()
	for _, tc := range []struct {
		auth, origin, host string
		want               int
	}{
		{"", "", "localhost:9417", 401},
		{"", "", "10.0.1.2:9417", 401},
		{"Bearer bad", "", "10.0.1.2:9417", 401},
		{"Bearer " + w.key, "http://foreign.test", "10.0.1.2:9417", 403},
		{"Bearer " + w.key, "", "desktop.lan:9417", 200},
		{"Bearer " + w.key, "", "10.0.1.2:9417", 200},
		{"Bearer " + w.key, "", "[fd00::2]:9417", 200},
		{"Bearer " + w.key, "", "127.0.0.1:9417", 200},
	} {
		r := httptest.NewRequest("POST", "/mcp", strings.NewReader(`{"jsonrpc":"2.0","id":1,"method":"initialize","params":{"protocolVersion":"2025-11-25","capabilities":{},"clientInfo":{"name":"test","version":"1"}}}`))
		r.Host = tc.host
		r.Header.Set("Origin", tc.origin)
		r.Header.Set("Authorization", tc.auth)
		r.Header.Set("Content-Type", "application/json")
		r.Header.Set("Accept", "application/json, text/event-stream")
		rw := httptest.NewRecorder()
		h.ServeHTTP(rw, r)
		if rw.Code != tc.want {
			t.Fatalf("%s got %d: %s", tc.host, rw.Code, rw.Body.String())
		}
	}
}

func TestLiveWorkspace(t *testing.T) {
	if os.Getenv("KOWLOON_INTEGRATION") != "1" {
		t.Skip("set KOWLOON_INTEGRATION=1 for a real tmux server")
	}
	// Use a short socket path. Everything, including the tmux session, belongs to this test.
	base, err := os.MkdirTemp("", "kw-live-")
	if err != nil {
		t.Fatal(err)
	}
	defer os.RemoveAll(base)
	for k, v := range map[string]string{"XDG_RUNTIME_DIR": base, "XDG_CONFIG_HOME": base + "/config", "XDG_DATA_HOME": base + "/data", "KOWLOON_PORT": "0", "HOME": base, "SHELL": "/bin/sh", "TERM": "xterm-256color", "CODEX_HOME": base + "/codex", "TMUX": "", "TMUX_PANE": ""} {
		t.Setenv(k, v)
	}
	if os.Getenv("KOWLOON_TEST_CODEX") == "1" {
		// A real CLI in an isolated test home must not leave a shared daemon behind.
		codex, err := exec.LookPath("codex")
		if err != nil {
			t.Fatal(err)
		}
		bin := filepath.Join(base, "bin")
		if err = os.Mkdir(bin, 0700); err != nil {
			t.Fatal(err)
		}
		wrapper := "#!/bin/sh\nexec " + quote(codex) + " --no-daemon \"$@\"\n"
		if err = os.WriteFile(filepath.Join(bin, "codex"), []byte(wrapper), 0700); err != nil {
			t.Fatal(err)
		}
		t.Setenv("PATH", bin+string(os.PathListSeparator)+os.Getenv("PATH"))
	}
	p, err := paths()
	if err != nil {
		t.Fatal(err)
	}
	repo := base + "/a repository"
	if err = os.Mkdir(repo, 0700); err != nil {
		t.Fatal(err)
	}
	for _, args := range [][]string{{"init", "-b", "main"}, {"-c", "user.name=Test", "-c", "user.email=test@localhost", "commit", "--allow-empty", "-m", "initial"}} {
		if _, err = git(repo, args...); err != nil {
			t.Fatal(err)
		}
	}
	defer p.tm("kill-server")
	t.Chdir(repo)
	if err = dispatch([]string{"--detach"}); err != nil {
		t.Fatal(err)
	}
	endpoint, err := os.ReadFile(filepath.Join(p.dir, "endpoint"))
	if err != nil {
		t.Fatal(err)
	}
	k, err := secret(p)
	if err != nil {
		t.Fatal(err)
	}
	tool := func(name string, args any, out any) {
		t.Helper()
		msg, _ := json.Marshal(map[string]any{"jsonrpc": "2.0", "id": 1, "method": "tools/call", "params": map[string]any{"name": name, "arguments": args}})
		req, err := http.NewRequest("POST", string(endpoint), strings.NewReader(string(msg)))
		if err != nil {
			t.Fatal(err)
		}
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("Accept", "application/json, text/event-stream")
		req.Header.Set("Authorization", "Bearer "+k)
		resp, err := (&http.Client{Timeout: 10 * time.Second}).Do(req)
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()
		var reply struct {
			Result struct {
				Structured json.RawMessage `json:"structuredContent"`
				IsError    bool
				Content    json.RawMessage
			}
			Error json.RawMessage
		}
		if err = json.NewDecoder(resp.Body).Decode(&reply); err != nil || reply.Error != nil || reply.Result.IsError {
			t.Fatalf("%s HTTP %d: %v %s %s", name, resp.StatusCode, err, reply.Error, reply.Result.Content)
		}
		if out != nil {
			if err = json.Unmarshal(reply.Result.Structured, out); err != nil {
				t.Fatal(err)
			}
		}
	}
	var s Snapshot
	tool("list_workspace", noArgs{}, &s)
	if s.Active != "" || len(s.Terminals) != 0 || len(s.Checkouts) != 0 {
		t.Fatalf("pathless startup from a Git repository opened a terminal: %+v", s)
	}
	if err = dispatch([]string{"--detach", "."}); err != nil {
		t.Fatal(err)
	}
	tool("list_workspace", noArgs{}, &s)
	if len(s.Terminals) != 1 || s.Terminals[0].Checkout.Branch != "main" || s.Width >= 60 {
		t.Fatal(s)
	}
	first := s.Active
	var child Terminal
	tool("create_worktree", treeArgs{repo, "feature/test"}, &child)
	if child.Checkout.Repo != repo || child.Checkout.Branch != "feature/test" {
		t.Fatal(child)
	}
	tool("resize_sidebar", widthArgs{35}, nil)
	tool("send_input", inputArgs{ID: child.ID, Text: "printf 'question> '; read answer; printf 'reply=%s\\n' \"$answer\"", Key: "Enter"}, nil)
	tool("send_input", inputArgs{ID: child.ID, Text: "yes", Key: "Enter"}, nil)
	eventually := func(check func() bool) {
		t.Helper()
		until := time.Now().Add(6 * time.Second)
		for time.Now().Before(until) {
			if check() {
				return
			}
			time.Sleep(60 * time.Millisecond)
		}
		t.Fatal("timed out waiting for terminal state")
	}
	eventually(func() bool {
		var view screen
		tool("read_terminal", readArgs{ID: child.ID}, &view)
		return strings.Contains(view.Text, "reply=yes")
	})
	tool("select_terminal", termArgs{first}, nil)
	// Simulate the documented Codex wire records in an inactive real terminal.
	tool("send_input", inputArgs{ID: child.ID, Text: `sh -c 'printf "\033]7501;state=blocked:app=codex:kind=question\033\\"; read a; printf "\033]7501;state=done:app=codex\033\\"; sleep 1'`, Key: "Enter"}, nil)
	var r waitResult
	tool("wait_for_state", waitArgs{ID: child.ID, States: []string{"waiting"}, After: &child.Revision, Seconds: 3}, &r)
	if !r.Matched || r.Status.Reason != "question" {
		t.Fatal(r)
	}
	tool("send_input", inputArgs{ID: child.ID, Text: "yes", Key: "Enter"}, nil)
	tool("wait_for_state", waitArgs{ID: child.ID, States: []string{"done"}, After: &r.Status.Revision, Seconds: 3}, &r)
	if !r.Matched {
		t.Fatal(r)
	}
	if err = dispatch([]string{"--detach"}); err != nil {
		t.Fatal(err)
	}
	tool("list_workspace", noArgs{}, &s)
	if s.Active != first || len(s.Terminals) != 2 {
		t.Fatalf("pathless reattach changed the workspace: %+v", s)
	}
	if err = start(p, repo); err != nil {
		t.Fatal(err)
	}
	tool("list_workspace", noArgs{}, &s)
	if len(s.Terminals) != 3 {
		t.Fatal(s)
	}
	// tmux treats a literal %4 in a pipe command as a time format.
	fourth := s.Active
	tool("send_input", inputArgs{ID: fourth, Text: `printf '\033]7501;state=working:app=codex\033\\'`, Key: "Enter"}, nil)
	rev := uint64(1)
	tool("wait_for_state", waitArgs{ID: fourth, States: []string{"working"}, After: &rev, Seconds: 2}, &r)
	if !r.Matched {
		t.Fatal("missing status on later terminal", fourth)
	}
	// Use keyboard input on the real sidebar; both it and the agent screens remain independent.
	side, _ := p.tm("display-message", "-p", "-t", "kowloon:workspace.0", "#{pane_id}")
	p.tm("send-keys", "-t", side, "s")
	eventually(func() bool { v, _ := p.tm("capture-pane", "-p", "-t", side); return strings.Contains(v, "Copy key") })
	p.tm("send-keys", "-t", side, "r")
	eventually(func() bool { v, _ := p.tm("capture-pane", "-p", "-t", side); return strings.Contains(v, k[:16]) })
	p.tm("send-keys", "-t", side, "c")
	eventually(func() bool { b, _ := p.tm("show-buffer", "-b", "kowloon-key"); return b == k })
	p.tm("send-keys", "-t", side, "Escape")
	// A shell can move to another checkout or switch branches without a UI action.
	tool("send_input", inputArgs{ID: first, Text: "cd " + quote(child.Checkout.Path) + " && git switch -c switched", Key: "Enter"}, nil)
	eventually(func() bool {
		var live Snapshot
		tool("list_workspace", noArgs{}, &live)
		for _, term := range live.Terminals {
			if term.ID == first {
				return term.Checkout.Path == child.Checkout.Path && term.Checkout.Branch == "switched" && live.Width == 35
			}
		}
		return false
	})
	// Launch the installed interactive CLI in a separate terminal.
	if os.Getenv("KOWLOON_TEST_CODEX") == "1" {
		var agent Terminal
		tool("launch_codex", pathArgs{repo}, &agent)
		eventually(func() bool {
			var view screen
			tool("read_terminal", readArgs{ID: agent.ID}, &view)
			return strings.Contains(strings.ToLower(view.Text), "codex")
		})
		tool("close_terminal", termArgs{agent.ID}, nil)
	}
	for _, term := range s.Terminals {
		tool("close_terminal", termArgs{term.ID}, nil)
	}
	tool("list_workspace", noArgs{}, &s)
	if len(s.Terminals) != 0 || len(s.Checkouts) != 0 {
		t.Fatal(s)
	}
	tool("open_repo", pathArgs{repo}, &child)
	if child.Checkout.Path != repo {
		t.Fatal(child)
	}
	// Click + New with a repository selected, then change directory in the new shell.
	click := fmt.Sprintf("\x1b[<0;%d;1M\x1b[<0;%d;1m", s.Width-2, s.Width-2)
	if _, err := p.tm("send-keys", "-t", side, "-l", click); err != nil {
		t.Fatal(err)
	}
	var homeID string
	eventually(func() bool {
		var live Snapshot
		tool("list_workspace", noArgs{}, &live)
		if len(live.Terminals) != 2 || live.Active == child.ID {
			return false
		}
		homeID = live.Active
		actual, err := p.tm("display-message", "-p", "-t", homeID, "#{pane_current_path}")
		return err == nil && actual == base
	})
	project := filepath.Join(base, "another-project")
	if _, err := git(base, "init", "-b", "trunk", project); err != nil {
		t.Fatal(err)
	}
	subdir := filepath.Join(project, "src")
	if err := os.Mkdir(subdir, 0700); err != nil {
		t.Fatal(err)
	}
	tool("send_input", inputArgs{ID: homeID, Text: "cd " + quote(subdir), Key: "Enter"}, nil)
	eventually(func() bool {
		view, _ := p.tm("capture-pane", "-p", "-t", side)
		return strings.Contains(view, "another-project  · trunk") && !strings.Contains(view, "▾ "+filepath.Base(base))
	})
	// A worktree keeps its parent repository open after the main shell exits.
	var worktree Terminal
	tool("create_worktree", treeArgs{repo, "feature/cleanup"}, &worktree)
	exitShell := func(id string) {
		t.Helper()
		tool("send_input", inputArgs{ID: id, Text: "exit", Key: "Enter"}, nil)
		var result waitResult
		tool("wait_for_state", waitArgs{ID: id, States: []string{"exited"}, Seconds: 4}, &result)
		if !result.Matched {
			t.Fatalf("terminal %s did not exit", id)
		}
	}
	exitShell(child.ID)
	eventually(func() bool {
		view, _ := p.tm("capture-pane", "-p", "-t", side)
		return strings.Contains(view, "▾ a repository") && strings.Contains(view, "feature/cleanup")
	})
	exitShell(worktree.ID)
	eventually(func() bool {
		tool("list_workspace", noArgs{}, &s)
		view, _ := p.tm("capture-pane", "-p", "-t", side)
		return s.Active == homeID && len(s.Checkouts) == 1 && s.Checkouts[0].Repo == project && !strings.Contains(view, "▾ a repository")
	})
	exitShell(homeID)
	tool("list_workspace", noArgs{}, &s)
	if s.Active != "" || len(s.Checkouts) != 0 {
		t.Fatal(s)
	}

}
