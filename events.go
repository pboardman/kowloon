package main

import (
	"context"
	"errors"
	"strconv"
	"strings"
	"time"
)

const eventCapacity = 512
const eventBatchSize = 64

type queueEvent struct {
	Type     string `json:"type"`
	Subject  string `json:"subject"`
	Revision uint64 `json:"revision"`
	Reason   string `json:"reason,omitempty"`
}

type eventLog struct {
	instance string
	sequence uint64
	ring     [eventCapacity]queueEvent
	waiters  chan struct{}
}

type eventArgs struct {
	Cursor string `json:"cursor,omitempty" jsonschema:"Cursor from the previous read. Omit to start at the current head."`
	Wait   int    `json:"wait_seconds,omitempty" jsonschema:"Wait for new events for up to 30 seconds."`
}

type eventPage struct {
	Events []queueEvent `json:"events"`
	Next   string       `json:"next_cursor"`
	Reset  bool         `json:"reset"`
}

func (l *eventLog) append(event queueEvent) {
	l.ring[l.sequence%eventCapacity] = event
	l.sequence++
}

func (l *eventLog) page(cursor string) (eventPage, error) {
	end := l.sequence
	p := eventPage{Events: []queueEvent{}}
	if cursor != "" {
		instance, number, ok := strings.Cut(cursor, "-")
		seq, err := strconv.ParseUint(number, 10, 64)
		if !ok || err != nil || len(instance) != len(l.instance) {
			return p, errors.New("invalid event cursor")
		}
		if instance != l.instance || seq > end || end-seq > eventCapacity {
			p.Reset = true
		} else {
			count := min(end-seq, eventBatchSize)
			end = seq + count
			for i := seq; i < end; i++ {
				p.Events = append(p.Events, l.ring[i%eventCapacity])
			}
		}
	}
	p.Next = l.instance + "-" + strconv.FormatUint(end, 10)
	return p, nil
}

func (w *workspace) readEvents(ctx context.Context, a eventArgs) (eventPage, error) {
	if a.Wait < 0 || a.Wait > 30 || len(a.Cursor) > 80 {
		return eventPage{}, errors.New("invalid cursor or wait_seconds outside 0 to 30")
	}
	timer := time.NewTimer(time.Duration(a.Wait) * time.Second)
	defer timer.Stop()
	waiting := false
	for {
		w.mu.Lock()
		p, err := w.events.page(a.Cursor)
		changed := w.changed
		w.mu.Unlock()
		if err != nil || p.Reset || len(p.Events) > 0 || a.Cursor == "" || a.Wait == 0 {
			return p, err
		}
		if !waiting {
			select {
			case w.events.waiters <- struct{}{}:
				defer func() { <-w.events.waiters }()
				waiting = true
			default:
				return eventPage{}, errors.New("too many waiting readers")
			}
		}
		select {
		case <-ctx.Done():
			return eventPage{}, ctx.Err()
		case <-timer.C:
			a.Wait = 0
		case <-changed:
		}
	}
}
