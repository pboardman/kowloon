package app

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"slices"
	"sort"
	"strings"
	"time"
)

type questionOption struct {
	Label       string `json:"label"`
	Description string `json:"description"`
}

type codexQuestion struct {
	ID       string           `json:"id"`
	Header   string           `json:"header"`
	Question string           `json:"question"`
	Options  []questionOption `json:"options,omitempty"`
	IsOther  bool             `json:"isOther" jsonschema:"Allows a written answer alongside the listed choices"`
	IsSecret bool             `json:"isSecret,omitempty"`
}

type questionRequest struct {
	RequestID  string          `json:"request_id"`
	RunID      string          `json:"run_id"`
	TerminalID string          `json:"terminal_id"`
	Questions  []codexQuestion `json:"questions"`
}

type pendingQuestion struct {
	questionRequest
	thread, wireID string
}

func readQuestion(msg rpcMessage, r run) (pendingQuestion, bool) {
	if msg.Method != "item/tool/requestUserInput" || len(msg.ID) == 0 || string(msg.ID) == "null" {
		return pendingQuestion{}, false
	}
	var p struct {
		ThreadID, TurnID, ItemID string
		Questions                []codexQuestion
	}
	if json.Unmarshal(msg.Params, &p) != nil || p.ThreadID != r.Thread || p.TurnID != r.ID || p.ItemID == "" || len(p.Questions) == 0 {
		return pendingQuestion{}, false
	}
	ids := map[string]bool{}
	for _, q := range p.Questions {
		if q.IsSecret || q.ID == "" || ids[q.ID] {
			return pendingQuestion{}, false
		}
		ids[q.ID] = true
	}
	// Keep the handle across a live restart, and invalidate it if the request or choices change.
	identity, _ := json.Marshal([]any{p, msg.ID})
	return pendingQuestion{
		questionRequest: questionRequest{RequestID: fmt.Sprintf("%x", sha256.Sum256(identity)), RunID: r.ID, TerminalID: r.Terminal, Questions: p.Questions},
		thread:          r.Thread, wireID: string(msg.ID),
	}, true
}

func (w *workspace) observeQuestion(r run, msg rpcMessage) {
	switch msg.Method {
	case "item/tool/requestUserInput":
		q, ok := readQuestion(msg, r)
		if !ok {
			return
		}
		w.mu.Lock()
		defer w.mu.Unlock()
		if _, exists := w.questions[q.RequestID]; exists {
			return
		}
		if w.questions == nil {
			w.questions = make(map[string]pendingQuestion)
		}
		w.questions[q.RequestID] = q
		if t := w.terms[r.Terminal]; t != nil {
			t.recordNotice(conversationNotice{Type: "question.opened", Subject: r.ID, requestID: q.RequestID})
		}
		w.notify()
	case "serverRequest/resolved":
		if id := resolvedRequest(msg, r.Thread); id != "" {
			w.forgetQuestions(r.ID, id)
		}
	}
}

func resolvedRequest(msg rpcMessage, thread string) string {
	var p struct {
		ThreadID  string
		RequestID json.RawMessage
	}
	if msg.Method == "serverRequest/resolved" && json.Unmarshal(msg.Params, &p) == nil && p.ThreadID == thread {
		return string(p.RequestID)
	}
	return ""
}

// A new connection receives its own replay of the requests that are still open.
func (w *workspace) forgetQuestions(runID, wireID string) {
	w.mu.Lock()
	defer w.mu.Unlock()
	for id, q := range w.questions {
		if q.RunID == runID && (wireID == "" || q.wireID == wireID) {
			delete(w.questions, id)
		}
	}
}

type questionFilter struct {
	RunID string `json:"run_id,omitempty" jsonschema:"Run ID from submit_prompt or the subject of a question.opened event"`
}
type questionList struct {
	Requests []questionRequest `json:"requests"`
}

func (w *workspace) listQuestions(in questionFilter) (questionList, error) {
	w.mu.Lock()
	defer w.mu.Unlock()
	out := questionList{Requests: []questionRequest{}}
	for _, q := range w.questions {
		if in.RunID == "" || in.RunID == q.RunID {
			out.Requests = append(out.Requests, q.questionRequest)
		}
	}
	sort.Slice(out.Requests, func(i, j int) bool { return out.Requests[i].RequestID < out.Requests[j].RequestID })
	return out, nil
}

type answerArgs struct {
	RequestID string            `json:"request_id" jsonschema:"Exact request_id from list_questions"`
	Answers   map[string]string `json:"answers" jsonschema:"One answer per question ID. Use the exact option label or an allowed written answer."`
}
type wireAnswer struct {
	Answers []string `json:"answers"`
}
type answerReceipt struct {
	RequestID string `json:"request_id"`
	Status    string `json:"status"`
}

func (q pendingQuestion) encodeAnswers(values map[string]string) (map[string]wireAnswer, error) {
	if len(values) != len(q.Questions) {
		return nil, errors.New("supply one answer for each listed question ID")
	}
	out := make(map[string]wireAnswer, len(values))
	for _, field := range q.Questions {
		value := values[field.ID]
		if strings.TrimSpace(value) == "" {
			return nil, fmt.Errorf("missing answer for question %q", field.ID)
		}
		if len(field.Options) > 0 && !field.IsOther && !slices.ContainsFunc(field.Options, func(o questionOption) bool { return o.Label == value }) {
			return nil, fmt.Errorf("choose a listed option for question %q", field.ID)
		}
		out[field.ID] = wireAnswer{[]string{value}}
	}
	return out, nil
}

var errStaleQuestion = errors.New("this question is no longer available; use list_questions")

func (w *workspace) answerQuestion(ctx context.Context, in answerArgs) (answerReceipt, error) {
	w.mu.Lock()
	q, ok := w.questions[in.RequestID]
	w.mu.Unlock()
	if !ok {
		return answerReceipt{}, errStaleQuestion
	}
	a, err := w.agent(q.TerminalID)
	if err != nil {
		return answerReceipt{}, err
	}
	a.submit.Lock()
	defer a.submit.Unlock()
	w.mu.Lock()
	_, ok = w.questions[in.RequestID]
	w.mu.Unlock()
	if !ok {
		return answerReceipt{}, errStaleQuestion
	}
	answers, err := q.encodeAnswers(in.Answers)
	if err != nil {
		return answerReceipt{}, err
	}
	ctx, cancel := context.WithTimeout(ctx, 4*time.Second)
	defer cancel()
	c, err := dialCodex(ctx, a.dir)
	if err != nil {
		return answerReceipt{}, err
	}
	defer c.conn.Close()
	// Obtain the still-live RPC request from Codex. A reply in the TUI or
	// another MCP call can invalidate the cached question at any time.
	r := run{ID: q.RunID, Thread: q.thread, Terminal: q.TerminalID}
	var replay json.RawMessage
	resolved := false
	c.onMessage = func(msg rpcMessage) {
		if live, ok := readQuestion(msg, r); ok && live.RequestID == q.RequestID {
			replay = msg.ID
		} else if resolvedRequest(msg, q.thread) == q.wireID {
			resolved = true
		}
	}
	if err := c.rejoin(ctx, q.thread); err != nil {
		return answerReceipt{}, err
	}
	deadline, _ := ctx.Deadline()
	c.conn.SetReadDeadline(deadline)
	for replay == nil && !resolved {
		var msg rpcMessage
		if err := c.conn.ReadJSON(&msg); err != nil {
			var ne net.Error
			if errors.As(err, &ne) && ne.Timeout() {
				return answerReceipt{}, errors.New("Codex did not replay this question before the timeout; use list_questions")
			}
			return answerReceipt{}, err
		}
		c.onMessage(msg)
	}
	if resolved {
		return answerReceipt{}, errStaleQuestion
	}
	if err := ctx.Err(); err != nil {
		return answerReceipt{}, err
	}
	c.conn.SetWriteDeadline(deadline)
	if err := c.conn.WriteJSON(map[string]any{"id": replay, "result": map[string]any{"answers": answers}}); err != nil {
		return answerReceipt{}, err
	}
	w.forgetQuestions(q.RunID, q.wireID)
	return answerReceipt{q.RequestID, "sent"}, nil
}
