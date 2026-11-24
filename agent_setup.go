package main

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"slices"
	"strings"
	"syscall"
	"time"

	"golang.org/x/sys/unix"
)

type setupBlocker struct {
	Kind     string   `json:"kind"`
	Question string   `json:"question"`
	Choices  []string `json:"choices"`
	Path     string   `json:"path,omitempty"`
}

var setupSteps = []setupBlocker{
	{Kind: "folder_trust", Question: "Allow Codex to load project configuration from this folder for this session?", Choices: []string{"open_untrusted", "trust_folder", "cancel"}},
	{Kind: "hook_review", Question: "Which hooks can Codex use for this session?", Choices: []string{"continue_without_hooks", "use_previously_trusted_hooks", "cancel"}},
}

// Both the pane and MCP submit decisions through the same exclusive file publication.
func setupState(dir string) (*setupBlocker, []string, error) {
	choices := []string{}
	for i := range setupSteps {
		step := &setupSteps[i]
		raw, err := os.ReadFile(filepath.Join(dir, step.Kind))
		if errors.Is(err, os.ErrNotExist) {
			return step, choices, nil
		}
		if err != nil {
			return nil, nil, err
		}
		value := string(raw)
		if !slices.Contains(step.Choices, value) {
			return nil, nil, errors.New("invalid saved setup choice")
		}
		choices = append(choices, value)
		if value == "cancel" {
			return nil, choices, nil
		}
	}
	return nil, choices, nil
}

func chooseSetup(dir, kind, choice string) error {
	step, _, err := setupState(dir)
	if err != nil {
		return err
	}
	if step == nil || step.Kind != kind {
		return errors.New("the setup decision changed; read get_agent_status again")
	}
	if !slices.Contains(step.Choices, choice) {
		return fmt.Errorf("choose one of %s", strings.Join(step.Choices, ", "))
	}
	return publishPrivate(filepath.Join(dir, kind), []byte(choice), os.Link)
}

func runCodexPane(dir, exe, trustRoot, thread string) error {
	ctx, cancel := signal.NotifyContext(context.Background(), syscall.SIGTERM, syscall.SIGHUP, os.Interrupt)
	defer cancel()
	cwd, err := os.Getwd()
	if err != nil {
		return err
	}
	fmt.Println("Codex setup for", clean(cwd))
	if cwd != trustRoot {
		fmt.Println("Folder trust applies to the checkout at", clean(trustRoot))
	}
	var previous string
	var choices []string
	input := bufio.NewReader(os.Stdin)
	for {
		step, values, err := setupState(dir)
		if err != nil {
			return err
		}
		if step == nil {
			choices = values
			break
		}
		if previous != step.Kind {
			fmt.Print("\x1b]7501;state=blocked:app=codex:kind=permission\a")
			fmt.Println(step.Question)
			for i, v := range step.Choices {
				fmt.Printf("  %d. %s\n", i+1, v)
			}
			fmt.Println("Enter a choice number here or answer through Kowloon.")
			previous = step.Kind
		}
		if ctx.Err() != nil {
			return ctx.Err()
		}
		fd := []unix.PollFd{{Fd: int32(os.Stdin.Fd()), Events: unix.POLLIN}}
		n, err := unix.Poll(fd, 150)
		if errors.Is(err, unix.EINTR) || (err == nil && n == 0) {
			continue
		}
		if err != nil {
			return err
		}
		line, err := input.ReadString('\n')
		if err != nil {
			return err
		}
		line = strings.TrimSpace(line)
		for i, choice := range step.Choices {
			if line == fmt.Sprint(i+1) || line == choice {
				if err := chooseSetup(dir, step.Kind, choice); err != nil {
					fmt.Println(err)
				}
				break
			}
		}
	}
	if slices.Contains(choices, "cancel") {
		return nil
	}
	trust := "untrusted"
	if choices[0] == "trust_folder" {
		trust = "trusted"
	}
	pathKey, _ := json.Marshal(trustRoot)
	trustEntry := "={trust_level=\"" + trust + "\"}"
	entries := string(pathKey) + trustEntry
	if trustRoot != cwd {
		cwdKey, _ := json.Marshal(cwd)
		entries += "," + string(cwdKey) + trustEntry
	}
	args := []string{"-c", "projects={" + entries + "}"}
	if choices[1] == "continue_without_hooks" {
		args = append(args, "--disable", "hooks")
	}
	socket := filepath.Join(dir, "rpc")
	url := "unix://" + socket
	appArgs := append([]string{"app-server"}, args...)
	app := exec.CommandContext(ctx, exe, append(appArgs, "--listen", url)...)
	app.Stderr = os.Stderr
	if err := app.Start(); err != nil {
		return err
	}
	end := make(chan error, 1)
	go func() { end <- app.Wait(); close(end) }()
	defer func() { _ = app.Process.Kill(); <-end }()
	deadline := time.After(15 * time.Second)
	for {
		if s, err := os.Stat(socket); err == nil && s.Mode()&os.ModeSocket != 0 {
			break
		}
		select {
		case err := <-end:
			return fmt.Errorf("Codex app server stopped during startup: %v", err)
		case <-deadline:
			return errors.New("Codex app server did not open its socket")
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(30 * time.Millisecond):
		}
	}
	args = append(args, "--remote", url, "-C", cwd)
	if thread == resumePicker {
		args = append(args, "resume")
	} else if thread != "" {
		if !threadUUID.MatchString(thread) {
			return errors.New("invalid Codex conversation ID")
		}
		args = append(args, "resume", thread)
	}
	tui := exec.CommandContext(ctx, exe, args...)
	tui.Stdin, tui.Stdout, tui.Stderr = os.Stdin, os.Stdout, os.Stderr
	return tui.Run()
}

type createWorkspaceArgs struct {
	Path         string `json:"path" jsonschema:"Absolute directory path to create or open"`
	InitGit      bool   `json:"init_git,omitempty" jsonschema:"Initialize Git only if there is no enclosing repository"`
	LaunchClaude bool   `json:"launch_claude,omitempty" jsonschema:"Open a Claude Code terminal instead of a shell"`
	LaunchCodex  bool   `json:"launch_codex,omitempty" jsonschema:"Open a Codex terminal instead of a shell"`
}

func (w *workspace) createWorkspace(a createWorkspaceArgs) (Terminal, error) {
	program := launchShell
	switch {
	case a.LaunchClaude && a.LaunchCodex:
		return Terminal{}, errors.New("choose one agent to launch")
	case a.LaunchClaude:
		program = launchClaude
	case a.LaunchCodex:
		program = launchCodex
	}
	if !filepath.IsAbs(a.Path) {
		return Terminal{}, errors.New("supply an absolute workspace path")
	}
	w.mu.Lock()
	defer w.mu.Unlock()
	if err := os.MkdirAll(a.Path, 0700); err != nil {
		return Terminal{}, err
	}
	path, err := directory(a.Path)
	if err != nil {
		return Terminal{}, err
	}
	if a.InitGit {
		probe := exec.Command("git", "-C", path, "rev-parse", "--git-dir")
		probe.Env = append(os.Environ(), "LC_ALL=C")
		if out, err := probe.CombinedOutput(); err != nil {
			if !strings.HasPrefix(string(out), "fatal: not a git repository") {
				return Terminal{}, fmt.Errorf("inspect repository: %w: %s", err, strings.TrimSpace(string(out)))
			}
			if _, err = git(path, "init"); err != nil {
				return Terminal{}, err
			}
		}
		delete(w.cache, path)
	}
	for _, t := range w.terms {
		if t.term.CWD != path || t.term.State == "exited" {
			continue
		}
		matchingShell := program == launchShell && t.agent == nil && t.term.State == "shell" && isShell(t.term.Program)
		matchingCodex := a.LaunchCodex && t.agent != nil && t.agent.cwd == path
		matchingClaude := a.LaunchClaude && t.claudePane
		if matchingShell || matchingCodex || matchingClaude {
			t.term.Checkout = w.resolve(path)
			return t.term, w.selectLocked(t.term.ID, true)
		}
	}
	return w.spawn(path, program)
}
