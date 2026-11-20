package main

import (
	"bytes"
	"encoding/json"
	"io"
	"net"
	"path/filepath"
	"strings"
	"time"
)

type Status struct {
	State    string `json:"state"`
	Reason   string `json:"reason,omitempty"`
	Revision uint64 `json:"revision"`
}
type statusEvent struct {
	Pane   string
	State  string
	Reason string
}

// Codex writes OSC 7501 to its PTY. tmux pipe-pane delivers the original bytes,
// including in parked panes. Retain only an incomplete record between reads.
var oscStart = []byte("\x1b]7501;")

type statusParser struct{ pending []byte }

func (p *statusParser) feed(data []byte, emit func(string, string)) {
	b := append(p.pending, data...)
	p.pending = nil
	for len(b) > 0 {
		i := bytes.Index(b, oscStart)
		if i < 0 {
			for n := min(len(b), len(oscStart)-1); n > 0; n-- {
				if bytes.Equal(b[len(b)-n:], oscStart[:n]) {
					p.pending = append([]byte(nil), b[len(b)-n:]...)
					break
				}
			}
			return
		}
		b = b[i:]
		end := bytes.IndexAny(b[len(oscStart):], "\x07\x1b")
		if end < 0 {
			if len(b) <= 1024 {
				p.pending = append([]byte(nil), b...)
			}
			return
		}
		end += len(oscStart)
		if b[end] == 0x1b {
			if end+1 == len(b) {
				if len(b) <= 1024 {
					p.pending = append([]byte(nil), b...)
				}
				return
			}
			if b[end+1] != '\\' {
				b = b[end:]
				continue
			}
		}
		record := b[len(oscStart):end]
		b = b[end+1:]
		if len(record) > 1024 {
			continue
		}
		var state, app, kind string
		for _, part := range strings.Split(string(record), ":") {
			k, v, _ := strings.Cut(part, "=")
			switch k {
			case "state":
				state = v
			case "kind":
				kind = v
			case "app":
				app = v
			}
		}
		if state == "clear" {
			emit("shell", "")
			continue
		}
		if app != "codex" {
			continue
		}
		switch state {
		case "idle", "working", "done", "error":
			emit(state, "")
		case "blocked":
			switch kind {
			case "question":
				emit("waiting", "question")
			case "auth", "permission":
				emit("blocked", kind)
			default:
				emit("blocked", "")
			}
		}
	}
}

func watch(p runtimePaths, pane string, r io.Reader) error {
	c, err := net.Dial("unixgram", filepath.Join(p.dir, "events"))
	if err != nil {
		return err
	}
	defer c.Close()
	var parser statusParser
	buf := make([]byte, 32768)
	for {
		n, err := r.Read(buf)
		parser.feed(buf[:n], func(s, reason string) {
			b, _ := json.Marshal(statusEvent{pane, s, reason})
			c.SetWriteDeadline(time.Now().Add(time.Second))
			_, _ = c.Write(b)
		})
		if err == io.EOF {
			return nil
		}
		if err != nil {
			return err
		}
	}
}
