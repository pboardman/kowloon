package main

import (
	"context"
	"errors"
	"time"
)

type conversation struct {
	ID       string   `json:"conversation_id"`
	Program  string   `json:"program,omitempty"`
	Checkout Checkout `json:"checkout"`
	Title    string   `json:"title,omitempty"`
	Status
	ActiveRun        *run              `json:"active_run,omitempty"`
	PendingQuestions []questionRequest `json:"pending_questions,omitempty"`
}

type conversationList struct {
	Conversations []conversation `json:"conversations"`
}

func (w *workspace) listConversations() conversationList {
	w.mu.Lock()
	defer w.mu.Unlock()
	out := conversationList{Conversations: []conversation{}}
	for _, t := range w.snapshotLocked().Terminals {
		if t.hasAgent() {
			tracked := w.terms[t.ID]
			if !tracked.conversationListed {
				tracked.notices = nil
				tracked.missedNotices = false
				tracked.conversationListed = true
			}
			c := conversation{ID: t.ID, Program: t.Program, Checkout: t.Checkout, Title: t.Title, Status: t.Status, ActiveRun: t.ActiveRun}
			for _, q := range w.questions {
				if q.TerminalID == t.ID {
					c.PendingQuestions = append(c.PendingQuestions, q.questionRequest)
				}
			}
			out.Conversations = append(out.Conversations, c)
		}
	}
	return out
}

type conversationArgs struct {
	ID string `json:"conversation_id" jsonschema:"ID from list_conversations"`
}

type conversationNotice struct {
	Type      string `json:"type"`
	Subject   string `json:"subject"`
	Revision  uint64 `json:"revision"`
	Reason    string `json:"reason,omitempty"`
	requestID string
}

const maxConversationNotices = 512

func (t *tracked) recordNotice(n conversationNotice) {
	t.noticeRevision++
	if n.Revision == 0 {
		n.Revision = t.noticeRevision
	}
	if len(t.notices) == maxConversationNotices {
		t.notices = t.notices[1:]
		t.missedNotices = true
	}
	t.notices = append(t.notices, n)
}

type conversationEvent struct {
	Event      *conversationNotice `json:"event,omitempty"`
	Question   *questionRequest    `json:"question,omitempty"`
	TimedOut   bool                `json:"timed_out,omitempty"`
	HistoryGap bool                `json:"history_gap,omitempty"`
}

// The read position belongs to the terminal. Repeated calls, including calls
// after a timeout, continue where the previous call left off.
func (w *workspace) nextConversationEvent(ctx context.Context, id string, timeout time.Duration) (conversationEvent, error) {
	timer := time.NewTimer(timeout)
	defer timer.Stop()
	w.mu.Lock()
	defer w.mu.Unlock()
	t, err := w.lookup(id)
	if err != nil {
		return conversationEvent{}, err
	}
	t.conversationListed = true
	waiting, expired := false, false
	for {
		if err := ctx.Err(); err != nil {
			return conversationEvent{}, err
		}
		out := conversationEvent{HistoryGap: t.missedNotices}
		t.missedNotices = false
		for len(t.notices) > 0 {
			e := t.notices[0]
			t.notices[0] = conversationNotice{}
			t.notices = t.notices[1:]
			if e.requestID != "" {
				q, pending := w.questions[e.requestID]
				if !pending {
					continue
				}
				out.Question = &q.questionRequest
			}
			out.Event = &e
			return out, nil
		}
		if out.HistoryGap {
			return out, nil
		}
		if !t.term.hasAgent() {
			return out, errors.New("this agent conversation has closed; use list_conversations")
		}
		if expired {
			out.TimedOut = true
			return out, nil
		}
		if !waiting {
			if w.conversationWaiters >= 32 {
				return out, errors.New("too many waiting readers")
			}
			w.conversationWaiters++
			defer func() { w.conversationWaiters-- }()
			waiting = true
		}
		changed := w.changed
		w.mu.Unlock()
		select {
		case <-ctx.Done():
		case <-timer.C:
			expired = true
		case <-changed:
		}
		w.mu.Lock()
	}
}
