package realtime

import (
	"encoding/json"
	"testing"
	"time"
)

// Every event carries "id" (empty string for broadcast-only events) so clients
// can parse one shape; version stays optional.
func TestEventAlwaysHasID(t *testing.T) {
	b, err := json.Marshal(Event{Type: "menu.updated"})
	if err != nil {
		t.Fatal(err)
	}
	if got, want := string(b), `{"type":"menu.updated","id":""}`; got != want {
		t.Fatalf("json = %s, want %s", got, want)
	}
	b, _ = json.Marshal(Event{Type: "order.updated", ID: "x", Version: 3})
	if got, want := string(b), `{"type":"order.updated","id":"x","version":3}`; got != want {
		t.Fatalf("json = %s, want %s", got, want)
	}
}

func TestSubscribeReceivesBroadcasts(t *testing.T) {
	h := NewHub()
	ch, cancel := h.Subscribe(4)
	h.Broadcast(Event{Type: "order.updated", ID: "o1", Version: 2})
	select {
	case ev := <-ch:
		if ev.Type != "order.updated" || ev.ID != "o1" || ev.Version != 2 {
			t.Fatalf("got %+v", ev)
		}
	case <-time.After(time.Second):
		t.Fatal("no event")
	}
	cancel()
	h.Broadcast(Event{Type: "order.updated"}) // must not block or panic after cancel
}
