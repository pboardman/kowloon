package main

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"
)

func TestQueueReplayResetAndCancellation(t *testing.T) {
	w := newWorkspace(runtimePaths{}, "")
	read := func(cursor string) eventPage {
		t.Helper()
		page, err := w.readEvents(t.Context(), eventArgs{Cursor: cursor})
		if err != nil {
			t.Fatal(err)
		}
		return page
	}
	first := read("").Next
	term := &tracked{term: Terminal{ID: "%1"}}
	w.mu.Lock()
	for i := 0; i < eventBatchSize+3; i++ {
		w.status(term, states[i%len(states)], "")
	}
	w.mu.Unlock()
	for range 2 {
		page := read(first)
		if len(page.Events) != eventBatchSize || page.Events[0].Revision != 1 || page.Reset {
			t.Fatal(page)
		}
		tail := read(page.Next)
		if len(tail.Events) != 3 || tail.Events[2].Revision != eventBatchSize+3 {
			t.Fatal(tail)
		}
	}
	w.mu.Lock()
	for i := 0; i < eventCapacity; i++ {
		w.status(term, states[i%len(states)], "")
	}
	w.mu.Unlock()
	if !read(first).Reset || !read(newWorkspace(runtimePaths{}, "").events.instance+"-0").Reset {
		t.Fatal("expected gap")
	}
	head := read("").Next
	ready := make(chan eventPage, 1)
	go func() { page, _ := w.readEvents(t.Context(), eventArgs{Cursor: head, Wait: 5}); ready <- page }()
	w.applyEvent(statusEvent{Pane: "%not-tracked", State: "waiting", Reason: "question"})
	w.mu.Lock()
	w.status(term, "waiting", "question")
	w.mu.Unlock()
	select {
	case page := <-ready:
		if len(page.Events) != 1 || page.Events[0].Type != "terminal.waiting" {
			t.Fatal(page)
		}
	case <-time.After(time.Second):
		t.Fatal("no wake-up")
	}
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	if _, err := w.readEvents(ctx, eventArgs{Cursor: read("").Next, Wait: 30}); !errors.Is(err, context.Canceled) {
		t.Fatal(err)
	}
}

func TestQueueBearerScopesOverHTTP(t *testing.T) {
	w := newWorkspace(runtimePaths{}, "")
	w.key = "control-fixture"
	w.eventKey = "events-fixture"
	srv := httptest.NewServer(w.handler())
	defer srv.Close()
	connect := func(key string) *mcp.ClientSession {
		t.Helper()
		client := mcp.NewClient(&mcp.Implementation{Name: "test", Version: "1"}, nil)
		session, err := client.Connect(t.Context(), &mcp.StreamableClientTransport{Endpoint: srv.URL + "/mcp", HTTPClient: &http.Client{Transport: bearerTransport(key)}, DisableStandaloneSSE: true, MaxRetries: -1}, nil)
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { session.Close() })
		return session
	}
	events := connect(w.eventKey)
	defs, err := events.ListTools(t.Context(), nil)
	if err != nil || len(defs.Tools) != 1 || defs.Tools[0].Name != "read_events" {
		t.Fatal(defs, err)
	}
	call, err := events.CallTool(t.Context(), &mcp.CallToolParams{Name: "list_workspace", Arguments: noArgs{}})
	if err == nil && !call.IsError {
		t.Fatal("event key can read private workspace data")
	}
	begin, err := events.ReadResource(t.Context(), &mcp.ReadResourceParams{URI: "queue://events/head"})
	if err != nil {
		t.Fatal(err)
	}
	var head eventPage
	if err = json.Unmarshal([]byte(begin.Contents[0].Text), &head); err != nil {
		t.Fatal(err)
	}
	w.mu.Lock()
	w.status(&tracked{term: Terminal{ID: "%2", CWD: "private-path", Title: "private-screen"}}, "blocked", "permission")
	w.mu.Unlock()
	for _, session := range []*mcp.ClientSession{events, connect(w.key)} {
		res, err := session.ReadResource(t.Context(), &mcp.ReadResourceParams{URI: "queue://events/" + head.Next})
		if err != nil {
			t.Fatal(err)
		}
		data := res.Contents[0].Text
		if strings.Contains(data, "private-") || !strings.Contains(data, "terminal.blocked") {
			t.Fatal(data)
		}
	}
	for _, token := range []string{"", "Bearer ", "Bearer bad"} {
		req, _ := http.NewRequest("POST", srv.URL+"/mcp", nil)
		req.Header.Set("Authorization", token)
		resp, err := srv.Client().Do(req)
		if err != nil {
			t.Fatal(err)
		}
		resp.Body.Close()
		if resp.StatusCode != 401 {
			t.Fatal(resp.StatusCode)
		}
	}
}
