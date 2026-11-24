package server_test

import (
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"go.uber.org/zap"

	"github.com/oneaccord/restro/api/internal/config"
	"github.com/oneaccord/restro/api/internal/realtime"
	"github.com/oneaccord/restro/api/internal/server"
)

func TestHealthz(t *testing.T) {
	h := server.New(server.Deps{
		Cfg: &config.Config{Env: "dev"},
		Log: zap.NewNop(),
		Hub: realtime.NewHub(),
	})

	req := httptest.NewRequest(http.MethodGet, "/api/healthz", nil)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rec.Code)
	}
	body, _ := io.ReadAll(rec.Body)
	if got, want := strings.TrimSpace(string(body)), `{"status":"ok"}`; got != want {
		t.Fatalf("body = %q, want %q", got, want)
	}
	if ct := rec.Header().Get("Content-Type"); ct != "application/json" {
		t.Fatalf("content-type = %q, want application/json", ct)
	}
}
