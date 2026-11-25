package alerts_test

import (
	"context"
	"encoding/json"
	"net/http"
	"slices"
	"sync"
	"testing"

	"connectrpc.com/connect"

	restaurantv1 "github.com/oneaccord/restro/api/gen/restaurant/v1"
	"github.com/oneaccord/restro/api/internal/alerts"
)

type stubSender struct {
	mu   sync.Mutex
	sent map[string]alerts.PushPayload // endpoint → payload
	gone map[string]bool
}

func (s *stubSender) Send(_ context.Context, sub alerts.PushSubscription, payload []byte) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	var p alerts.PushPayload
	_ = json.Unmarshal(payload, &p)
	s.sent[sub.Endpoint] = p
	if s.gone[sub.Endpoint] {
		return http.StatusGone, nil
	}
	return http.StatusCreated, nil
}

// A High alert is pushed after the request to the owner and the listed managers'
// devices — never to the manager it is about (Sita approved the void), nor to a
// manager who isn't listed, nor to a cashier. A 410 subscription is removed.
func TestWebPushGoesToRecipientsAfterTheRequest(t *testing.T) {
	f := newFixture(t)
	stub := &stubSender{sent: map[string]alerts.PushPayload{}, gone: map[string]bool{"https://push.test/bikash-old": true}}
	wait, restore := alerts.UsePushSender(f.pool, "PUBKEY", stub)
	defer restore()

	f.configure(func(c *restaurantv1.AlertConfig) { c.RecipientIds = []string{f.sitaID.String(), f.bikashID.String()} })
	save := func(ctx context.Context, endpoint string) {
		_, err := f.al.SavePushSubscription(ctx, &restaurantv1.SavePushSubscriptionRequest{Endpoint: endpoint, P256Dh: "k", Auth: "a"})
		f.must(err)
	}
	save(f.owner, "https://push.test/ram")
	save(f.sita, "https://push.test/sita")
	save(f.bikash, "https://push.test/bikash")
	save(f.bikash, "https://push.test/bikash-old")
	if _, err := f.al.SavePushSubscription(f.hari, &restaurantv1.SavePushSubscriptionRequest{Endpoint: "https://push.test/hari", P256Dh: "k", Auth: "a"}); err == nil {
		t.Fatal("cashier subscribed")
	}
	cfg, err := f.al.GetPushConfig(f.owner)
	f.must(err)
	if !cfg.Enabled || cfg.PublicKey != "PUBKEY" {
		t.Fatalf("config = %+v", cfg)
	}

	ic := alerts.EventsInterceptor(f.hub)
	run := func(ctx context.Context, fn func(context.Context)) {
		_, _ = ic(func(c context.Context, _ connect.AnyRequest) (connect.AnyResponse, error) {
			fn(c)
			return nil, nil
		})(ctx, connect.NewRequest(&restaurantv1.GetSessionRequest{}))
	}
	o := f.newOrder(f.gita, f.t1, line("veg", f.veg), line("chow", f.chow))
	f.openShift(f.hari, 0)
	first := f.pay(f.hari, o, nil)
	f.cancel(f.hari, first)
	f.mustVoid(f.hari, o, "chow", nil)
	run(f.hari, func(c context.Context) { f.pay(c, o, nil) })
	wait()

	got := make([]string, 0, len(stub.sent))
	for e := range stub.sent {
		got = append(got, e)
	}
	slices.Sort(got)
	if want := []string{"https://push.test/bikash", "https://push.test/bikash-old", "https://push.test/ram"}; !slices.Equal(got, want) {
		t.Fatalf("pushed to %v, want %v", got, want)
	}
	p := stub.sent["https://push.test/ram"]
	a := f.stored("CANCEL_REBILL_LOWER")[0]
	if p.Kind != "ALERT" || p.AlertID != a.ID.String() || p.URL != "/admin/alerts/?alert="+a.ID.String() || p.Body != "Cancelled and re-billed lower · Rs 200.00" {
		t.Fatalf("payload = %+v", p)
	}
	var n int
	f.must(f.pool.QueryRow(context.Background(), `SELECT count(*) FROM push_subscriptions WHERE endpoint = 'https://push.test/bikash-old'`).Scan(&n))
	if n != 0 {
		t.Fatal("410 subscription kept")
	}

	// Unsubscribing removes only the caller's own row.
	_, err = f.al.DeletePushSubscription(f.sita, &restaurantv1.DeletePushSubscriptionRequest{Endpoint: "https://push.test/ram"})
	f.must(err)
	f.must(f.pool.QueryRow(context.Background(), `SELECT count(*) FROM push_subscriptions WHERE endpoint = 'https://push.test/ram'`).Scan(&n))
	if n != 1 {
		t.Fatal("someone else's subscription removed")
	}
	_, err = f.al.DeletePushSubscription(f.owner, &restaurantv1.DeletePushSubscriptionRequest{Endpoint: "https://push.test/ram"})
	f.must(err)
	f.must(f.pool.QueryRow(context.Background(), `SELECT count(*) FROM push_subscriptions WHERE endpoint = 'https://push.test/ram'`).Scan(&n))
	if n != 0 {
		t.Fatal("owner's subscription kept")
	}
}
