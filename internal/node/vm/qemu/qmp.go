package qemu

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"sync"
	"time"
)

// qmp is a minimal QEMU Machine Protocol client: one command at a time,
// asynchronous events fanned out to subscribers. done closes when the
// connection ends, which for a QMP server socket means qemu exited.
type qmp struct {
	c    net.Conn
	mu   sync.Mutex // one command in flight
	id   int
	resp chan qmpMsg

	evMu sync.Mutex
	subs map[int]func(qmpEvent)
	next int

	done chan struct{}
}

type qmpMsg struct {
	Event  string          `json:"event"`
	Data   json.RawMessage `json:"data"`
	Return json.RawMessage `json:"return"`
	Error  *struct {
		Class string `json:"class"`
		Desc  string `json:"desc"`
	} `json:"error"`
	ID  *int            `json:"id"`
	QMP json.RawMessage `json:"QMP"`
}

type qmpEvent struct {
	Name string
	Data json.RawMessage
}

func dialQMP(ctx context.Context, network, addr string) (*qmp, error) {
	c, err := (&net.Dialer{}).DialContext(ctx, network, addr)
	if err != nil {
		return nil, err
	}
	c.SetDeadline(time.Now().Add(5 * time.Second))
	dec := json.NewDecoder(c)
	var greet qmpMsg
	if err := dec.Decode(&greet); err != nil || greet.QMP == nil {
		c.Close()
		return nil, fmt.Errorf("qmp greeting: %v", err)
	}
	c.SetDeadline(time.Time{})
	q := &qmp{c: c, resp: make(chan qmpMsg, 16), subs: map[int]func(qmpEvent){}, done: make(chan struct{})}
	go q.read(dec)
	if err := q.cmd(ctx, "qmp_capabilities", nil, nil); err != nil {
		c.Close()
		return nil, err
	}
	return q, nil
}

func (q *qmp) read(dec *json.Decoder) {
	defer close(q.done)
	for {
		var m qmpMsg
		if err := dec.Decode(&m); err != nil {
			return
		}
		if m.Event != "" {
			q.evMu.Lock()
			for _, fn := range q.subs {
				fn(qmpEvent{m.Event, m.Data})
			}
			q.evMu.Unlock()
			continue
		}
		select {
		case q.resp <- m:
		default: // nobody waiting (timed-out command); drop
		}
	}
}

// subscribe calls fn (from the reader goroutine; must not block) for every
// event until the returned cancel func is called.
func (q *qmp) subscribe(fn func(qmpEvent)) (cancel func()) {
	q.evMu.Lock()
	defer q.evMu.Unlock()
	q.next++
	id := q.next
	q.subs[id] = fn
	return func() {
		q.evMu.Lock()
		delete(q.subs, id)
		q.evMu.Unlock()
	}
}

var errQMPClosed = errors.New("qmp: connection closed (qemu exited)")

// cmd runs one command and decodes its return value into out (if non-nil).
func (q *qmp) cmd(ctx context.Context, name string, args any, out any) error {
	q.mu.Lock()
	defer q.mu.Unlock()
	q.id++
	id := q.id
	req := map[string]any{"execute": name, "id": id}
	if args != nil {
		req["arguments"] = args
	}
	b, _ := json.Marshal(req)
	select {
	case <-q.done:
		return fmt.Errorf("qmp %s: %w", name, errQMPClosed)
	default:
	}
	if _, err := q.c.Write(append(b, '\n')); err != nil {
		return fmt.Errorf("qmp %s: %w (%v)", name, errQMPClosed, err)
	}
	for {
		select {
		case m := <-q.resp:
			if m.ID == nil || *m.ID != id {
				continue // stale reply of an abandoned command
			}
			if m.Error != nil {
				return fmt.Errorf("qmp %s: %s: %s", name, m.Error.Class, m.Error.Desc)
			}
			if out != nil {
				return json.Unmarshal(m.Return, out)
			}
			return nil
		case <-q.done:
			return fmt.Errorf("qmp %s: %w", name, errQMPClosed)
		case <-ctx.Done():
			return fmt.Errorf("qmp %s: %w", name, ctx.Err())
		}
	}
}

func (q *qmp) close() { q.c.Close() }

// waitEvent runs a command and waits for the first event accepted by match
// (subscribed before the command is sent, so it cannot be missed).
func (q *qmp) cmdWaitEvent(ctx context.Context, name string, args any, match func(qmpEvent) bool) (qmpEvent, error) {
	ch := make(chan qmpEvent, 1)
	cancel := q.subscribe(func(e qmpEvent) {
		if match(e) {
			select {
			case ch <- e:
			default:
			}
		}
	})
	defer cancel()
	if err := q.cmd(ctx, name, args, nil); err != nil {
		return qmpEvent{}, err
	}
	select {
	case e := <-ch:
		return e, nil
	case <-q.done:
		return qmpEvent{}, errQMPClosed
	case <-ctx.Done():
		return qmpEvent{}, ctx.Err()
	}
}
