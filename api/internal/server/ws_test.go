package server_test

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/coder/websocket"
	"go.uber.org/zap"

	"github.com/oneaccord/restro/api/internal/auth"
	"github.com/oneaccord/restro/api/internal/config"
	"github.com/oneaccord/restro/api/internal/db"
	"github.com/oneaccord/restro/api/internal/realtime"
	"github.com/oneaccord/restro/api/internal/server"
)

// An upgrade without a session is refused before the socket opens.
func TestWebSocketRejectsUnauthenticated(t *testing.T) {
	hub := realtime.NewHub()
	srv := httptest.NewServer(server.New(server.Deps{Cfg: &config.Config{Env: "dev"}, Log: zap.NewNop(), Hub: hub}))
	defer srv.Close()

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	_, resp, err := websocket.Dial(ctx, "ws"+strings.TrimPrefix(srv.URL, "http")+"/api/ws", nil)
	if err == nil {
		t.Fatal("dial without a session succeeded")
	}
	if resp == nil || resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("status = %v, want 401", resp)
	}
}

// With a valid session (cookie or bearer) the socket opens and receives events,
// each with an "id" (empty for broadcast-only events).
func TestWebSocketAcceptsSession(t *testing.T) {
	url := os.Getenv("TEST_DATABASE_URL")
	if url == "" {
		t.Skip("set TEST_DATABASE_URL to run the websocket auth test")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	pool, err := db.New(ctx, url)
	if err != nil {
		t.Fatalf("db.New: %v", err)
	}
	defer pool.Close()
	if err := db.RunMigrations(ctx, pool); err != nil {
		t.Fatalf("migrate: %v", err)
	}
	if _, err := pool.Exec(ctx, `TRUNCATE users, sessions, devices, audit_log RESTART IDENTITY CASCADE`); err != nil {
		t.Fatalf("truncate: %v", err)
	}
	authSvc := auth.NewService(pool, zap.NewNop())
	if _, err := authSvc.SeedOwner(ctx, "Ram", "owner", "owner123", "1111"); err != nil {
		t.Fatalf("seed: %v", err)
	}
	_, token, err := authSvc.PasswordLogin(ctx, "owner", "owner123")
	if err != nil {
		t.Fatalf("login: %v", err)
	}

	hub := realtime.NewHub()
	srv := httptest.NewServer(server.New(server.Deps{Cfg: &config.Config{Env: "dev"}, Log: zap.NewNop(), Pool: pool, Hub: hub}))
	defer srv.Close()
	wsURL := "ws" + strings.TrimPrefix(srv.URL, "http") + "/api/ws"

	// A bad token is still refused.
	h := http.Header{}
	h.Set("Authorization", "Bearer not-a-token")
	if _, resp, err := websocket.Dial(ctx, wsURL, &websocket.DialOptions{HTTPHeader: h}); err == nil || resp == nil || resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("bad token: err=%v resp=%v, want 401", err, resp)
	}

	for name, hdr := range map[string]http.Header{
		"cookie": {"Cookie": []string{"rm_session=" + token}},
		"bearer": {"Authorization": []string{"Bearer " + token}},
	} {
		t.Run(name, func(t *testing.T) {
			conn, _, err := websocket.Dial(ctx, wsURL, &websocket.DialOptions{HTTPHeader: hdr})
			if err != nil {
				t.Fatalf("dial: %v", err)
			}
			defer conn.Close(websocket.StatusNormalClosure, "")
			// The hub registers the client after the upgrade; retry the poke until it lands.
			got := make(chan []byte, 1)
			go func() {
				_, data, err := conn.Read(ctx)
				if err == nil {
					got <- data
				}
			}()
			for {
				hub.Broadcast(realtime.Event{Type: "menu.updated"})
				select {
				case data := <-got:
					var m map[string]any
					if err := json.Unmarshal(data, &m); err != nil {
						t.Fatalf("json: %v", err)
					}
					if id, ok := m["id"]; !ok || id != "" {
						t.Fatalf("event %s: want id \"\"", data)
					}
					return
				case <-time.After(50 * time.Millisecond):
				case <-ctx.Done():
					t.Fatal("no event received")
				}
			}
		})
	}
}
