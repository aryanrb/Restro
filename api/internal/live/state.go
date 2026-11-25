package live

import restaurantv1 "github.com/oneaccord/restro/api/gen/restaurant/v1"

// Shared live-view rules, mirroring web/src/lib/live/live-state.ts. The LiveState
// enum values are in display priority order (BILL_REQUESTED = 1 … FREE = 8).

// lineState is what the state rule needs of a sent line.
type lineState struct {
	id     string
	status string
	billed bool // on an ISSUED bill
}

// orderState of one open order from its sent lines and the optional bill request.
func orderState(items []lineState, billRequested bool) restaurantv1.LiveState {
	if billRequested {
		return restaurantv1.LiveState_LIVE_STATE_BILL_REQUESTED
	}
	var live []lineState
	for _, i := range items {
		if i.status != "VOIDED" {
			live = append(live, i)
		}
	}
	has := func(status string) bool {
		for _, i := range live {
			if i.status == status {
				return true
			}
		}
		return false
	}
	switch {
	case has("READY"):
		return restaurantv1.LiveState_LIVE_STATE_READY
	case has("PREPARING"):
		return restaurantv1.LiveState_LIVE_STATE_COOKING
	case has("SENT"):
		return restaurantv1.LiveState_LIVE_STATE_ORDERED
	case len(live) == 0:
		return restaurantv1.LiveState_LIVE_STATE_SEATED
	}
	// Everything served: still to bill, or billed (pay-first, split) and not closed yet.
	for _, i := range live {
		if !i.billed {
			return restaurantv1.LiveState_LIVE_STATE_SERVED
		}
	}
	return restaurantv1.LiveState_LIVE_STATE_BILLED
}

// tableState: a table shows its most urgent order; FREE without orders.
func tableState(orders []*restaurantv1.LiveOrder) restaurantv1.LiveState {
	best := restaurantv1.LiveState_LIVE_STATE_FREE
	for _, o := range orders {
		if o.State != restaurantv1.LiveState_LIVE_STATE_UNSPECIFIED && o.State < best {
			best = o.State
		}
	}
	return best
}
