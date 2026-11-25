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

// The live, analytics and calendar services are mounted under /api: without a
// session they answer 401 (unauthenticated), not 404.
func TestLiveAndAnalyticsRoutesMounted(t *testing.T) {
	h := server.New(server.Deps{Cfg: &config.Config{Env: "dev"}, Log: zap.NewNop(), Hub: realtime.NewHub()})
	for path, body := range map[string]string{
		"/api/restaurant.v1.LiveService/GetSnapshot":             `{}`,
		"/api/restaurant.v1.LiveService/GetOrder":                `{"orderId":"x"}`,
		"/api/restaurant.v1.AnalyticsService/GetTrend":           `{}`,
		"/api/restaurant.v1.AnalyticsService/Rebuild":            `{}`,
		"/api/restaurant.v1.CalendarService/ListCalendarEvents":  `{}`,
		"/api/restaurant.v1.CalendarService/RemoveCalendarEvent": `{"id":"x"}`,
	} {
		req := httptest.NewRequest(http.MethodPost, path, strings.NewReader(body))
		req.Header.Set("Content-Type", "application/json")
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, req)
		if rec.Code != http.StatusUnauthorized {
			t.Fatalf("%s: status = %d, want 401 (%s)", path, rec.Code, rec.Body.String())
		}
	}
}
