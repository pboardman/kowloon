package main

import (
	"bytes"
	"crypto/rand"
	_ "embed"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
)

//go:embed tmux.conf
var tmuxConfig []byte

type runtimePaths struct{ dir, config, data, exe string }

func privateDir(path string) error {
	if err := os.MkdirAll(path, 0700); err != nil {
		return err
	}
	s, err := os.Lstat(path)
	if err != nil {
		return err
	}
	if !s.IsDir() || s.Mode().Perm()&0077 != 0 || s.Sys().(*syscall.Stat_t).Uid != uint32(os.Getuid()) {
		return fmt.Errorf("Kowloon needs a private directory owned by your account: %s", path)
	}
	return nil
}

func paths() (runtimePaths, error) {
	var p runtimePaths
	base := os.Getenv("XDG_RUNTIME_DIR")
	if base != "" {
		p.dir = filepath.Join(base, "kowloon")
	} else {
		p.dir = filepath.Join(os.TempDir(), "kowloon-"+strconv.Itoa(os.Getuid()))
	}
	config, err := os.UserConfigDir()
	if err != nil {
		return p, err
	}
	p.config = filepath.Join(config, "kowloon")
	data := os.Getenv("XDG_DATA_HOME")
	if data == "" {
		home, err := os.UserHomeDir()
		if err != nil {
			return p, err
		}
		data = filepath.Join(home, ".local", "share")
	}
	p.data = filepath.Join(data, "kowloon")
	p.exe, err = os.Executable()
	if err != nil {
		return p, err
	}
	for _, d := range []string{p.dir, p.config} {
		if err := privateDir(d); err != nil {
			return p, err
		}
	}
	return p, nil
}

func publishPrivate(path string, data []byte, publish func(string, string) error) error {
	f, err := os.CreateTemp(filepath.Dir(path), ".kowloon-*")
	if err != nil {
		return err
	}
	defer os.Remove(f.Name())
	if _, err = f.Write(data); err != nil {
		f.Close()
		return err
	}
	if err = f.Close(); err != nil {
		return err
	}
	return publish(f.Name(), path)
}

func writePrivate(path string, data []byte) error { return publishPrivate(path, data, os.Rename) }

func secret(p runtimePaths) (string, error) {
	path := filepath.Join(p.config, "mcp.key")
	if _, err := os.Lstat(path); errors.Is(err, os.ErrNotExist) {
		// Separate login sessions can share this key. Only the first publisher wins.
		if err := publishPrivate(path, []byte(randomID(32)), os.Link); err != nil && !errors.Is(err, os.ErrExist) {
			return "", err
		}
	}
	s, err := os.Lstat(path)
	if err != nil {
		return "", err
	}
	if !s.Mode().IsRegular() || s.Mode().Perm()&0077 != 0 {
		return "", fmt.Errorf("restrict %s to its owner", path)
	}
	b, err := os.ReadFile(path)
	if err != nil {
		return "", err
	}
	k := strings.TrimSpace(string(b))
	if len(k) < 32 {
		return "", errors.New("MCP key is empty or too short")
	}
	return k, nil
}

func randomID(n int) string {
	b := make([]byte, n)
	if _, err := rand.Read(b); err != nil {
		panic(err)
	}
	return hex.EncodeToString(b)
}

func command(input io.Reader, name string, args ...string) (string, error) {
	cmd := exec.Command(name, args...)
	cmd.Stdin = input
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	out, err := cmd.Output()
	if err != nil {
		return "", fmt.Errorf("%s: %w %s", name, err, strings.TrimSpace(stderr.String()))
	}
	return strings.TrimSuffix(string(out), "\n"), nil
}

func (p runtimePaths) tmArgs(args ...string) []string {
	return append([]string{"-S", filepath.Join(p.dir, "tmux"), "-f", filepath.Join(p.dir, "tmux.conf")}, args...)
}
func (p runtimePaths) tm(args ...string) (string, error) {
	return command(nil, "tmux", p.tmArgs(args...)...)
}
func (p runtimePaths) tmInput(input string, args ...string) (string, error) {
	return command(strings.NewReader(input), "tmux", p.tmArgs(args...)...)
}
func quote(s string) string { return "'" + strings.ReplaceAll(s, "'", "'\"'\"'") + "'" }

// tmux expands time formats and tmux formats before passing pipe-pane to a shell.
func watcherCommand(exe, pane string) string {
	literal := strings.NewReplacer("%", "%%", "#", "##").Replace(quote(exe))
	return "exec " + literal + " _watch " + strings.TrimPrefix(pane, "%")
}

func directory(path string) (string, error) {
	if path == "~" || strings.HasPrefix(path, "~/") {
		home, err := os.UserHomeDir()
		if err != nil {
			return "", err
		}
		path = filepath.Join(home, strings.TrimPrefix(strings.TrimPrefix(path, "~"), "/"))
	}
	if path == "" {
		return "", errors.New("enter a directory")
	}
	abs, err := filepath.Abs(path)
	if err != nil {
		return "", err
	}
	abs, err = filepath.EvalSymlinks(abs)
	if err != nil {
		return "", err
	}
	s, err := os.Stat(abs)
	if err != nil {
		return "", err
	}
	if !s.IsDir() {
		return "", fmt.Errorf("%s is not a directory", abs)
	}
	return abs, nil
}
