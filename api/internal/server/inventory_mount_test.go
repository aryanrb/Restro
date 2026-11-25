package server_test

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"go.uber.org/zap"

	"github.com/oneaccord/restro/api/internal/config"
	"github.com/oneaccord/restro/api/internal/realtime"
	"github.com/oneaccord/restro/api/internal/server"
)

// The inventory service and the menu recipe RPCs are mounted under /api: without a
// session they answer 401 (unauthenticated), not 404.
func TestInventoryRoutesMounted(t *testing.T) {
	h := server.New(server.Deps{Cfg: &config.Config{Env: "dev"}, Log: zap.NewNop(), Hub: realtime.NewHub()})
	for _, path := range []string{
		"/api/restaurant.v1.InventoryService/ListInventoryItems",
		"/api/restaurant.v1.InventoryService/LogWaste",
		"/api/restaurant.v1.MenuService/SaveRecipe",
	} {
		req := httptest.NewRequest(http.MethodPost, path, strings.NewReader(`{}`))
		req.Header.Set("Content-Type", "application/json")
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, req)
		if rec.Code != http.StatusUnauthorized {
			t.Fatalf("%s: status = %d, want 401 (%s)", path, rec.Code, rec.Body.String())
		}
	}
}
