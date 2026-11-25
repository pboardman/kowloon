package app

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"path/filepath"
	"slices"
	"time"

	"github.com/gorilla/websocket"
)

// The stock Codex TUI and this client share one private app server per managed pane.
type codexRPC struct {
	conn      *websocket.Conn
	seq       int
	onMessage func(rpcMessage)
}

type rpcMessage struct {
	ID     json.RawMessage
	Method string
	Params json.RawMessage
	Result json.RawMessage
	Error  *rpcFault
}

func dialCodex(ctx context.Context, dir string) (*codexRPC, error) {
	d := websocket.Dialer{HandshakeTimeout: 3 * time.Second, NetDialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
		return new(net.Dialer).DialContext(ctx, "unix", filepath.Join(dir, "rpc"))
	}}
	conn, _, err := d.DialContext(ctx, "ws://localhost/", nil)
	if err != nil {
		return nil, err
	}
	conn.SetReadLimit(24 << 20)
	c := &codexRPC{conn: conn}
	if err = c.call(ctx, "initialize", map[string]any{"clientInfo": map[string]string{"name": "kowloon", "version": "1"}}, nil); err == nil {
		err = conn.WriteJSON(map[string]string{"method": "initialized"})
	}
	if err != nil {
		conn.Close()
		return nil, err
	}
	return c, nil
}

type rpcFault struct {
	Code    int
	Message string
}

func (f *rpcFault) Error() string { return fmt.Sprintf("Codex (%d): %s", f.Code, f.Message) }
func (c *codexRPC) call(ctx context.Context, method string, params, out any) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	deadline := time.Now().Add(5 * time.Second)
	if t, ok := ctx.Deadline(); ok && t.Before(deadline) {
		deadline = t
	}
	c.conn.SetReadDeadline(deadline)
	c.conn.SetWriteDeadline(deadline)
	c.seq++
	if err := c.conn.WriteJSON(map[string]any{"method": method, "id": c.seq, "params": params}); err != nil {
		return err
	}
	for {
		var msg rpcMessage
		if err := c.conn.ReadJSON(&msg); err != nil {
			return err
		}
		if msg.Method != "" && c.onMessage != nil {
			c.onMessage(msg)
		}
		if msg.Method != "" || string(msg.ID) != fmt.Sprint(c.seq) {
			continue
		}
		if msg.Error != nil {
			return msg.Error
		}
		if out == nil {
			return nil
		}
		return json.Unmarshal(msg.Result, out)
	}
}

func (c *codexRPC) rejoin(ctx context.Context, thread string) error {
	return c.call(ctx, "thread/resume", map[string]any{"threadId": thread, "excludeTurns": true}, nil)
}

type codexTurn struct{ ID, Status string }
type codexThread struct {
	ID, Cwd        string
	ParentThreadID string
	Ephemeral      bool
	Status         struct {
		Type        string
		ActiveFlags []string
	}
	Turns []codexTurn
}

func (c *codexRPC) readThread(ctx context.Context, id string, turns bool) (codexThread, error) {
	var r struct{ Thread codexThread }
	err := c.call(ctx, "thread/read", map[string]any{"threadId": id, "includeTurns": turns}, &r)
	return r.Thread, err
}

var errAmbiguousThread = errors.New("multiple Codex conversations are open in this terminal; launch a new terminal for prompt submission")

func (c *codexRPC) rootThread(ctx context.Context, cwd string) (codexThread, error) {
	var r struct {
		Data       []string
		NextCursor string
	}
	var root codexThread
	if err := c.call(ctx, "thread/loaded/list", map[string]int{"limit": 100}, &r); err != nil {
		return root, err
	}
	if r.NextCursor != "" {
		return root, errors.New("too many loaded Codex threads to identify the active terminal")
	}
	for _, id := range r.Data {
		t, err := c.readThread(ctx, id, false)
		if err != nil {
			return root, err
		}
		if t.Ephemeral || t.ParentThreadID != "" || t.Cwd != cwd {
			continue
		}
		if root.ID != "" {
			return root, errAmbiguousThread
		}
		root = t
	}
	return root, nil
}

func turnState(turn codexTurn, t codexThread) (string, error) {
	switch turn.Status {
	case "completed":
		return "finished", nil
	case "failed":
		return "failed", nil
	case "interrupted":
		return "interrupted", nil
	case "inProgress":
		if slices.Contains(t.Status.ActiveFlags, "waitingOnApproval") {
			return "waiting_for_approval", nil
		}
		if slices.Contains(t.Status.ActiveFlags, "waitingOnUserInput") {
			return "waiting_for_input", nil
		}
		return "running", nil
	default:
		return "", fmt.Errorf("unknown Codex turn status %q", turn.Status)
	}
}
func (c *codexRPC) runState(ctx context.Context, thread, run string) (string, error) {
	t, err := c.readThread(ctx, thread, false)
	if err != nil {
		return "", err
	}
	params := map[string]any{"threadId": thread, "itemsView": "notLoaded", "limit": 32}
	for {
		var p struct {
			Data       []codexTurn
			NextCursor string
		}
		err = c.call(ctx, "thread/turns/list", params, &p)
		var fault *rpcFault
		if errors.As(err, &fault) && fault.Code == -32601 {
			t, err = c.readThread(ctx, thread, true)
			p.Data = t.Turns
		}
		if err != nil {
			return "", err
		}
		for _, v := range p.Data {
			if v.ID == run {
				return turnState(v, t)
			}
		}
		if p.NextCursor == "" || params["cursor"] == p.NextCursor {
			break
		}
		params["cursor"] = p.NextCursor
	}
	return "", errors.New("Codex has not published the requested turn")
}
