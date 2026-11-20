package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"time"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/x/ansi"
	"github.com/modelcontextprotocol/go-sdk/mcp"
	"golang.org/x/sys/unix"
	"golang.org/x/term"
)

func main() {
	if err := dispatch(os.Args[1:]); err != nil {
		fmt.Fprintln(os.Stderr, "kowloon:", err)
		os.Exit(1)
	}
}
func dispatch(args []string) error {
	fs := flag.NewFlagSet("kowloon", flag.ContinueOnError)
	detach := fs.Bool("detach", false, "start without attaching a terminal")
	stop := fs.Bool("stop", false, "end the workspace and terminate all its running terminals")
	fs.Usage = func() {
		fmt.Fprintln(fs.Output(), "Usage: kowloon [--detach] [directory] | kowloon --stop\n\nStart an empty workspace or reattach to an existing one. Supply a directory to open a shell there.\nDrag the divider to resize. Press Ctrl+b, then d to detach.")
		fs.PrintDefaults()
	}
	if err := fs.Parse(args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return nil
		}
		return err
	}
	p, err := paths()
	if err != nil {
		return err
	}
	a := fs.Args()
	if *stop {
		if *detach || len(a) != 0 {
			return errors.New("use --stop alone to end all workspace terminals")
		}
		_, err := p.tm("kill-session", "-t", "=kowloon")
		return err
	}
	if len(a) > 0 {
		switch a[0] {
		case "_watch":
			if len(a) == 2 {
				return watch(p, "%"+a[1], os.Stdin)
			}
		case "_run":
			if len(a) == 3 {
				if _, err := p.tm("wait-for", a[1]); err != nil {
					return err
				}
				return syscall.Exec(a[2], []string{a[2]}, os.Environ())
			}
		case "_sidebar":
			if len(a) == 2 {
				return serve(p, a[1])
			}
		case "_empty":
			fmt.Println("Choose + New in the sidebar to open a terminal.")
			for {
				time.Sleep(time.Hour)
			}
		}
	}
	if len(a) > 1 {
		return errors.New("supply one directory")
	}
	var path string
	if len(a) > 0 {
		path, err = directory(a[0])
		if err != nil {
			return err
		}
	}
	if !*detach && !term.IsTerminal(int(os.Stdin.Fd())) {
		return errors.New("run kowloon in a terminal or use --detach")
	}
	if err := start(p, path); err != nil {
		return err
	}
	if *detach {
		fmt.Println("Kowloon is running. Use kowloon to attach.")
		return nil
	}
	exe, err := exec.LookPath("tmux")
	if err != nil {
		return err
	}
	os.Unsetenv("TMUX")
	return syscall.Exec(exe, append([]string{"tmux"}, p.tmArgs("attach-session", "-t", "kowloon")...), os.Environ())
}

func start(p runtimePaths, path string) error {
	lock, err := os.OpenFile(filepath.Join(p.dir, "start.lock"), os.O_CREATE|os.O_RDWR, 0600)
	if err != nil {
		return err
	}
	defer lock.Close()
	if err = unix.Flock(int(lock.Fd()), unix.LOCK_EX); err != nil {
		return err
	}
	if err := writePrivate(filepath.Join(p.dir, "tmux.conf"), tmuxConfig); err != nil {
		return err
	}
	_, existing := p.tm("has-session", "-t", "=kowloon")
	if existing != nil {
		width, height, err := term.GetSize(int(os.Stdout.Fd()))
		if err != nil || width < 20 || height < 5 {
			width, height = 120, 40
		}
		args := []string{"new-session", "-d", "-x", strconv.Itoa(width), "-y", strconv.Itoa(height), "-s", "kowloon", "-n", "workspace"}
		if path != "" {
			args = append(args, "-c", path)
		}
		_, err = p.tm(append(args, p.exe, "_sidebar", path)...)
		if err != nil {
			return err
		}
	}
	for range 100 {
		v, err := p.tm("show-options", "-qv", "-t", "kowloon", "@ready")
		if err == nil && v == "1" {
			dead, _ := p.tm("display-message", "-p", "-t", "kowloon:workspace.0", "#{pane_dead}")
			if dead == "1" {
				return errors.New("the sidebar exited; use kowloon --stop to end this workspace before restarting")
			}
			if existing == nil && path != "" {
				return openOnRunning(p, path)
			}
			return nil
		}
		dead, _ := p.tm("display-message", "-p", "-t", "kowloon:workspace.0", "#{pane_dead}")
		if dead == "1" {
			screen, _ := p.tm("capture-pane", "-p", "-t", "kowloon:workspace.0")
			if existing != nil {
				p.tm("kill-session", "-t", "=kowloon")
			}
			return errors.New(strings.TrimSpace(screen))
		}
		time.Sleep(50 * time.Millisecond)
	}
	return errors.New("workspace startup timed out")
}

type bearerTransport string

func (token bearerTransport) RoundTrip(r *http.Request) (*http.Response, error) {
	if r.URL.Scheme != "http" || r.URL.Hostname() != "127.0.0.1" {
		return nil, errors.New("invalid local MCP endpoint")
	}
	copy := r.Clone(r.Context())
	copy.Header.Set("Authorization", "Bearer "+string(token))
	return http.DefaultTransport.RoundTrip(copy)
}

// Open additional directories through the same MCP interface as other clients.
func openOnRunning(p runtimePaths, cwd string) error {
	addr, err := os.ReadFile(filepath.Join(p.dir, "endpoint"))
	if err != nil {
		return err
	}
	k, err := secret(p)
	if err != nil {
		return err
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	client := mcp.NewClient(&mcp.Implementation{Name: "kowloon-cli", Version: "0.1.0"}, nil)
	session, err := client.Connect(ctx, &mcp.StreamableClientTransport{
		Endpoint: string(addr), HTTPClient: &http.Client{Transport: bearerTransport(k), CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }},
		DisableStandaloneSSE: true, MaxRetries: -1}, nil)
	if err != nil {
		return err
	}
	defer session.Close()
	result, err := session.CallTool(ctx, &mcp.CallToolParams{Name: "new_terminal", Arguments: pathArgs{cwd}})
	if err != nil {
		return err
	}
	if result.IsError {
		for _, content := range result.Content {
			if msg, ok := content.(*mcp.TextContent); ok {
				return errors.New(msg.Text)
			}
		}
		return errors.New("opening terminal failed")
	}
	return nil
}

func serve(p runtimePaths, path string) error {
	side := os.Getenv("TMUX_PANE")
	w := newWorkspace(p, side)
	var err error
	w.agentBell, err = p.loadAgentBell()
	if err != nil {
		return err
	}
	w.key, err = secret(p)
	if err != nil {
		return err
	}
	w.eventKey, err = privateKey(p, "events.key")
	if err != nil {
		return err
	}
	if w.key == w.eventKey {
		return errors.New("MCP control and event keys must differ")
	}
	port := os.Getenv("KOWLOON_PORT")
	if port == "" {
		port = "9417"
	}
	n, err := strconv.Atoi(port)
	if err != nil || n < 0 || n > 65535 {
		return errors.New("invalid KOWLOON_PORT")
	}
	ln, err := net.Listen("tcp", net.JoinHostPort("", port))
	if err != nil {
		return err
	}
	defer ln.Close()
	// The CLI on this host always connects through loopback, including with port 0.
	w.endpoint = fmt.Sprintf("http://127.0.0.1:%d/mcp", ln.Addr().(*net.TCPAddr).Port)
	eventPath := filepath.Join(p.dir, "events")
	os.Remove(eventPath)
	events, err := net.ListenUnixgram("unixgram", &net.UnixAddr{Name: eventPath, Net: "unixgram"})
	if err != nil {
		return err
	}
	defer events.Close()
	go func() {
		var buf [2048]byte
		for {
			n, _, err := events.ReadFromUnix(buf[:])
			if err != nil {
				return
			}
			var e statusEvent
			if json.Unmarshal(buf[:n], &e) != nil {
				continue
			}
			if w.applyEvent(e) {
				// Ring from the visible sidebar, including for agents in parked panes.
				_, _ = os.Stdout.WriteString("\a")
			}
		}
	}()
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
	w.home, err = p.tm("split-window", "-d", "-h", "-l", strconv.Itoa(max(1, width-target-1)), "-P", "-F", "#{pane_id}", "-t", side, p.exe, "_empty")
	if err != nil {
		return err
	}
	if path != "" {
		if _, err = w.open(path, false, false); err != nil {
			return err
		}
	}
	program := tea.NewProgram(newUI(w), tea.WithAltScreen(), tea.WithMouseCellMotion())
	server := &http.Server{Handler: w.handler(), MaxHeaderBytes: 32 << 10, ReadHeaderTimeout: 5 * time.Second, ReadTimeout: 15 * time.Second, IdleTimeout: 60 * time.Second}
	go server.Serve(ln)
	defer server.Close()
	if err = writePrivate(filepath.Join(p.dir, "endpoint"), []byte(w.endpoint)); err != nil {
		return err
	}
	if _, err = p.tm("set-option", "-t", "kowloon", "@ready", "1"); err != nil {
		return err
	}
	_, err = program.Run()
	return err
}
