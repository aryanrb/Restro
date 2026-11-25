package guest

import (
	"context"
	"encoding/json"
	"net/http"
	"time"

	"github.com/coder/websocket"
	"github.com/google/uuid"

	"github.com/oneaccord/restro/api/internal/realtime"
)

// WatchHandler serves the guest page's public socket, /api/guest/ws?t=<code>:
// no session, identified only by the table code. It sends only
// {"type":"guest.status","id":<table id>} for that table — on the table's
// guest.status, when one of its orders changes (order.updated) and when settings
// change — and closes once the code stops working. Server -> client only.
func (s *Service) WatchHandler() http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		code := r.URL.Query().Get("t")
		t, err := tableByCode(r.Context(), s.q, code)
		if err != nil {
			http.Error(w, "table code is not valid", http.StatusNotFound)
			return
		}
		if s.hub == nil {
			http.Error(w, "realtime unavailable", http.StatusServiceUnavailable)
			return
		}
		conn, err := websocket.Accept(w, r, &websocket.AcceptOptions{
			InsecureSkipVerify: true, // same as the staff socket: origin checked by Caddy in deploy
		})
		if err != nil {
			return
		}
		defer conn.CloseNow()
		events, cancel := s.hub.Subscribe(64)
		defer cancel()
		ctx := conn.CloseRead(r.Context()) // we never act on client frames
		tableID := t.ID.String()
		frame, _ := json.Marshal(realtime.Event{Type: EventStatus, ID: tableID})

		for {
			select {
			case <-ctx.Done():
				return
			case ev := <-events:
				relevant := false
				switch ev.Type {
				case EventStatus:
					if ev.ID != tableID {
						continue
					}
					// The code may have been regenerated (or the table deactivated).
					if _, err := tableByCode(ctx, s.q, code); err != nil {
						_ = conn.Close(websocket.StatusPolicyViolation, "table code is no longer valid")
						return
					}
					relevant = true
				case "settings.updated":
					relevant = true
				case "order.updated":
					if id, err := uuid.Parse(ev.ID); err == nil {
						if tid, err := s.q.TableOfOrder(ctx, id); err == nil && tid.Valid && uuid.UUID(tid.Bytes) == t.ID {
							relevant = true
						}
					}
				}
				if !relevant {
					continue
				}
				wctx, done := context.WithTimeout(ctx, 5*time.Second)
				err := conn.Write(wctx, websocket.MessageText, frame)
				done()
				if err != nil {
					return
				}
			}
		}
	})
}
