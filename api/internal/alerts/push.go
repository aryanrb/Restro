package alerts

// Web Push (spec "Push setup"). The engine records every immediate notification in
// alert_pushes inside the audited transaction — quiet hours, the daily cap and the
// "about themselves" exclusions are applied there, once. The same rows are what
// ListNotifications shows in the app; this file delivers them to subscribed
// browsers once the transaction committed: the request's pushes are collected in
// ctx (like the realtime events), and after the handler returned each one is
// re-read (a rolled-back push is gone) and sent asynchronously.

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"slices"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	webpush "github.com/SherClockHolmes/webpush-go"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"
	"go.uber.org/zap"

	restaurantv1 "github.com/oneaccord/restro/api/gen/restaurant/v1"
	"github.com/oneaccord/restro/api/internal/auth"
	"github.com/oneaccord/restro/api/internal/connecterr"
	"github.com/oneaccord/restro/api/internal/db/sqlc"
)

// PushSubscription is one browser's Web Push endpoint and keys.
type PushSubscription struct {
	Endpoint, P256dh, Auth string
}

// PushSender delivers one encrypted Web Push message and returns the push
// service's HTTP status.
type PushSender interface {
	Send(ctx context.Context, sub PushSubscription, payload []byte) (status int, err error)
}

// PushConfig is the server's VAPID identity (env VAPID_PUBLIC_KEY,
// VAPID_PRIVATE_KEY, VAPID_SUBJECT).
type PushConfig struct {
	PublicKey, PrivateKey, Subject string
}

// Enabled: both keys are set.
func (c PushConfig) Enabled() bool { return c.PublicKey != "" && c.PrivateKey != "" }

type pusher struct {
	publicKey string
	sender    PushSender
	pool      *pgxpool.Pool
	log       *zap.Logger
	wg        sync.WaitGroup
}

var activePusher atomic.Pointer[pusher]

// EnablePush turns on Web Push delivery for this process. Without keys push stays
// off (GetPushConfig reports disabled) and everything else works as before.
func EnablePush(pool *pgxpool.Pool, cfg PushConfig, log *zap.Logger) {
	if log == nil {
		log = zap.NewNop()
	}
	if !cfg.Enabled() {
		activePusher.Store(nil)
		log.Info("web push disabled (no VAPID keys)")
		return
	}
	activePusher.Store(&pusher{publicKey: cfg.PublicKey, pool: pool, log: log,
		sender: vapidSender{cfg: cfg, client: &http.Client{Timeout: 15 * time.Second}}})
}

// UsePushSender installs a sender (tests). wait blocks until every send started so
// far finished; restore turns push off again.
func UsePushSender(pool *pgxpool.Pool, publicKey string, sender PushSender) (wait func(), restore func()) {
	p := &pusher{publicKey: publicKey, sender: sender, pool: pool, log: zap.NewNop()}
	activePusher.Store(p)
	return p.wg.Wait, func() { p.wg.Wait(); activePusher.Store(nil) }
}

// vapidSender sends with github.com/SherClockHolmes/webpush-go.
type vapidSender struct {
	cfg    PushConfig
	client *http.Client
}

func (v vapidSender) Send(ctx context.Context, sub PushSubscription, payload []byte) (int, error) {
	resp, err := webpush.SendNotificationWithContext(ctx, payload,
		&webpush.Subscription{Endpoint: sub.Endpoint, Keys: webpush.Keys{P256dh: sub.P256dh, Auth: sub.Auth}},
		&webpush.Options{
			HTTPClient: v.client, Subscriber: v.cfg.Subject, VAPIDPublicKey: v.cfg.PublicKey, VAPIDPrivateKey: v.cfg.PrivateKey,
			TTL: 12 * 3600, Urgency: webpush.UrgencyHigh,
		})
	if err != nil {
		return 0, err
	}
	defer resp.Body.Close()
	_, _ = io.Copy(io.Discard, resp.Body)
	return resp.StatusCode, nil
}

// --- collecting the pushes a request wrote ---

func queuePush(ctx context.Context, rows []sqlc.AlertPush) {
	c, _ := ctx.Value(eventsKey{}).(*collector)
	if c == nil || len(rows) == 0 {
		return
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	for _, r := range rows {
		c.pushes = append(c.pushes, r.ID)
	}
}

// SendQueuedPushes delivers the pushes queued in ctx (see WithEvents) in the
// background. Call it after the transaction that wrote them committed; a push
// whose row is gone (rolled back) is skipped.
func SendQueuedPushes(ctx context.Context) {
	c, _ := ctx.Value(eventsKey{}).(*collector)
	if c == nil {
		return
	}
	c.mu.Lock()
	ids := c.pushes
	c.pushes = nil
	c.mu.Unlock()
	p := activePusher.Load()
	if p == nil || len(ids) == 0 {
		return
	}
	p.wg.Add(1)
	go func() {
		defer p.wg.Done()
		ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
		defer cancel()
		for _, id := range ids {
			if err := p.deliver(ctx, id); err != nil {
				p.log.Error("alerts: web push", zap.String("push", id.String()), zap.Error(err))
			}
		}
	}()
}

// deliver sends one alert_pushes row to the subscribed devices of the owner(s) and
// the managers the owner lists — never to someone the push is about.
func (p *pusher) deliver(ctx context.Context, id uuid.UUID) error {
	q := sqlc.New(p.pool)
	row, err := q.GetAlertPush(ctx, id)
	if noRows(err) {
		return nil // rolled back
	}
	if err != nil {
		return err
	}
	cfg, err := loadConfig(ctx, q)
	if err != nil {
		return err
	}
	owners, err := q.ActiveOwnerIDs(ctx)
	if err != nil {
		return err
	}
	var users []uuid.UUID
	for _, u := range append(owners, cfg.Recipients...) {
		if !slices.Contains(row.ExcludeIds, u) && !slices.Contains(users, u) {
			users = append(users, u)
		}
	}
	if len(users) == 0 {
		return nil
	}
	targets, err := q.ListPushTargets(ctx, users)
	if err != nil {
		return err
	}
	for _, t := range targets {
		if t.Role != auth.RoleOwner && (t.Role != auth.RoleManager || !cfg.isRecipient(t.UserID)) {
			continue
		}
		body, err := json.Marshal(pushPayload(row, t.Language))
		if err != nil {
			return err
		}
		status, err := p.sender.Send(ctx, PushSubscription{Endpoint: t.Endpoint, P256dh: t.P256dh, Auth: t.Auth}, body)
		switch {
		case err != nil:
			p.log.Warn("alerts: web push send", zap.Error(err))
		case status == http.StatusNotFound || status == http.StatusGone:
			// The browser unsubscribed or the subscription expired.
			if err := q.DeletePushSubscriptionByEndpoint(ctx, t.Endpoint); err != nil {
				return err
			}
		case status >= 400:
			p.log.Warn("alerts: web push rejected", zap.Int("status", status))
		}
	}
	return nil
}

// PushPayload is what the service worker receives (web scripts/build-sw.ts).
// The text names the rule and amount, never staff (lock screens).
type PushPayload struct {
	Title   string `json:"title"`
	Body    string `json:"body"`
	AlertID string `json:"alertId,omitempty"`
	URL     string `json:"url"`
	Tag     string `json:"tag"` // the push id: the in-app notifier uses the same tag
	Kind    string `json:"kind"`
}

func pushPayload(p sqlc.AlertPush, lang string) PushPayload {
	ne := lang == "ne"
	pick := func(en, np string) string {
		if ne {
			return np
		}
		return en
	}
	out := PushPayload{Title: pick("Restaurant alert", "रेस्टुरेन्ट सतर्कता"), Tag: p.ID.String(), Kind: p.Kind, URL: "/admin/alerts/"}
	switch p.Kind {
	case "ALERT":
		label := p.Rule
		if l, ok := ruleLabels[p.Rule]; ok {
			label = pick(l[0], l[1])
		}
		out.Body = label
		if p.AmountPaisa.Valid {
			out.Body += pick(" · ", " · रकम ") + formatRs(p.AmountPaisa.Int64)
		}
		if p.AlertID.Valid {
			out.AlertID = uuid.UUID(p.AlertID.Bytes).String()
			out.URL = "/admin/alerts/?alert=" + out.AlertID
		}
	case "MORE":
		out.Body = pick("More alerts are waiting in the app", "एपमा थप सतर्कता पर्खिरहेका छन्")
	case "SUMMARY":
		n := p.Count.Int32
		word := "alerts"
		if n == 1 {
			word = "alert"
		}
		out.Body = pick(fmt.Sprintf("Today: %s · %d %s to check", formatRs(p.AmountPaisa.Int64), n, word),
			fmt.Sprintf("आज: %s · %d सतर्कता जाँच्न बाँकी", formatRs(p.AmountPaisa.Int64), n))
		out.URL = "/admin/alerts/summary/"
	}
	return out
}

// formatRs: "Rs 1,23,456.50" (web lib/money formatMoney).
func formatRs(paisa int64) string {
	sign := ""
	if paisa < 0 {
		sign, paisa = "-", -paisa
	}
	digits := strconv.FormatInt(paisa/100, 10)
	if len(digits) > 3 {
		head, last3 := digits[:len(digits)-3], digits[len(digits)-3:]
		var parts []string
		for len(head) > 2 {
			parts = append([]string{head[len(head)-2:]}, parts...)
			head = head[:len(head)-2]
		}
		if head != "" {
			parts = append([]string{head}, parts...)
		}
		digits = strings.Join(append(parts, last3), ",")
	}
	return fmt.Sprintf("%sRs %s.%02d", sign, digits, paisa%100)
}

// ruleLabels: EN / NE rule names (web i18n alerts.rule.*).
var ruleLabels = map[string][2]string{
	"VOID_AFTER_KITCHEN":       {"Void after kitchen started", "भान्सा सुरु भएपछि रद्द"},
	"REPEATED_VOIDS":           {"Repeated voids", "बारम्बार रद्द"},
	"SELF_APPROVED":            {"Self-approved", "आफैंले स्वीकृत"},
	"HIGH_DISCOUNT":            {"High discount", "ठूलो छुट"},
	"DISCOUNT_TOTAL":           {"Discount total", "कुल छुट"},
	"CANCEL_REBILL_LOWER":      {"Cancelled and re-billed lower", "रद्द गरी कम रकममा पुनः बिल"},
	"LATE_CANCELLATION":        {"Late cancellation", "ढिलो रद्द"},
	"OLD_REPRINT":              {"Reprint of an old bill", "पुरानो बिलको पुनः छपाइ"},
	"CASH_SHORT":               {"Cash short", "नगद कम"},
	"CASH_OVER":                {"Cash over", "नगद बढी"},
	"REPEATED_SHORTAGES":       {"Repeated shortages", "बारम्बार नगद कम"},
	"SERVED_NOT_BILLED":        {"Served but never billed", "खुवाइयो तर बिल भएन"},
	"AFTER_HOURS_LOGIN":        {"After-hours login", "खुल्ने समय बाहिर लगइन"},
	"MANAGER_PIN_LOCKED":       {"Manager PIN locked", "व्यवस्थापकको PIN बन्द"},
	"PRICE_DIP":                {"Price dip", "मूल्य झारेर फेरि बढाइयो"},
	"CREDIT_OVER_LIMIT":        {"Credit over limit", "सीमाभन्दा बढी उधारो"},
	"CREDIT_NEW_ACCOUNT":       {"Credit to a new account", "नयाँ खातामा उधारो"},
	"LOYALTY_STAFF_NUMBER":     {"Staff number on a bill", "बिलमा कर्मचारीको नम्बर"},
	"LOYALTY_ONE_CASHIER":      {"One cashier feeds a member", "एउटै क्यासियरले सदस्यलाई छाप"},
	"LOYALTY_MANY_REDEMPTIONS": {"Many loyalty redemptions", "धेरै लोयल्टी इनाम"},
	"LOYALTY_STAMP_ADJUSTMENT": {"Large stamp adjustment", "ठूलो छाप मिलान"},
	"LARGE_PAID_OUT":           {"Large till paid-out", "गल्लाबाट ठूलो भुक्तानी"},
	"HIGH_WASTE":               {"High waste or stock loss", "धेरै खेर वा स्टक घाटा"},
}

// --- RPCs ---

// requirePushUser: the owner or a manager (a manager not (yet) on the recipient
// list may subscribe; they get pushes once the owner lists them).
func requirePushUser(ctx context.Context) (*sqlc.User, error) {
	actor, err := auth.RequireActor(ctx)
	if err != nil {
		return nil, err
	}
	if actor.User.Role != auth.RoleOwner && actor.User.Role != auth.RoleManager {
		return nil, connecterr.NotAllowed("only the owner and managers get alert notifications")
	}
	return actor.User, nil
}

func (s *Service) GetPushConfig(ctx context.Context) (*restaurantv1.GetPushConfigResponse, error) {
	if _, err := auth.RequireActor(ctx); err != nil {
		return nil, err
	}
	p := activePusher.Load()
	if p == nil {
		return &restaurantv1.GetPushConfigResponse{}, nil
	}
	return &restaurantv1.GetPushConfigResponse{Enabled: true, PublicKey: p.publicKey}, nil
}

func (s *Service) SavePushSubscription(ctx context.Context, req *restaurantv1.SavePushSubscriptionRequest) (*restaurantv1.SavePushSubscriptionResponse, error) {
	u, err := requirePushUser(ctx)
	if err != nil {
		return nil, err
	}
	if !strings.HasPrefix(req.GetEndpoint(), "https://") {
		return nil, connecterr.Invalid("endpoint", "endpoint must be an https URL")
	}
	if _, err := s.q.UpsertPushSubscription(ctx, sqlc.UpsertPushSubscriptionParams{
		UserID: u.ID, Endpoint: req.GetEndpoint(), P256dh: req.GetP256Dh(), Auth: req.GetAuth(), DeviceLabel: req.GetDeviceLabel(),
	}); err != nil {
		return nil, s.internal("save push subscription", err)
	}
	return &restaurantv1.SavePushSubscriptionResponse{}, nil
}

func (s *Service) DeletePushSubscription(ctx context.Context, req *restaurantv1.DeletePushSubscriptionRequest) (*restaurantv1.DeletePushSubscriptionResponse, error) {
	actor, err := auth.RequireActor(ctx)
	if err != nil {
		return nil, err
	}
	if _, err := s.q.DeleteUserPushSubscription(ctx, sqlc.DeleteUserPushSubscriptionParams{Endpoint: req.GetEndpoint(), UserID: actor.User.ID}); err != nil {
		return nil, s.internal("delete push subscription", err)
	}
	return &restaurantv1.DeletePushSubscriptionResponse{}, nil
}
