package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"net"
	"net/http"
	"net/netip"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"time"

	tea "github.com/charmbracelet/bubbletea"
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
	restart := fs.Bool("restart", false, "reload the Kowloon server and keep live terminal processes")
	fresh := fs.Bool("no-restore", false, "skip the saved workspace when starting a new tmux session")
	stop := fs.Bool("stop", false, "end the workspace and terminate all its running terminals")
	fs.Usage = func() {
		fmt.Fprintln(fs.Output(), "Usage: kowloon [--detach] [--no-restore] [directory] | kowloon --restart | kowloon --stop\n\nRestore the saved workspace or reattach to an existing one. Supply a directory to open a shell there.\nDrag the divider to resize. Press Ctrl+b, then d to detach.")
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
		if *detach || *restart || *fresh || len(a) != 0 {
			return errors.New("use --stop alone to end all workspace terminals")
		}
		lock, err := lockWorkspace(p)
		if err != nil {
			return err
		}
		defer lock.Close()
		flushRunningSession(p)
		_, err = p.tm("kill-session", "-t", "=kowloon")
		return err
	}
	if len(a) > 0 {
		switch a[0] {
		case "_watch":
			if len(a) == 2 {
				return watch(p, "%"+a[1], os.Stdin)
			}
		case "_codex":
			if len(a) == 4 || len(a) == 5 {
				thread := ""
				if len(a) == 5 {
					thread = a[4]
				}
				return runCodexPane(a[1], a[2], a[3], thread)
			}
		case "_run":
			if len(a) >= 3 {
				if _, err := p.tm("wait-for", a[1]); err != nil {
					return err
				}
				return syscall.Exec(a[2], a[2:], os.Environ())
			}
		case "_sidebar":
			if len(a) == 4 {
				addr, err := netip.ParseAddrPort(a[3])
				if err != nil {
					return err
				}
				return serve(p, a[1], a[2], addr)
			}
			if len(a) == 2 || len(a) == 3 {
				addr, err := p.mcpAddress()
				if err != nil {
					return err
				}
				mode := "restore"
				if len(a) == 3 {
					mode = a[2]
				}
				return serve(p, a[1], mode, addr)
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
	if !*restart && !*detach && !term.IsTerminal(int(os.Stdin.Fd())) {
		return errors.New("run kowloon in a terminal or use --detach")
	}
	if err := start(p, path, *restart, *fresh); err != nil {
		return err
	}
	if *detach || *restart {
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

func lockWorkspace(p runtimePaths) (*os.File, error) {
	lock, err := os.OpenFile(filepath.Join(p.dir, "start.lock"), os.O_CREATE|os.O_RDWR, 0600)
	if err != nil {
		return nil, err
	}
	if err = unix.Flock(int(lock.Fd()), unix.LOCK_EX); err != nil {
		lock.Close()
		return nil, err
	}
	return lock, nil
}

func start(p runtimePaths, path string, restart, fresh bool) error {
	lock, err := lockWorkspace(p)
	if err != nil {
		return err
	}
	defer lock.Close()
	if err := writePrivate(filepath.Join(p.dir, "tmux.conf"), tmuxConfig); err != nil {
		return err
	}
	addr, err := p.mcpAddress()
	if err != nil {
		return err
	}
	_, existing := p.tm("has-session", "-t", "=kowloon")
	mode := "restore"
	if fresh {
		mode = "fresh"
	}
	if existing == nil {
		dead, err := p.tm("display-message", "-p", "-t", "kowloon:workspace.0", "#{pane_dead}")
		if err != nil {
			return err
		}
		if dead == "1" || restart {
			cmd, err := p.tm("display-message", "-p", "-t", "kowloon:workspace.0", "#{pane_start_command}")
			if err != nil {
				return err
			}
			if !strings.Contains(cmd, " _sidebar ") {
				return errors.New("could not identify the Kowloon sidebar to restart")
			}
			if dead != "1" {
				flushRunningSession(p)
			}

			if _, err = p.tm("set-option", "-t", "kowloon", "@ready", "0"); err != nil {
				return err
			}
			if _, err = p.tmInDir(p.dir, "respawn-pane", "-k", "-t", "kowloon:workspace.0", "-c", ".", p.exe, "_sidebar", "", "live", addr.String()); err != nil {
				return err
			}
		}
	} else {
		width, height, err := term.GetSize(int(os.Stdout.Fd()))
		if err != nil || width < 20 || height < 5 {
			width, height = 120, 40
		}
		args := []string{"new-session", "-d", "-x", strconv.Itoa(width), "-y", strconv.Itoa(height), "-s", "kowloon", "-n", "workspace"}
		_, err = p.tmInDir(path, append(args, p.exe, "_sidebar", path, mode, addr.String())...)
		if err != nil {
			return err
		}
	}
	for range 600 {
		v, err := p.tm("show-options", "-qv", "-t", "kowloon", "@ready")
		if err == nil && v == "1" {
			dead, _ := p.tm("display-message", "-p", "-t", "kowloon:workspace.0", "#{pane_dead}")
			if dead == "1" {
				return errors.New("the Kowloon sidebar exited during startup")
			}
			if existing == nil && path != "" {
				return callRunning(p, "new_terminal", pathArgs{path})
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

func mcpBindAddress(value string) (netip.Addr, error) {
	if value == "" || value == "localhost" {
		return netip.MustParseAddr("127.0.0.1"), nil
	}
	ip, err := netip.ParseAddr(value)
	if err != nil || ip.IsMulticast() {
		return netip.Addr{}, errors.New("bind must be localhost or a local IP address")
	}
	ip = ip.Unmap()
	if !ip.IsUnspecified() && !localMCPHost(ip.String()) {
		return netip.Addr{}, fmt.Errorf("bind address %s is not assigned to this machine", ip)
	}
	return ip, nil
}

func localMCPHost(host string) bool {
	ip, err := netip.ParseAddr(host)
	if err != nil || ip.IsUnspecified() {
		return false
	}
	if ip.IsLoopback() {
		return true
	}
	// Only send the key directly to an address assigned to this machine.
	addrs, err := net.InterfaceAddrs()
	if err != nil {
		return false
	}
	for _, addr := range addrs {
		prefix, err := netip.ParsePrefix(addr.String())
		if err == nil && prefix.Addr().Unmap() == ip.WithZone("").Unmap() {
			return true
		}
	}
	return false
}

var localHTTPTransport = &http.Transport{} // A local control request must bypass HTTP proxies.

type bearerTransport string

func (token bearerTransport) RoundTrip(r *http.Request) (*http.Response, error) {
	if r.URL.Scheme != "http" || !localMCPHost(r.URL.Hostname()) {
		return nil, errors.New("invalid local MCP endpoint")
	}
	copy := r.Clone(r.Context())
	copy.Header.Set("Authorization", "Bearer "+string(token))
	return localHTTPTransport.RoundTrip(copy)
}

// Older controllers do not offer save_session. Their panes can still be adopted.
func flushRunningSession(p runtimePaths) {
	instance, _ := p.tm("show-options", "-qv", "-t", "kowloon", "@kowloon_instance")
	if instance != "" {
		if err := callRunning(p, "save_session", noArgs{}); err != nil {
			fmt.Fprintln(os.Stderr, "Kowloon could not update its last saved session:", err)
		}
	}
}

func callRunning(p runtimePaths, name string, args any) error {
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
	result, err := session.CallTool(ctx, &mcp.CallToolParams{Name: name, Arguments: args})
	if err != nil {
		return err
	}
	if result.IsError {
		for _, content := range result.Content {
			if msg, ok := content.(*mcp.TextContent); ok {
				return errors.New(msg.Text)
			}
		}
		return fmt.Errorf("%s failed", name)
	}
	return nil
}

func serve(p runtimePaths, path, mode string, address netip.AddrPort) error {
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
	host := address.Addr()
	network := "tcp4"
	if host.Is6() {
		network = "tcp6"
	}
	addr := address.String()
	ln, err := net.Listen(network, addr)
	if mode == "live" {
		for attempt := 0; attempt < 20 && errors.Is(err, syscall.EADDRINUSE); attempt++ {
			time.Sleep(50 * time.Millisecond)
			ln, err = net.Listen(network, addr)
		}
	}
	if err != nil {
		return err
	}
	defer ln.Close()
	// Use loopback for wildcard listeners and the bound interface otherwise.
	endpoint := ln.Addr().(*net.TCPAddr).AddrPort()
	if host.IsUnspecified() {
		loopback := netip.IPv6Loopback()
		if host.Is4() {
			loopback = netip.MustParseAddr("127.0.0.1")
		}
		endpoint = netip.AddrPortFrom(loopback, endpoint.Port())
	}
	w.endpoint = (&url.URL{Scheme: "http", Host: endpoint.String(), Path: "/mcp"}).String()
	eventPath := filepath.Join(p.dir, "events")
	os.Remove(eventPath)
	events, err := net.ListenUnixgram("unixgram", &net.UnixAddr{Name: eventPath, Net: "unixgram"})
	if err != nil {
		return err
	}
	defer events.Close()

	if err = w.recoverSession(path, mode); err != nil {
		return err
	}
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
	model := newUI(w)
	if len(w.pending) > 0 {
		model.notice = fmt.Sprintf("Restore skipped %d unavailable terminals", len(w.pending))
	}
	program := tea.NewProgram(model, tea.WithAltScreen(), tea.WithMouseCellMotion())
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
