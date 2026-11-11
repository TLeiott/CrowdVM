package guest

import (
	"encoding/json"
	"net/http"
	"sync"
	"time"

	"github.com/crowdvm/crowdvm/internal/proto"
)

// Guest event types emitted by this package.
const (
	EvSetupStatus     = "setup.status"      // {"status","error"}
	EvFileSkipped     = "file.skipped"      // {"path","error"}: a config/secret file was not written
	EvRepoSetup       = "repo.setup"        // {"repo","path","status","error"}
	EvCommandStarted  = "command.started"   // CommandRecord
	EvCommandExited   = "command.exited"    // CommandRecord
	EvDesktopStarted  = "desktop.started"   // {"port","display"}
	EvRecordingStatus = "desktop.recording" // {"recording","path"}
)

// hub fans events out to /v1/internal/events/stream subscribers and keeps a
// short ring for replay with ?since=.
type hub struct {
	mu   sync.Mutex
	subs map[chan proto.GuestEvent]struct{}
	ring []proto.GuestEvent
}

const ringSize = 512

func newHub() *hub { return &hub{subs: map[chan proto.GuestEvent]struct{}{}} }

func (h *hub) emit(ev proto.GuestEvent) {
	if ev.Time.IsZero() {
		ev.Time = time.Now().UTC()
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	h.ring = append(h.ring, ev)
	if len(h.ring) > ringSize {
		h.ring = h.ring[len(h.ring)-ringSize:]
	}
	for c := range h.subs {
		select {
		case c <- ev:
		default: // slow subscriber: drop rather than block the agent
		}
	}
}

func (h *hub) subscribe(since time.Time) (chan proto.GuestEvent, []proto.GuestEvent) {
	c := make(chan proto.GuestEvent, 1024)
	h.mu.Lock()
	defer h.mu.Unlock()
	var past []proto.GuestEvent
	if !since.IsZero() {
		for _, ev := range h.ring {
			if ev.Time.After(since) {
				past = append(past, ev)
			}
		}
	}
	h.subs[c] = struct{}{}
	return c, past
}

func (h *hub) unsubscribe(c chan proto.GuestEvent) {
	h.mu.Lock()
	delete(h.subs, c)
	h.mu.Unlock()
}

// Emit publishes a guest event; data is JSON-encoded.
func (a *Agent) Emit(typ string, data any) {
	b, _ := json.Marshal(data)
	a.events.emit(proto.GuestEvent{Type: typ, Data: b})
}

// GET /v1/internal/events/stream[?since=RFC3339]
func (a *Agent) handleEventsStream(w http.ResponseWriter, r *http.Request) {
	var since time.Time
	if s := r.URL.Query().Get("since"); s != "" {
		t, err := time.Parse(time.RFC3339Nano, s)
		if err != nil {
			writeErr(w, http.StatusBadRequest, "invalid_since", "since must be RFC 3339")
			return
		}
		since = t
	}
	c, past := a.events.subscribe(since)
	defer a.events.unsubscribe(c)
	w.Header().Set("Content-Type", "application/x-ndjson")
	w.WriteHeader(http.StatusOK)
	enc := json.NewEncoder(w)
	fl, _ := w.(http.Flusher)
	for _, ev := range past {
		_ = enc.Encode(ev)
	}
	if fl != nil {
		fl.Flush()
	}
	for {
		select {
		case <-r.Context().Done():
			return
		case ev := <-c:
			if enc.Encode(ev) != nil {
				return
			}
			if fl != nil {
				fl.Flush()
			}
		}
	}
}
