// Package catalog is the admin domain: settings, floor (areas/tables), stations and
// menu. All writes require OWNER/MANAGER, are audited in-transaction, and emit a
// realtime poke. Reads require any session.
package catalog

import (
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"
	"go.uber.org/zap"

	restaurantv1 "github.com/oneaccord/restro/api/gen/restaurant/v1"
	"github.com/oneaccord/restro/api/internal/connecterr"
	"github.com/oneaccord/restro/api/internal/db/sqlc"
	"github.com/oneaccord/restro/api/internal/realtime"
)

// parseID parses a UUID, returning an INVALID error naming the field on failure.
func parseID(field, s string) (uuid.UUID, error) {
	id, err := uuid.Parse(s)
	if err != nil {
		return uuid.Nil, connecterr.Invalid(field, "invalid id")
	}
	return id, nil
}

// Service owns catalog business logic, DB access and errors.
type Service struct {
	pool *pgxpool.Pool
	q    *sqlc.Queries
	hub  *realtime.Hub
	log  *zap.Logger
}

func NewService(pool *pgxpool.Pool, hub *realtime.Hub, log *zap.Logger) *Service {
	return &Service{pool: pool, q: sqlc.New(pool), hub: hub, log: log}
}

func (s *Service) emit(ev realtime.Event) {
	if s.hub != nil {
		s.hub.Broadcast(ev)
	}
}

// --- move helper (sort swapping) ---

type sortable struct {
	ID   uuid.UUID
	Sort int32
}

// neighborSwap returns the two rows whose sort values must be swapped to move
// target up/down, or ok=false at the ends.
func neighborSwap(items []sortable, target uuid.UUID, up bool) (a, b sortable, ok bool) {
	pos := -1
	for i, it := range items {
		if it.ID == target {
			pos = i
			break
		}
	}
	if pos < 0 {
		return a, b, false
	}
	n := pos + 1
	if up {
		n = pos - 1
	}
	if n < 0 || n >= len(items) {
		return a, b, false
	}
	a, b = items[pos], items[n]
	a.Sort, b.Sort = b.Sort, a.Sort
	return a, b, true
}

func moveUp(m restaurantv1.Move) bool { return m == restaurantv1.Move_MOVE_UP }

// --- proto converters ---

func local(en, ne string) *restaurantv1.LocalText { return &restaurantv1.LocalText{En: en, Ne: ne} }

func toSettings(s sqlc.Setting) *restaurantv1.Settings {
	return &restaurantv1.Settings{
		Version: s.Version, Name: local(s.NameEn, s.NameNe), Address: s.Address, Phone: s.Phone,
		PanNumber: s.PanNumber, VatRegistered: s.VatRegistered, PricesIncludeVat: s.PricesIncludeVat,
		VatRateBp: s.VatRateBp, ServiceChargeEnabled: s.ServiceChargeEnabled, ServiceChargeRateBp: s.ServiceChargeRateBp,
		BillingMode: billingModeEnum(s.BillingMode), ReceiptWidth: receiptWidthEnum(s.ReceiptWidth),
		RoundTotalToRupee: s.RoundTotalToRupee, DefaultLanguage: langEnum(s.DefaultLanguage),
		KdsYellowMinutes: s.KdsYellowMinutes, KdsRedMinutes: s.KdsRedMinutes, LogoUrl: s.LogoUrl,
	}
}

func toArea(a sqlc.Area) *restaurantv1.Area {
	return &restaurantv1.Area{Id: a.ID.String(), Name: local(a.NameEn, a.NameNe), Sort: a.Sort}
}

func toTable(t sqlc.DiningTable) *restaurantv1.Table {
	return &restaurantv1.Table{Id: t.ID.String(), AreaId: t.AreaID.String(), Name: t.Name, Seats: t.Seats, Sort: t.Sort, Active: t.Active}
}

func toStation(s sqlc.Station) *restaurantv1.Station {
	return &restaurantv1.Station{Id: s.ID.String(), Name: local(s.NameEn, s.NameNe), Sort: s.Sort}
}

func toCategory(c sqlc.MenuCategory) *restaurantv1.Category {
	return &restaurantv1.Category{Id: c.ID.String(), Name: local(c.NameEn, c.NameNe), Sort: c.Sort, Active: c.Active}
}

func toVariant(v sqlc.ItemVariant) *restaurantv1.Variant {
	return &restaurantv1.Variant{Id: v.ID.String(), Name: local(v.NameEn, v.NameNe), PricePaisa: v.PricePaisa}
}

func toModifier(m sqlc.Modifier) *restaurantv1.Modifier {
	return &restaurantv1.Modifier{Id: m.ID.String(), Name: local(m.NameEn, m.NameNe), PriceDeltaPaisa: m.PriceDeltaPaisa}
}

func toModifierGroup(g sqlc.ModifierGroup, mods []sqlc.Modifier) *restaurantv1.ModifierGroup {
	out := &restaurantv1.ModifierGroup{
		Id: g.ID.String(), Version: g.Version, Name: local(g.NameEn, g.NameNe),
		Min: g.MinSelect, Max: g.MaxSelect, Modifiers: make([]*restaurantv1.Modifier, 0, len(mods)),
	}
	for _, m := range mods {
		out.Modifiers = append(out.Modifiers, toModifier(m))
	}
	return out
}

func toMenuItem(it sqlc.MenuItem, variants []sqlc.ItemVariant, groupIDs []string) *restaurantv1.MenuItem {
	out := &restaurantv1.MenuItem{
		Id: it.ID.String(), Version: it.Version, CategoryId: it.CategoryID.String(), Name: local(it.NameEn, it.NameNe),
		PricePaisa: it.PricePaisa, Color: it.Color, StationId: it.StationID.String(), Active: it.Active,
		Is_86D: it.Is86d, Sort: it.Sort, Variants: make([]*restaurantv1.Variant, 0, len(variants)),
		ModifierGroupIds: groupIDs,
	}
	if it.ImageUrl != "" {
		out.ImageUrl = &it.ImageUrl
	}
	for _, v := range variants {
		out.Variants = append(out.Variants, toVariant(v))
	}
	return out
}

// --- enum <-> string ---

func billingModeEnum(s string) restaurantv1.BillingMode {
	if s == "TAX_INVOICE" {
		return restaurantv1.BillingMode_BILLING_MODE_TAX_INVOICE
	}
	return restaurantv1.BillingMode_BILLING_MODE_ESTIMATE
}

func receiptWidthEnum(s string) restaurantv1.ReceiptWidth {
	if s == "58MM" {
		return restaurantv1.ReceiptWidth_RECEIPT_WIDTH_58MM
	}
	return restaurantv1.ReceiptWidth_RECEIPT_WIDTH_80MM
}

func receiptWidthStr(w restaurantv1.ReceiptWidth) string {
	if w == restaurantv1.ReceiptWidth_RECEIPT_WIDTH_58MM {
		return "58MM"
	}
	return "80MM"
}

func langEnum(s string) restaurantv1.Language {
	if s == "ne" {
		return restaurantv1.Language_LANGUAGE_NE
	}
	return restaurantv1.Language_LANGUAGE_EN
}

func langStr(l restaurantv1.Language) string {
	if l == restaurantv1.Language_LANGUAGE_NE {
		return "ne"
	}
	return "en"
}
