package server_test

import (
	"context"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"

	"go.uber.org/zap"

	"github.com/oneaccord/restro/api/internal/config"
	"github.com/oneaccord/restro/api/internal/db"
	"github.com/oneaccord/restro/api/internal/db/sqlc"
	"github.com/oneaccord/restro/api/internal/realtime"
	"github.com/oneaccord/restro/api/internal/server"
)

func post(h http.Handler, path, body string) *httptest.ResponseRecorder {
	req := httptest.NewRequest(http.MethodPost, path, strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	return rec
}

// The staff desk needs a session; the public guest service exposes only its four
// RPCs (no staff method is reachable under it), and answers without a session.
func TestGuestPublicServiceCannotReachStaff(t *testing.T) {
	h := server.New(server.Deps{Cfg: &config.Config{Env: "dev"}, Log: zap.NewNop(), Hub: realtime.NewHub()})
	// Valid bodies, so the 401 comes from the session check, not validation.
	desk := map[string]string{
		"ListGuestRequests": `{}`, "AcceptGuestOrder": `{"requestId":"x","lines":[{"lineId":"y","quantity":1}]}`,
		"RejectGuestOrder": `{"requestId":"x","reason":"r"}`, "ResolveGuestAlert": `{"requestId":"x"}`, "ListGuestTables": `{}`,
		"SetGuestOrderingEnabled": `{"enabled":true}`, "SetTableGuestOrdering": `{"tableId":"x"}`, "NewGuestCode": `{"tableId":"x"}`,
	}
	for m, body := range desk {
		if rec := post(h, "/api/restaurant.v1.GuestDeskService/"+m, body); rec.Code != http.StatusUnauthorized {
			t.Fatalf("desk %s without a session: status = %d, want 401 (%s)", m, rec.Code, rec.Body.String())
		}
		if rec := post(h, "/api/restaurant.v1.GuestService/"+m, body); rec.Code != http.StatusNotFound {
			t.Fatalf("public GuestService/%s: status = %d, want 404", m, rec.Code)
		}
	}

	url := os.Getenv("TEST_DATABASE_URL")
	if url == "" {
		t.Skip("set TEST_DATABASE_URL to run the public guest call")
	}
	ctx := context.Background()
	pool, err := db.New(ctx, url)
	if err != nil {
		t.Fatalf("db.New: %v", err)
	}
	defer pool.Close()
	if err := db.RunMigrations(ctx, pool); err != nil {
		t.Fatalf("migrate: %v", err)
	}
	q := sqlc.New(pool)
	area, err := q.CreateArea(ctx, sqlc.CreateAreaParams{NameEn: "Guest mount"})
	if err != nil {
		t.Fatal(err)
	}
	tb, err := q.CreateTable(ctx, sqlc.CreateTableParams{AreaID: area.ID, Name: "Q1", Seats: 2, Active: true})
	if err != nil {
		t.Fatal(err)
	}
	h = server.New(server.Deps{Cfg: &config.Config{Env: "dev"}, Log: zap.NewNop(), Pool: pool, Hub: realtime.NewHub()})
	if rec := post(h, "/api/restaurant.v1.GuestService/GetGuestMenu", `{"tableCode":"`+tb.GuestCode+`"}`); rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), `"tableName":"Q1"`) {
		t.Fatalf("public menu without a session: %d %s", rec.Code, rec.Body.String())
	}
	if rec := post(h, "/api/restaurant.v1.GuestService/GetGuestMenu", `{"tableCode":"nope"}`); rec.Code != http.StatusNotFound || !strings.Contains(rec.Body.String(), "not_found") {
		t.Fatalf("bad code: %d %s", rec.Code, rec.Body.String())
	}
	req := httptest.NewRequest(http.MethodGet, "/api/guest/ws?t=nope", nil)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusNotFound {
		t.Fatalf("guest socket with a bad code: %d", rec.Code)
	}
}
