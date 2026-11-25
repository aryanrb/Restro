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
	"github.com/oneaccord/restro/api/internal/alerts"
	"github.com/oneaccord/restro/api/internal/analytics"
	"github.com/oneaccord/restro/api/internal/auth"
	"github.com/oneaccord/restro/api/internal/billing"
	"github.com/oneaccord/restro/api/internal/catalog"
	"github.com/oneaccord/restro/api/internal/config"
	"github.com/oneaccord/restro/api/internal/connecterr"
	"github.com/oneaccord/restro/api/internal/credit"
	"github.com/oneaccord/restro/api/internal/guest"
	"github.com/oneaccord/restro/api/internal/inventory"
	"github.com/oneaccord/restro/api/internal/kitchen"
	"github.com/oneaccord/restro/api/internal/live"
	"github.com/oneaccord/restro/api/internal/loyalty"
	"github.com/oneaccord/restro/api/internal/orders"
	"github.com/oneaccord/restro/api/internal/purchases"
	"github.com/oneaccord/restro/api/internal/realtime"
	"github.com/oneaccord/restro/api/internal/reports"
	"github.com/oneaccord/restro/api/internal/reservations"
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
	// CORS for the browser frontends. Auth rides on session cookies, so credentials
	// are allowed and origins must be an explicit list (never "*"). AllowHeaders is
	// left empty so Echo reflects the preflight's requested headers, which covers
	// Connect's Connect-Protocol-Version / Connect-Timeout-Ms and Authorization.
	e.Use(middleware.CORSWithConfig(middleware.CORSConfig{
		AllowOrigins:     d.Cfg.CORSOrigins,
		AllowCredentials: true,
	}))

	e.GET("/api/healthz", func(c echo.Context) error {
		return c.JSON(http.StatusOK, map[string]string{"status": "ok"})
	})
	// Shared interceptor chain for every Connect service: load the session (outer),
	// validate the request (protovalidate), map service errors to ErrorInfo (inner).
	// Suspicious-activity rules run on every audit_log write (audit/hook).
	alerts.Install(d.Log)
	authSvc := auth.NewService(d.Pool, d.Log)
	e.Any("/api/ws", echo.WrapHandler(wsAuth(authSvc, d.Hub)))

	ic := connect.WithInterceptors(
		authSvc.Interceptor(),
		connectvalidate.NewInterceptor(),
		connecterr.Interceptor(),
		// Stock events (stock.low / stock.86suggest / stock.updated) written by any
		// request — order sends, voids, counter sales, waste, counts — are broadcast
		// only once the handler succeeded (its transaction committed).
		inventory.EventsInterceptor(d.Hub),
		// alert.created / alert.updated written by the alert rules (which run on
		// every audited write, in its transaction) go out once the request finished.
		alerts.EventsInterceptor(d.Hub),
	)

	authHandler := auth.NewHandler(authSvc, d.Cfg.Env == "prod").WithCrossSiteCookies(d.Cfg.CookieCrossSite)
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

	creditH := credit.NewHandler(credit.NewService(d.Pool, d.Hub, d.Log))
	customerPath, customerHH := restaurantv1connect.NewCustomerServiceHandler(creditH, ic)
	mount(e, customerPath, customerHH)
	creditPath, creditHH := restaurantv1connect.NewCreditServiceHandler(creditH, ic)
	mount(e, creditPath, creditHH)

	shiftH := shift.NewHandler(shift.NewService(d.Pool, d.Hub, d.Log))
	shiftPath, shiftHH := restaurantv1connect.NewShiftServiceHandler(shiftH, ic)
	mount(e, shiftPath, shiftHH)

	reportH := reports.NewHandler(reports.NewService(d.Pool, d.Log))
	reportPath, reportHH := restaurantv1connect.NewReportServiceHandler(reportH, ic)
	mount(e, reportPath, reportHH)

	inventoryH := inventory.NewHandler(inventory.NewService(d.Pool, d.Hub, authSvc, d.Log))
	inventoryPath, inventoryHH := restaurantv1connect.NewInventoryServiceHandler(inventoryH, ic)
	mount(e, inventoryPath, inventoryHH)

	// Read-only live view (owner/manager): its own handler group with no write routes.
	livePath, liveHH := restaurantv1connect.NewLiveServiceHandler(live.NewHandler(live.NewService(d.Pool, d.Log)), ic)
	mount(e, livePath, liveHH)

	analyticsH := analytics.NewHandler(analytics.NewService(d.Pool, d.Log))
	analyticsPath, analyticsHH := restaurantv1connect.NewAnalyticsServiceHandler(analyticsH, ic)
	mount(e, analyticsPath, analyticsHH)
	calendarPath, calendarHH := restaurantv1connect.NewCalendarServiceHandler(analyticsH, ic)
	mount(e, calendarPath, calendarHH)

	purchaseH := purchases.NewHandler(purchases.NewService(d.Pool, d.Hub, authSvc, d.Log))
	supplierPath, supplierHH := restaurantv1connect.NewSupplierServiceHandler(purchaseH, ic)
	mount(e, supplierPath, supplierHH)
	purchasePath, purchaseHH := restaurantv1connect.NewPurchaseServiceHandler(purchaseH, ic)
	mount(e, purchasePath, purchaseHH)

	alertH := alerts.NewHandler(alerts.NewService(d.Pool, d.Hub, d.Log))
	alertPath, alertHH := restaurantv1connect.NewAlertServiceHandler(alertH, ic)
	mount(e, alertPath, alertHH)

	reservationPath, reservationHH := restaurantv1connect.NewReservationServiceHandler(reservations.NewHandler(reservations.NewService(d.Pool, d.Hub, d.Log)), ic)
	mount(e, reservationPath, reservationHH)

	// Loyalty stamp card. GetCardStatus is public: it reads no session.
	loyaltyPath, loyaltyHH := restaurantv1connect.NewLoyaltyServiceHandler(loyalty.NewHandler(loyalty.NewService(d.Pool, d.Hub, authSvc, d.Log)), ic)
	mount(e, loyaltyPath, loyaltyHH)

	// Guest table ordering. The desk (staff) service uses the normal chain; the
	// PUBLIC guest service gets a chain WITHOUT the session interceptor, so even a
	// request carrying a staff cookie has no actor there: it is identified only by
	// its table code and its handler type exposes only the four guest RPCs.
	guestSvc := guest.NewService(d.Pool, d.Hub, d.Log)
	guestDeskPath, guestDeskHH := restaurantv1connect.NewGuestDeskServiceHandler(guest.NewDeskHandler(guestSvc), ic)
	mount(e, guestDeskPath, guestDeskHH)
	publicIC := connect.WithInterceptors(connectvalidate.NewInterceptor(), connecterr.Interceptor())
	guestPath, guestHH := restaurantv1connect.NewGuestServiceHandler(guest.NewPublicHandler(guestSvc), publicIC)
	mount(e, guestPath, guestHH)
	// The guest page's own socket: only that table's guest.status pokes.
	e.GET("/api/guest/ws", echo.WrapHandler(guestSvc.WatchHandler()))

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
