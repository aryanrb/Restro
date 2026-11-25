// Package catalog is the admin domain: settings, floor (areas/tables), stations and
// menu. All writes require OWNER/MANAGER, are audited in-transaction, and emit a
// realtime poke. Reads require any session.
package catalog

import (
	"encoding/base64"
	"errors"
	"fmt"
	"slices"
	"strings"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
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

// --- move helper ---

type sortable struct {
	ID   uuid.UUID
	Sort int32
}

// moveWithin moves target one step up/down among its siblings (already in display
// order) and renumbers them 1..n, so duplicate or gapped sort values heal. It
// returns the rows whose sort changed (none at the ends) and whether target is
// among the siblings.
func moveWithin(siblings []sortable, target uuid.UUID, up bool) (changed []sortable, found bool) {
	pos := -1
	for i, it := range siblings {
		if it.ID == target {
			pos = i
			break
		}
	}
	if pos < 0 {
		return nil, false
	}
	n := pos + 1
	if up {
		n = pos - 1
	}
	if n < 0 || n >= len(siblings) {
		return nil, true
	}
	order := append([]sortable(nil), siblings...)
	order[pos], order[n] = order[n], order[pos]
	for i, it := range order {
		if want := int32(i + 1); it.Sort != want {
			changed = append(changed, sortable{ID: it.ID, Sort: want})
		}
	}
	return changed, true
}

// --- shared checks ---

// requireName rejects a blank English name (the Nepali one is optional).
func requireName(name *restaurantv1.LocalText) *connecterr.Error {
	if strings.TrimSpace(name.GetEn()) == "" {
		return connecterr.Invalid("name", "English name is required")
	}
	return nil
}

// itemColors mirrors the web palette (web/src/lib/admin/colors.ts ITEM_COLORS):
// each keeps white tile text readable.
var itemColors = []string{
	"#b91c1c", "#c2410c", "#a16207", "#15803d", "#0f766e",
	"#0369a1", "#1d4ed8", "#6d28d9", "#be185d", "#44403c",
}

// paletteColor returns the canonical (lower-case) palette colour, or ok=false.
func paletteColor(c string) (string, bool) {
	c = strings.ToLower(strings.TrimSpace(c))
	if slices.Contains(itemColors, c) {
		return c, true
	}
	return "", false
}

const maxImageBytes = 1 << 20 // 1 MB

// imageDataURL validates an uploaded image (item or settings) and returns it as a
// data URL; an empty image returns "" (clear).
func imageDataURL(image []byte, contentType string) (string, *connecterr.Error) {
	if len(image) == 0 {
		return "", nil
	}
	if len(image) > maxImageBytes {
		return "", connecterr.Invalid("image", "image must be 1 MB or smaller")
	}
	switch contentType {
	case "image/jpeg", "image/png", "image/webp":
	default:
		return "", connecterr.Invalid("image", "image must be JPEG, PNG or WebP")
	}
	return fmt.Sprintf("data:%s;base64,%s", contentType, base64.StdEncoding.EncodeToString(image)), nil
}

// notFound maps pgx.ErrNoRows to NOT_FOUND and anything else to Internal.
func notFound(err error, what string) *connecterr.Error {
	if errors.Is(err, pgx.ErrNoRows) {
		return connecterr.NotFound(what + " not found")
	}
	return connecterr.Internal(err)
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
		QrImages: qrImages(s), GuestOrdering: s.GuestOrdering,
	}
}

// qrImages lists the configured payment QRs, Fonepay first.
func qrImages(s sqlc.Setting) []*restaurantv1.QrImage {
	out := []*restaurantv1.QrImage{}
	if s.QrFonepayUrl != "" {
		out = append(out, &restaurantv1.QrImage{Provider: restaurantv1.QrProvider_QR_PROVIDER_FONEPAY, Url: s.QrFonepayUrl})
	}
	if s.QrOtherUrl != "" {
		out = append(out, &restaurantv1.QrImage{Provider: restaurantv1.QrProvider_QR_PROVIDER_OTHER, Url: s.QrOtherUrl})
	}
	return out
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
