package server

import (
	"sync"
	"time"
)

// Lifecycle event types published on the Bus. The first six are Boat's
// webhook events; the rest are internal.
const (
	EvSandboxReady     = "sandbox.ready"
	EvSandboxError     = "sandbox.error"
	EvSandboxArchived  = "sandbox.archived"
	EvSandboxHydrated  = "sandbox.hydrated"
	EvSandboxDegraded  = "sandbox.degraded"
	EvSandboxRecovered = "sandbox.recovered"

	EvSandboxCreated  = "sandbox.created"  // row inserted (create, fork, from saved snapshot)
	EvSandboxState    = "sandbox.state"    // every state change (PreviousState -> State)
	EvSandboxDeleted  = "sandbox.deleted"  // deletion accepted
	EvSandboxUpdated  = "sandbox.updated"  // name/ttl/visibility change
	EvSnapshotCreated = "snapshot.created" // a snapshot row was recorded
	EvSnapshotStored  = "snapshot.stored"  // its blob reached the server
	EvGuestEvent      = "guest.event"      // raw guest event relayed by a node (Data["guest"])
	EvNodeOnline      = "node.online"
	EvNodeOffline     = "node.offline"
)

// Event is one internal event. Subscribers must not block for long: events
// are delivered in order by a single dispatcher goroutine.
type Event struct {
	ID            string         `json:"id,omitempty"` // evt_<32 hex> on lifecycle events from transitions
	Type          string         `json:"type"`
	Time          time.Time      `json:"time"`
	SandboxID     string         `json:"sandboxId,omitempty"`
	SandboxName   string         `json:"sandboxName,omitempty"`
	OwnerID       string         `json:"ownerId,omitempty"`
	OrgID         string         `json:"orgId,omitempty"`
	NodeID        string         `json:"nodeId,omitempty"`
	PreviousState string         `json:"previousState,omitempty"`
	State         string         `json:"state,omitempty"`
	Reason        string         `json:"reason,omitempty"`
	Data          map[string]any `json:"data,omitempty"`
}

// Bus is an in-process pub/sub for lifecycle events (webhooks, OTel and the
// dashboard's live view subscribe to it).
type Bus struct {
	mu   sync.RWMutex
	subs map[int]func(Event)
	next int
	ch   chan Event
	done chan struct{}
	once sync.Once
}

func NewBus() *Bus {
	b := &Bus{subs: map[int]func(Event){}, ch: make(chan Event, 4096), done: make(chan struct{})}
	go b.loop()
	return b
}

// Subscribe registers fn and returns a function that unsubscribes it.
func (b *Bus) Subscribe(fn func(Event)) (unsubscribe func()) {
	b.mu.Lock()
	id := b.next
	b.next++
	b.subs[id] = fn
	b.mu.Unlock()
	return func() {
		b.mu.Lock()
		delete(b.subs, id)
		b.mu.Unlock()
	}
}

// Publish queues e for delivery (never blocks the caller for long).
func (b *Bus) Publish(e Event) {
	if e.Time.IsZero() {
		e.Time = time.Now().UTC()
	}
	select {
	case <-b.done:
	case b.ch <- e:
	}
}

func (b *Bus) loop() {
	for {
		select {
		case <-b.done:
			return
		case e := <-b.ch:
			b.mu.RLock()
			fns := make([]func(Event), 0, len(b.subs))
			for _, fn := range b.subs {
				fns = append(fns, fn)
			}
			b.mu.RUnlock()
			for _, fn := range fns {
				func() {
					defer func() { _ = recover() }()
					fn(e)
				}()
			}
		}
	}
}

// Close stops delivery.
func (b *Bus) Close() { b.once.Do(func() { close(b.done) }) }
