package live

import (
	"testing"

	restaurantv1 "github.com/oneaccord/restro/api/gen/restaurant/v1"
)

// Mirrors web/src/lib/live/live-state.test.ts.

func it(id, status string) lineState { return lineState{id: id, status: status} }

func TestOrderStateFollowsTheDisplayPriority(t *testing.T) {
	billed := func(ids ...string) map[string]bool {
		m := map[string]bool{}
		for _, id := range ids {
			m[id] = true
		}
		return m
	}
	for _, c := range []struct {
		items  []lineState
		billed map[string]bool
		asked  bool
		want   restaurantv1.LiveState
	}{
		{nil, nil, false, restaurantv1.LiveState_LIVE_STATE_SEATED},
		{[]lineState{it("a", "VOIDED")}, nil, false, restaurantv1.LiveState_LIVE_STATE_SEATED},
		{[]lineState{it("a", "SENT"), it("b", "SERVED")}, nil, false, restaurantv1.LiveState_LIVE_STATE_ORDERED},
		{[]lineState{it("a", "SENT"), it("b", "PREPARING")}, nil, false, restaurantv1.LiveState_LIVE_STATE_COOKING},
		{[]lineState{it("a", "READY"), it("b", "PREPARING")}, nil, false, restaurantv1.LiveState_LIVE_STATE_READY},
		{[]lineState{it("a", "SERVED")}, nil, false, restaurantv1.LiveState_LIVE_STATE_SERVED},
		{[]lineState{it("a", "SERVED")}, billed("a"), false, restaurantv1.LiveState_LIVE_STATE_BILLED},
		{[]lineState{it("a", "READY")}, nil, true, restaurantv1.LiveState_LIVE_STATE_BILL_REQUESTED},
	} {
		items := make([]lineState, len(c.items))
		for i, x := range c.items {
			x.billed = c.billed[x.id]
			items[i] = x
		}
		if got := orderState(items, c.asked); got != c.want {
			t.Fatalf("%+v asked=%v: %v, want %v", c.items, c.asked, got, c.want)
		}
	}
}

func TestATableShowsItsMostUrgentOrder(t *testing.T) {
	if got := tableState(nil); got != restaurantv1.LiveState_LIVE_STATE_FREE {
		t.Fatalf("no orders = %v", got)
	}
	orders := []*restaurantv1.LiveOrder{
		{State: restaurantv1.LiveState_LIVE_STATE_SERVED},
		{State: restaurantv1.LiveState_LIVE_STATE_READY},
		{State: restaurantv1.LiveState_LIVE_STATE_SEATED},
	}
	if got := tableState(orders); got != restaurantv1.LiveState_LIVE_STATE_READY {
		t.Fatalf("most urgent = %v", got)
	}
}
