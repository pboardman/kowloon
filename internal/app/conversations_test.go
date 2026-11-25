package app

import (
	"context"
	"errors"
	"testing"
	"time"
)

func TestConversationWaitUsesListedIDAcrossCalls(t *testing.T) {
	w := newWorkspace(runtimePaths{}, "")
	for _, id := range []string{"%1", "%2", "%3"} {
		t := &tracked{term: Terminal{ID: id}}
		w.terms[id] = t
		w.status(t, "shell", "")
	}
	a, b := w.terms["%1"], w.terms["%2"]
	w.status(a, "done", "")
	w.status(b, "working", "")
	l := w.listConversations()
	if len(l.Conversations) != 2 || l.Conversations[0].ID != a.term.ID || l.Conversations[0].State != "done" {
		t.Fatal(l)
	}
	id := l.Conversations[0].ID
	next := func() conversationEvent {
		t.Helper()
		got, err := w.nextConversationEvent(t.Context(), id, time.Millisecond)
		if err != nil {
			t.Fatal(err)
		}
		return got
	}
	if !next().TimedOut {
		t.Fatal("expected to wait after listing current state")
	}
	w.status(a, "working", "")
	w.status(b, "error", "")
	w.status(a, "blocked", "auth")
	w.listConversations()
	w.status(a, "done", "")
	for _, typ := range []string{"terminal.blocked", "terminal.done"} {
		got := next()
		if got.Event == nil || got.Event.Subject != id || got.Event.Type != typ {
			t.Fatal(got)
		}
	}
	if !next().TimedOut {
		t.Fatal("duplicate or unrelated event")
	}
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	if _, err := w.nextConversationEvent(ctx, id, time.Hour); !errors.Is(err, context.Canceled) {
		t.Fatal(err)
	}
	w.status(a, "shell", "")
	if got := next(); got.Event == nil || got.Event.Type != "terminal.shell" {
		t.Fatal(got)
	}
	if _, err := w.nextConversationEvent(t.Context(), id, time.Hour); err == nil {
		t.Fatal("waiting on closed agent")
	}
}

func TestConversationHistoryOverflowIsLocal(t *testing.T) {
	w := newWorkspace(runtimePaths{}, "")
	for _, id := range []string{"%1", "%2"} {
		w.terms[id] = &tracked{term: Terminal{ID: id, Status: Status{State: "working"}}}
	}
	w.listConversations()
	w.status(w.terms["%1"], "done", "")
	for range maxConversationNotices + 1 {
		w.status(w.terms["%2"], "working", "")
		w.status(w.terms["%2"], "done", "")
	}
	for _, id := range []string{"%1", "%2"} {
		got, err := w.nextConversationEvent(t.Context(), id, time.Millisecond)
		if err != nil || got.Event == nil || got.Event.Subject != id || got.HistoryGap != (id == "%2") {
			t.Fatalf("%s: %+v, %v", id, got, err)
		}
	}
}
