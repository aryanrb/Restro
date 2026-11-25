package orders

import (
	"context"
	"errors"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"

	restaurantv1 "github.com/oneaccord/restro/api/gen/restaurant/v1"
	"github.com/oneaccord/restro/api/internal/audit"
	"github.com/oneaccord/restro/api/internal/auth"
	"github.com/oneaccord/restro/api/internal/connecterr"
	"github.com/oneaccord/restro/api/internal/db/sqlc"
	"github.com/oneaccord/restro/api/internal/dbx"
	"github.com/oneaccord/restro/api/internal/realtime"
)

func parseID(field, s string) (uuid.UUID, error) {
	id, err := uuid.Parse(s)
	if err != nil {
		return uuid.Nil, connecterr.Invalid(field, "invalid id")
	}
	return id, nil
}

// --- queries ---

func (s *Service) ListOpenOrders(ctx context.Context) ([]*restaurantv1.Order, error) {
	if _, err := requireOrderRole(ctx); err != nil {
		return nil, err
	}
	rows, err := s.q.ListOpenOrders(ctx)
	if err != nil {
		return nil, connecterr.Internal(err)
	}
	out := make([]*restaurantv1.Order, 0, len(rows))
	for _, o := range rows {
		ord, err := s.assembleOrder(ctx, s.q, o.ID)
		if err != nil {
			return nil, err
		}
		out = append(out, ord)
	}
	return out, nil
}

func (s *Service) GetOrder(ctx context.Context, req *restaurantv1.GetOrderRequest) (*restaurantv1.Order, error) {
	if _, err := requireOrderRole(ctx); err != nil {
		return nil, err
	}
	var o sqlc.Order
	var err error
	switch {
	case req.Id != nil:
		id, perr := parseID("id", req.GetId())
		if perr != nil {
			return nil, perr
		}
		o, err = s.q.GetOrderByID(ctx, id)
	case req.ClientId != nil:
		cid, perr := parseID("client_id", req.GetClientId())
		if perr != nil {
			return nil, perr
		}
		o, err = s.q.GetOrderByClientID(ctx, cid)
	default:
		return nil, connecterr.Invalid("id", "id or client_id is required")
	}
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, connecterr.NotFound("order not found")
	}
	if err != nil {
		return nil, connecterr.Internal(err)
	}
	return s.assembleOrder(ctx, s.q, o.ID)
}

// --- commands ---

func (s *Service) CreateOrder(ctx context.Context, req *restaurantv1.CreateOrderRequest) (*restaurantv1.OrderResult, error) {
	actor, err := requireOrderRole(ctx)
	if err != nil {
		return nil, err
	}
	cmdID, err := parseID("command_id", req.GetCommandId())
	if err != nil {
		return nil, err
	}
	ocid, err := parseID("order_client_id", req.GetOrderClientId())
	if err != nil {
		return nil, err
	}

	var table pgtype.UUID
	if req.GetType() == restaurantv1.OrderType_ORDER_TYPE_DINE_IN {
		if req.TableId == nil {
			return nil, connecterr.Invalid("table_id", "dine-in orders need a table")
		}
		tid, perr := parseID("table_id", req.GetTableId())
		if perr != nil {
			return nil, perr
		}
		table = dbx.UUID(tid)
	} else if req.TableId != nil {
		return nil, connecterr.Invalid("table_id", "takeaway orders have no table")
	}

	out := &restaurantv1.OrderResult{}
	err = s.runIdempotent(ctx, cmdID, actor.User.ID, out, func(q *sqlc.Queries) error {
		o, e := q.CreateOrder(ctx, sqlc.CreateOrderParams{
			ClientID: ocid, Type: orderTypeStr(req.GetType()), TableID: table,
			WaiterUserID: actor.User.ID, GuestCount: req.GetGuestCount(),
		})
		if errors.Is(e, pgx.ErrNoRows) { // client_id exists -> idempotent create
			o, e = q.GetOrderByClientID(ctx, ocid)
		}
		if e != nil {
			return connecterr.Internal(e)
		}
		ord, e := s.assembleOrder(ctx, q, o.ID)
		if e != nil {
			return e
		}
		out.Order = ord
		return nil
	})
	if err != nil {
		return nil, err
	}
	s.emit(realtime.Event{Type: "order.updated", ID: out.Order.Id, Version: out.Order.Version})
	return out, nil
}

func (s *Service) AddItems(ctx context.Context, req *restaurantv1.AddItemsRequest) (*restaurantv1.AddItemsResult, error) {
	actor, err := requireOrderRole(ctx)
	if err != nil {
		return nil, err
	}
	cmdID, err := parseID("command_id", req.GetCommandId())
	if err != nil {
		return nil, err
	}
	ocid, err := parseID("order_client_id", req.GetOrderClientId())
	if err != nil {
		return nil, err
	}

	out := &restaurantv1.AddItemsResult{}
	err = s.runIdempotent(ctx, cmdID, actor.User.ID, out, func(q *sqlc.Queries) error {
		order, e := q.GetOrderByClientID(ctx, ocid)
		if errors.Is(e, pgx.ErrNoRows) {
			return connecterr.NotFound("order not found")
		}
		if e != nil {
			return connecterr.Internal(e)
		}
		if e := requireOpen(order); e != nil {
			return e
		}

		applied := 0
		for _, ni := range req.GetItems() {
			item, ie := q.GetItem(ctx, mustUUID(ni.GetMenuItemId()))
			if errors.Is(ie, pgx.ErrNoRows) {
				out.Rejected = append(out.Rejected, rejected(ni.GetClientId(), restaurantv1.ErrorReason_ERROR_REASON_INVALID, "item not found"))
				continue
			}
			if ie != nil {
				return connecterr.Internal(ie)
			}
			if item.Is86d {
				out.Rejected = append(out.Rejected, rejected(ni.GetClientId(), restaurantv1.ErrorReason_ERROR_REASON_ITEM_86D, "item is out of stock"))
				continue
			}

			unitPrice := item.PricePaisa
			varEn, varNe, hasVar := "", "", false
			if ni.VariantId != nil {
				if v, ve := q.GetVariant(ctx, mustUUID(ni.GetVariantId())); ve == nil {
					unitPrice, varEn, varNe, hasVar = v.PricePaisa, v.NameEn, v.NameNe, true
				}
			}

			oi, ce := q.CreateOrderItem(ctx, sqlc.CreateOrderItemParams{
				OrderID: order.ID, ClientID: mustUUID(ni.GetClientId()), MenuItemID: item.ID,
				NameEn: item.NameEn, NameNe: item.NameNe, VariantNameEn: varEn, VariantNameNe: varNe, HasVariant: hasVar,
				UnitPricePaisa: unitPrice, Quantity: ni.GetQuantity(), Note: ni.GetNote(), StationID: item.StationID,
			})
			if ce != nil {
				return connecterr.Internal(ce)
			}
			for _, mid := range ni.GetModifierIds() {
				m, me := q.GetModifierByID(ctx, mustUUID(mid))
				if me != nil {
					continue
				}
				if e := q.CreateOrderItemModifier(ctx, sqlc.CreateOrderItemModifierParams{
					OrderItemID: oi.ID, NameEn: m.NameEn, NameNe: m.NameNe, PriceDeltaPaisa: m.PriceDeltaPaisa,
				}); e != nil {
					return connecterr.Internal(e)
				}
			}
			applied++
		}
		if applied > 0 {
			if _, e := q.BumpOrderVersion(ctx, order.ID); e != nil {
				return connecterr.Internal(e)
			}
		}
		ord, e := s.assembleOrder(ctx, q, order.ID)
		if e != nil {
			return e
		}
		out.Order = ord
		return nil
	})
	if err != nil {
		return nil, err
	}
	s.emit(realtime.Event{Type: "order.updated", ID: out.Order.Id, Version: out.Order.Version})
	return out, nil
}

func (s *Service) SendItems(ctx context.Context, req *restaurantv1.SendItemsRequest) (*restaurantv1.OrderResult, error) {
	actor, err := requireOrderRole(ctx)
	if err != nil {
		return nil, err
	}
	cmdID, err := parseID("command_id", req.GetCommandId())
	if err != nil {
		return nil, err
	}
	ocid, err := parseID("order_client_id", req.GetOrderClientId())
	if err != nil {
		return nil, err
	}

	out := &restaurantv1.OrderResult{}
	err = s.runIdempotent(ctx, cmdID, actor.User.ID, out, func(q *sqlc.Queries) error {
		order, e := q.GetOrderByClientID(ctx, ocid)
		if errors.Is(e, pgx.ErrNoRows) {
			return connecterr.NotFound("order not found")
		}
		if e != nil {
			return connecterr.Internal(e)
		}
		if e := requireOpen(order); e != nil {
			return e
		}
		items, e := q.ListOrderItems(ctx, order.ID)
		if e != nil {
			return connecterr.Internal(e)
		}
		want := map[string]bool{}
		for _, id := range req.GetItemIds() {
			want[id] = true
		}
		var sendIDs []uuid.UUID
		for _, it := range items {
			if it.Status != "DRAFT" {
				continue
			}
			if len(want) == 0 || want[it.ID.String()] {
				sendIDs = append(sendIDs, it.ID)
			}
		}
		if len(sendIDs) > 0 {
			if e := q.SetOrderItemStatus(ctx, sqlc.SetOrderItemStatusParams{
				Status: "SENT", OrderID: order.ID, Ids: sendIDs, FromStatus: []string{"DRAFT"},
			}); e != nil {
				return connecterr.Internal(e)
			}
			if _, e := q.BumpOrderVersion(ctx, order.ID); e != nil {
				return connecterr.Internal(e)
			}
		}
		ord, e := s.assembleOrder(ctx, q, order.ID)
		if e != nil {
			return e
		}
		out.Order = ord
		return nil
	})
	if err != nil {
		return nil, err
	}
	s.emit(realtime.Event{Type: "ticket.created", ID: out.Order.Id})
	s.emit(realtime.Event{Type: "order.updated", ID: out.Order.Id, Version: out.Order.Version})
	return out, nil
}

func (s *Service) VoidItem(ctx context.Context, req *restaurantv1.VoidItemRequest) (*restaurantv1.OrderResult, error) {
	actor, err := requireOrderRole(ctx)
	if err != nil {
		return nil, err
	}
	cmdID, err := parseID("command_id", req.GetCommandId())
	if err != nil {
		return nil, err
	}
	ocid, err := parseID("order_client_id", req.GetOrderClientId())
	if err != nil {
		return nil, err
	}
	itemID, err := parseID("item_id", req.GetItemId())
	if err != nil {
		return nil, err
	}

	out := &restaurantv1.OrderResult{}
	err = s.runIdempotent(ctx, cmdID, actor.User.ID, out, func(q *sqlc.Queries) error {
		order, e := q.GetOrderByClientID(ctx, ocid)
		if errors.Is(e, pgx.ErrNoRows) {
			return connecterr.NotFound("order not found")
		}
		if e != nil {
			return connecterr.Internal(e)
		}
		if e := requireOpen(order); e != nil {
			return e
		}
		item, e := q.GetOrderItem(ctx, itemID)
		if errors.Is(e, pgx.ErrNoRows) || (e == nil && item.OrderID != order.ID) {
			return connecterr.NotFound("item not found on this order")
		}
		if e != nil {
			return connecterr.Internal(e)
		}

		// A line on an issued invoice is part of an immutable fiscal document; the
		// invoice must be cancelled (which clears invoice_id) before it can be voided.
		// TODO: switch to an ITEM_BILLED reason once the proto has one.
		if item.InvoiceID.Valid {
			return connecterr.Invalid("item_id", "item is on an issued invoice; cancel the invoice first")
		}

		var approvedBy uuid.UUID
		switch item.Status {
		case "VOIDED":
			return connecterr.Invalid("item_id", "item is already voided")
		case "DRAFT":
			// free void
		case "SENT":
			if req.GetReason() == "" {
				return connecterr.Invalid("reason", "a reason is required")
			}
		case "PREPARING", "READY", "SERVED":
			if req.GetReason() == "" {
				return connecterr.Invalid("reason", "a reason is required")
			}
			if req.Approval == nil {
				return connecterr.ManagerApprovalRequired("voiding a prepared item needs a manager PIN")
			}
			mgr, me := s.auth.VerifyManagerApproval(ctx, auth.ManagerApproval{ManagerUserID: req.Approval.GetManagerUserId(), Pin: req.Approval.GetPin()})
			if me != nil {
				return me
			}
			approvedBy = mgr.ID
		}

		if e := q.VoidOrderItem(ctx, sqlc.VoidOrderItemParams{
			ID: itemID, VoidReason: req.GetReason(), VoidedByUserID: dbx.UUID(actor.User.ID), VoidApprovedByUserID: dbx.UUIDOpt(approvedBy),
		}); e != nil {
			return connecterr.Internal(e)
		}
		if _, e := q.CheckBumpOrderVersion(ctx, sqlc.CheckBumpOrderVersionParams{ID: order.ID, Version: req.GetBaseVersion()}); errors.Is(e, pgx.ErrNoRows) {
			return connecterr.StaleVersion("order changed; review and retry")
		} else if e != nil {
			return connecterr.Internal(e)
		}
		if e := audit.Write(ctx, q, audit.Entry{
			ActorID: actor.User.ID, Action: "order.void", Entity: "order_item", EntityID: itemID.String(),
			Reason: req.GetReason(), ApproverID: approvedBy,
		}); e != nil {
			return e
		}
		ord, e := s.assembleOrder(ctx, q, order.ID)
		if e != nil {
			return e
		}
		out.Order = ord
		return nil
	})
	if err != nil {
		return nil, err
	}
	s.emit(realtime.Event{Type: "order.updated", ID: out.Order.Id, Version: out.Order.Version})
	return out, nil
}

func (s *Service) MarkServed(ctx context.Context, req *restaurantv1.MarkServedRequest) (*restaurantv1.OrderResult, error) {
	actor, err := requireOrderRole(ctx)
	if err != nil {
		return nil, err
	}
	cmdID, err := parseID("command_id", req.GetCommandId())
	if err != nil {
		return nil, err
	}
	ocid, err := parseID("order_client_id", req.GetOrderClientId())
	if err != nil {
		return nil, err
	}
	ids := make([]uuid.UUID, 0, len(req.GetItemIds()))
	for _, id := range req.GetItemIds() {
		u, perr := parseID("item_ids", id)
		if perr != nil {
			return nil, perr
		}
		ids = append(ids, u)
	}

	out := &restaurantv1.OrderResult{}
	err = s.runIdempotent(ctx, cmdID, actor.User.ID, out, func(q *sqlc.Queries) error {
		order, e := q.GetOrderByClientID(ctx, ocid)
		if errors.Is(e, pgx.ErrNoRows) {
			return connecterr.NotFound("order not found")
		}
		if e != nil {
			return connecterr.Internal(e)
		}
		if e := requireServable(order); e != nil {
			return e
		}
		if e := q.SetOrderItemStatus(ctx, sqlc.SetOrderItemStatusParams{
			Status: "SERVED", OrderID: order.ID, Ids: ids, FromStatus: []string{"SENT", "PREPARING", "READY"},
		}); e != nil {
			return connecterr.Internal(e)
		}
		if _, e := q.BumpOrderVersion(ctx, order.ID); e != nil {
			return connecterr.Internal(e)
		}
		ord, e := s.assembleOrder(ctx, q, order.ID)
		if e != nil {
			return e
		}
		out.Order = ord
		return nil
	})
	if err != nil {
		return nil, err
	}
	s.emit(realtime.Event{Type: "order.updated", ID: out.Order.Id, Version: out.Order.Version})
	return out, nil
}

func (s *Service) TransferTable(ctx context.Context, req *restaurantv1.TransferTableRequest) (*restaurantv1.OrderResult, error) {
	actor, err := requireOrderRole(ctx)
	if err != nil {
		return nil, err
	}
	cmdID, err := parseID("command_id", req.GetCommandId())
	if err != nil {
		return nil, err
	}
	ocid, err := parseID("order_client_id", req.GetOrderClientId())
	if err != nil {
		return nil, err
	}
	newTable, err := parseID("new_table_id", req.GetNewTableId())
	if err != nil {
		return nil, err
	}

	var oldTable string
	out := &restaurantv1.OrderResult{}
	err = s.runIdempotent(ctx, cmdID, actor.User.ID, out, func(q *sqlc.Queries) error {
		order, e := q.GetOrderByClientID(ctx, ocid)
		if errors.Is(e, pgx.ErrNoRows) {
			return connecterr.NotFound("order not found")
		}
		if e != nil {
			return connecterr.Internal(e)
		}
		if e := requireOpen(order); e != nil {
			return e
		}
		oldTable = pgUUIDStr(order.TableID)
		if _, e := q.CheckBumpOrderVersion(ctx, sqlc.CheckBumpOrderVersionParams{ID: order.ID, Version: req.GetBaseVersion()}); errors.Is(e, pgx.ErrNoRows) {
			return connecterr.StaleVersion("order changed; review and retry")
		} else if e != nil {
			return connecterr.Internal(e)
		}
		if e := q.SetOrderTable(ctx, sqlc.SetOrderTableParams{ID: order.ID, TableID: dbx.UUID(newTable)}); e != nil {
			return connecterr.Internal(e)
		}
		if e := audit.Write(ctx, q, audit.Entry{ActorID: actor.User.ID, Action: "order.transfer", Entity: "order", EntityID: order.ID.String(), After: audit.JSON(map[string]string{"new_table": newTable.String()})}); e != nil {
			return e
		}
		ord, e := s.assembleOrder(ctx, q, order.ID)
		if e != nil {
			return e
		}
		out.Order = ord
		return nil
	})
	if err != nil {
		return nil, err
	}
	if oldTable != "" {
		s.emit(realtime.Event{Type: "table.updated", ID: oldTable})
	}
	s.emit(realtime.Event{Type: "table.updated", ID: newTable.String()})
	s.emit(realtime.Event{Type: "order.updated", ID: out.Order.Id, Version: out.Order.Version})
	return out, nil
}

func (s *Service) MergeTables(ctx context.Context, req *restaurantv1.MergeTablesRequest) (*restaurantv1.OrderResult, error) {
	actor, err := requireOrderRole(ctx)
	if err != nil {
		return nil, err
	}
	cmdID, err := parseID("command_id", req.GetCommandId())
	if err != nil {
		return nil, err
	}
	srcCID, err := parseID("source_order_client_id", req.GetSourceOrderClientId())
	if err != nil {
		return nil, err
	}
	tgtCID, err := parseID("target_order_client_id", req.GetTargetOrderClientId())
	if err != nil {
		return nil, err
	}

	var srcID string
	out := &restaurantv1.OrderResult{}
	err = s.runIdempotent(ctx, cmdID, actor.User.ID, out, func(q *sqlc.Queries) error {
		src, e := q.GetOrderByClientID(ctx, srcCID)
		if errors.Is(e, pgx.ErrNoRows) {
			return connecterr.NotFound("source order not found")
		}
		if e != nil {
			return connecterr.Internal(e)
		}
		tgt, e := q.GetOrderByClientID(ctx, tgtCID)
		if errors.Is(e, pgx.ErrNoRows) {
			return connecterr.NotFound("target order not found")
		}
		if e != nil {
			return connecterr.Internal(e)
		}
		if e := requireOpen(src); e != nil {
			return connecterr.OrderClosed("source order is not open")
		}
		if e := requireOpen(tgt); e != nil {
			return connecterr.OrderClosed("target order is not open")
		}
		srcID = src.ID.String()
		if _, e := q.CheckBumpOrderVersion(ctx, sqlc.CheckBumpOrderVersionParams{ID: src.ID, Version: req.GetSourceBaseVersion()}); errors.Is(e, pgx.ErrNoRows) {
			return connecterr.StaleVersion("source order changed; review and retry")
		} else if e != nil {
			return connecterr.Internal(e)
		}
		if _, e := q.CheckBumpOrderVersion(ctx, sqlc.CheckBumpOrderVersionParams{ID: tgt.ID, Version: req.GetTargetBaseVersion()}); errors.Is(e, pgx.ErrNoRows) {
			return connecterr.StaleVersion("target order changed; review and retry")
		} else if e != nil {
			return connecterr.Internal(e)
		}
		if e := q.MoveOrderItemsToOrder(ctx, sqlc.MoveOrderItemsToOrderParams{OrderID: src.ID, OrderID_2: tgt.ID}); e != nil {
			return connecterr.Internal(e)
		}
		if e := q.SetOrderStatus(ctx, sqlc.SetOrderStatusParams{ID: src.ID, Status: "CLOSED"}); e != nil {
			return connecterr.Internal(e)
		}
		if e := audit.Write(ctx, q, audit.Entry{ActorID: actor.User.ID, Action: "order.merge", Entity: "order", EntityID: tgt.ID.String(), After: audit.JSON(map[string]string{"merged_from": src.ID.String()})}); e != nil {
			return e
		}
		ord, e := s.assembleOrder(ctx, q, tgt.ID)
		if e != nil {
			return e
		}
		out.Order = ord
		return nil
	})
	if err != nil {
		return nil, err
	}
	if srcID != "" {
		s.emit(realtime.Event{Type: "order.updated", ID: srcID})
	}
	s.emit(realtime.Event{Type: "order.updated", ID: out.Order.Id, Version: out.Order.Version})
	s.emit(realtime.Event{Type: "table.updated"})
	return out, nil
}

// --- small helpers ---

func orderTypeStr(t restaurantv1.OrderType) string {
	if t == restaurantv1.OrderType_ORDER_TYPE_TAKEAWAY {
		return "TAKEAWAY"
	}
	return "DINE_IN"
}

func rejected(clientID string, reason restaurantv1.ErrorReason, msg string) *restaurantv1.RejectedLine {
	return &restaurantv1.RejectedLine{ClientId: clientID, Reason: reason, Message: msg}
}

// mustUUID parses an id that protovalidate already constrained to be non-empty;
// a parse failure yields the zero UUID (which will simply miss any lookup).
func mustUUID(s string) uuid.UUID {
	id, _ := uuid.Parse(s)
	return id
}
