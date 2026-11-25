// Package server builds the Echo HTTP server (per the One Accord boilerplate):
// recover/request-id/logging middleware, health, the websocket, and the ConnectRPC
// services mounted under /api. Connect handles its own request/response + errors
// via interceptors; Echo's error handler only covers the plain routes.
package server

import (
	"context"
	"net/http"

	"connectrpc.com/connect"
	connectvalidate "connectrpc.com/validate"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/labstack/echo/v4"
	"github.com/labstack/echo/v4/middleware"
	"go.uber.org/zap"

	restaurantv1 "github.com/oneaccord/restro/api/gen/restaurant/v1"
	"github.com/oneaccord/restro/api/gen/restaurant/v1/restaurantv1connect"
	"github.com/oneaccord/restro/api/internal/auth"
	"github.com/oneaccord/restro/api/internal/billing"
	"github.com/oneaccord/restro/api/internal/catalog"
	"github.com/oneaccord/restro/api/internal/config"
	"github.com/oneaccord/restro/api/internal/connecterr"
	"github.com/oneaccord/restro/api/internal/kitchen"
	"github.com/oneaccord/restro/api/internal/orders"
	"github.com/oneaccord/restro/api/internal/realtime"
	"github.com/oneaccord/restro/api/internal/reports"
	"github.com/oneaccord/restro/api/internal/shift"
)

// Deps are everything the services need. Fields fill in as domains land.
type Deps struct {
	Cfg  *config.Config
	Log  *zap.Logger
	Pool *pgxpool.Pool
	Hub  *realtime.Hub
}

// New builds the Echo server with all routes mounted.
func New(d Deps) *echo.Echo {
	e := echo.New()
	e.HideBanner = true
	e.HidePort = true
	e.Use(middleware.Recover(), middleware.RequestID(), requestLogger(d.Log))

	e.GET("/api/healthz", func(c echo.Context) error {
		return c.JSON(http.StatusOK, map[string]string{"status": "ok"})
	})
	// Shared interceptor chain for every Connect service: load the session (outer),
	// validate the request (protovalidate), map service errors to ErrorInfo (inner).
	authSvc := auth.NewService(d.Pool, d.Log)
	e.Any("/api/ws", echo.WrapHandler(wsAuth(authSvc, d.Hub)))

	ic := connect.WithInterceptors(
		authSvc.Interceptor(),
		connectvalidate.NewInterceptor(),
		connecterr.Interceptor(),
	)

	authHandler := auth.NewHandler(authSvc, d.Cfg.Env == "prod")
	authPath, authH := restaurantv1connect.NewAuthServiceHandler(authHandler, ic)
	mount(e, authPath, authH)
	staffPath, staffH := restaurantv1connect.NewStaffServiceHandler(authHandler, ic)
	mount(e, staffPath, staffH)

	cat := catalog.NewHandler(catalog.NewService(d.Pool, d.Hub, d.Log))
	setPath, setH := restaurantv1connect.NewSettingsServiceHandler(cat, ic)
	mount(e, setPath, setH)
	floorPath, floorH := restaurantv1connect.NewFloorServiceHandler(cat, ic)
	mount(e, floorPath, floorH)
	stationPath, stationH := restaurantv1connect.NewStationServiceHandler(cat, ic)
	mount(e, stationPath, stationH)
	menuPath, menuH := restaurantv1connect.NewMenuServiceHandler(cat, ic)
	mount(e, menuPath, menuH)

	orderH := orders.NewHandler(orders.NewService(d.Pool, d.Hub, authSvc, d.Log))
	orderPath, orderHH := restaurantv1connect.NewOrderServiceHandler(orderH, ic)
	mount(e, orderPath, orderHH)

	kitchenH := kitchen.NewHandler(kitchen.NewService(d.Pool, d.Hub, d.Log))
	kitchenPath, kitchenHH := restaurantv1connect.NewKitchenServiceHandler(kitchenH, ic)
	mount(e, kitchenPath, kitchenHH)

	billingH := billing.NewHandler(billing.NewService(d.Pool, d.Hub, authSvc, d.Log))
	billingPath, billingHH := restaurantv1connect.NewBillingServiceHandler(billingH, ic)
	mount(e, billingPath, billingHH)

	shiftH := shift.NewHandler(shift.NewService(d.Pool, d.Hub, d.Log))
	shiftPath, shiftHH := restaurantv1connect.NewShiftServiceHandler(shiftH, ic)
	mount(e, shiftPath, shiftHH)

	reportH := reports.NewHandler(reports.NewService(d.Pool, d.Log))
	reportPath, reportHH := restaurantv1connect.NewReportServiceHandler(reportH, ic)
	mount(e, reportPath, reportHH)

	return e
}

// wsAuth admits a WebSocket upgrade only with a valid session: the web's session
// cookie or a mobile bearer token, resolved exactly as for a Connect request (it
// runs the same auth interceptor). Anything else gets 401 before the upgrade.
func wsAuth(authSvc *auth.Service, next http.Handler) http.Handler {
	ic := authSvc.Interceptor()
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		req := connect.NewRequest(&restaurantv1.GetSessionRequest{})
		for k, v := range r.Header {
			req.Header()[k] = v
		}
		var loggedIn bool
		_, _ = ic(func(ctx context.Context, _ connect.AnyRequest) (connect.AnyResponse, error) {
			loggedIn = auth.ActorFrom(ctx).LoggedIn()
			return nil, nil
		})(r.Context(), req)
		if !loggedIn {
			http.Error(w, "unauthenticated", http.StatusUnauthorized)
			return
		}
		next.ServeHTTP(w, r)
	})
}

// mount serves a Connect handler under /api, so the generated path
// /restaurant.v1.X/Method is reachable at /api/restaurant.v1.X/Method.
func mount(e *echo.Echo, path string, h http.Handler) {
	e.Any("/api"+path+"*", echo.WrapHandler(http.StripPrefix("/api", h)))
}

func requestLogger(log *zap.Logger) echo.MiddlewareFunc {
	return middleware.RequestLoggerWithConfig(middleware.RequestLoggerConfig{
		LogStatus: true, LogMethod: true, LogURI: true, LogLatency: true,
		LogValuesFunc: func(_ echo.Context, v middleware.RequestLoggerValues) error {
			log.Info("request",
				zap.String("method", v.Method),
				zap.String("uri", v.URI),
				zap.Int("status", v.Status),
				zap.Duration("latency", v.Latency),
			)
			return nil
		},
	})
}
