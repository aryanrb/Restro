// Package realtime is the in-process WebSocket hub. One deployment = one process,
// so no broker is needed. Server -> client "something changed" pokes only; clients
// refetch via Connect. See proto/websocket-events.md for the catalogue.
package realtime

import (
	"context"
	"encoding/json"
	"net/http"
	"sync"
	"time"

	"github.com/coder/websocket"
)

// Event is a poke. It is marshalled to a JSON text frame: {"type","id","version"}.
type Event struct {
	Type    string `json:"type"`
	ID      string `json:"id,omitempty"`
	Version int64  `json:"version,omitempty"`
}

type client struct {
	conn *websocket.Conn
	send chan []byte
}

// Hub fans events out to every connected client. A slow client is dropped rather
// than allowed to block the hub; it refetches everything when it reconnects.
type Hub struct {
	mu      sync.Mutex
	clients map[*client]struct{}
}

func NewHub() *Hub {
	return &Hub{clients: make(map[*client]struct{})}
}

// Broadcast delivers ev to every client with room in its buffer. Clients whose
// buffer is full are dropped (closed), on the theory that a reconnect + refetch is
// always correct and a blocked hub never is.
func (h *Hub) Broadcast(ev Event) {
	data, err := json.Marshal(ev)
	if err != nil {
		return
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	for c := range h.clients {
		select {
		case c.send <- data:
		default:
			delete(h.clients, c)
			close(c.send)
			// ponytail: drop slow consumers; close async so one stuck socket can't
			// stall the broadcast under the lock.
			go c.conn.Close(websocket.StatusPolicyViolation, "slow consumer")
		}
	}
}

func (h *Hub) add(c *client) {
	h.mu.Lock()
	h.clients[c] = struct{}{}
	h.mu.Unlock()
}

func (h *Hub) remove(c *client) {
	h.mu.Lock()
	if _, ok := h.clients[c]; ok {
		delete(h.clients, c)
		close(c.send)
	}
	h.mu.Unlock()
}

// ServeHTTP upgrades the request and registers the connection.
// NOTE: step 4 adds session auth on the upgrade (reject unauthenticated). For now
// any client may connect; in deploy, Caddy enforces same-origin.
func (h *Hub) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	conn, err := websocket.Accept(w, r, &websocket.AcceptOptions{
		InsecureSkipVerify: true, // ponytail: origin checked by Caddy in deploy; tighten with OriginPatterns there
	})
	if err != nil {
		return
	}
	c := &client{conn: conn, send: make(chan []byte, 16)}
	h.add(c)

	ctx := r.Context()
	go h.writeLoop(ctx, c)

	// Server -> client only: we never act on client frames. Reading just detects
	// disconnects so we can drop the client.
	for {
		if _, _, err := conn.Read(ctx); err != nil {
			break
		}
	}
	h.remove(c)
	_ = conn.Close(websocket.StatusNormalClosure, "")
}

func (h *Hub) writeLoop(ctx context.Context, c *client) {
	for data := range c.send {
		wctx, cancel := context.WithTimeout(ctx, 5*time.Second)
		err := c.conn.Write(wctx, websocket.MessageText, data)
		cancel()
		if err != nil {
			return
		}
	}
}
