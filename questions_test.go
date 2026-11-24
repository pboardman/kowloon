package main

import (
	"encoding/json"
	"errors"
	"strings"
	"testing"
)

func TestQuestionScopeAndResolution(t *testing.T) {
	w := newWorkspace(runtimePaths{}, "")
	r := run{ID: "turn", Thread: "thread", Terminal: "%4"}
	params := `{"threadId":"thread","turnId":"turn","itemId":"item","questions":[{"id":"q","question":"Private question?"}]}`
	msg := rpcMessage{ID: json.RawMessage("4"), Method: "item/tool/requestUserInput", Params: json.RawMessage(params)}
	terminal := &tracked{term: Terminal{ID: r.Terminal, Status: Status{State: "working"}}}
	w.terms[r.Terminal] = terminal
	w.observeQuestion(r, rpcMessage{ID: msg.ID, Method: "item/commandExecution/requestApproval", Params: msg.Params})
	for _, invalid := range []string{
		strings.Replace(params, `"turn"`, `"another"`, 1),
		strings.Replace(params, `"thread"`, `"another"`, 1),
		strings.Replace(params, `"id":"q"`, `"id":"q","isSecret":true`, 1),
	} {
		w.observeQuestion(r, rpcMessage{ID: msg.ID, Method: msg.Method, Params: json.RawMessage(invalid)})
	}
	if len(terminal.notices) != 0 {
		t.Fatal("unexpected event for an unrelated or private request")
	}
	w.observeQuestion(r, msg)
	w.observeQuestion(r, msg)
	if len(terminal.notices) != 1 || terminal.notices[0].Subject != r.ID || terminal.notices[0].Type != "question.opened" || terminal.notices[0].Reason != "" {
		t.Fatal(terminal.notices)
	}
	out, _ := w.listQuestions(questionFilter{RunID: r.ID})
	if len(out.Requests) != 1 {
		t.Fatal(out)
	}
	id := out.Requests[0].RequestID
	w.observeQuestion(r, rpcMessage{Method: "serverRequest/resolved", Params: json.RawMessage(`{"threadId":"thread","requestId":4}`)})
	// The same model question ID can appear again with a different request ID.
	msg.ID = json.RawMessage("5")
	w.observeQuestion(r, msg)
	got, _ := w.listQuestions(questionFilter{})
	if len(got.Requests) != 1 || got.Requests[0].RequestID == id {
		t.Fatal(got)
	}
	if _, err := w.answerQuestion(t.Context(), answerArgs{RequestID: id, Answers: map[string]string{"q": "old"}}); !errors.Is(err, errStaleQuestion) {
		t.Fatal(err)
	}
}

func TestQuestionAnswerValidation(t *testing.T) {
	q := pendingQuestion{questionRequest: questionRequest{Questions: []codexQuestion{
		{ID: "color", Options: []questionOption{{Label: "Green"}}},
		{ID: "note"},
	}}}
	for _, values := range []map[string]string{
		{"color": "Green"}, {"color": "Green", "wrong": "x"}, {"color": "Green", "note": " "}, {"color": "Blue", "note": "x"},
	} {
		if _, err := q.encodeAnswers(values); err == nil {
			t.Fatal("accepted", values)
		}
	}
	values := map[string]string{"color": "Green", "note": "write this verbatim\n"}
	out, err := q.encodeAnswers(values)
	if err != nil || out["note"].Answers[0] != values["note"] || out["color"].Answers[0] != "Green" {
		t.Fatal(out, err)
	}
}
