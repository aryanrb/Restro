package catalog

import (
	"bytes"
	"context"
	"errors"
	"os"
	"testing"

	"connectrpc.com/connect"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"
	"go.uber.org/zap"

	restaurantv1 "github.com/oneaccord/restro/api/gen/restaurant/v1"
	"github.com/oneaccord/restro/api/internal/auth"
	"github.com/oneaccord/restro/api/internal/connecterr"
	"github.com/oneaccord/restro/api/internal/db"
	"github.com/oneaccord/restro/api/internal/db/sqlc"
	"github.com/oneaccord/restro/api/internal/dbx"
)

const (
	red  = "#b91c1c"
	blue = "#1d4ed8"
)

type fixture struct {
	t    *testing.T
	pool *pgxpool.Pool
	q    *sqlc.Queries
	svc  *Service
	ctx  context.Context // a MANAGER session
}

func newFixture(t *testing.T) *fixture {
	t.Helper()
	url := os.Getenv("TEST_DATABASE_URL")
	if url == "" {
		t.Skip("set TEST_DATABASE_URL to run catalog integration tests")
	}
	ctx := context.Background()
	pool, err := db.New(ctx, url)
	if err != nil {
		t.Fatalf("db.New: %v", err)
	}
	t.Cleanup(pool.Close)
	if err := db.RunMigrations(ctx, pool); err != nil {
		t.Fatalf("migrate: %v", err)
	}
	if _, err := pool.Exec(ctx, `TRUNCATE users, sessions, devices, audit_log, item_modifier_groups, modifiers,
		modifier_groups, item_variants, menu_items, menu_categories, stations, dining_tables, areas RESTART IDENTITY CASCADE`); err != nil {
		t.Fatalf("truncate: %v", err)
	}
	if _, err := pool.Exec(ctx, `DELETE FROM settings; INSERT INTO settings (id) VALUES (1)`); err != nil {
		t.Fatalf("reset settings: %v", err)
	}
	f := &fixture{t: t, pool: pool, q: sqlc.New(pool), svc: NewService(pool, nil, zap.NewNop())}
	f.ctx = f.as(auth.RoleManager)
	return f
}

// as resolves a real session for a fresh user of the given role through the auth
// interceptor, exactly as a request would be.
func (f *fixture) as(role string) context.Context {
	f.t.Helper()
	ctx := context.Background()
	pw, err := auth.HashSecret("secret123")
	if err != nil {
		f.t.Fatalf("hash: %v", err)
	}
	username := "u-" + uuid.NewString()[:8]
	if _, err := f.q.CreateUser(ctx, sqlc.CreateUserParams{
		Name: role, Role: role, Username: dbx.Text(username), PasswordHash: dbx.Text(pw), Language: "en", Active: true, OrganisationID: dbx.BootstrapOrgID(ctx, f.q),
	}); err != nil {
		f.t.Fatalf("create user: %v", err)
	}
	authSvc := auth.NewService(f.pool, zap.NewNop())
	_, token, err := authSvc.PasswordLogin(ctx, username, "secret123")
	if err != nil {
		f.t.Fatalf("login: %v", err)
	}
	req := connect.NewRequest(&restaurantv1.GetMenuRequest{})
	req.Header().Set("Authorization", "Bearer "+token)
	var out context.Context
	_, _ = authSvc.Interceptor()(func(c context.Context, _ connect.AnyRequest) (connect.AnyResponse, error) {
		out = c
		return nil, nil
	})(ctx, req)
	if !auth.ActorFrom(out).LoggedIn() {
		f.t.Fatalf("actor not resolved")
	}
	return out
}

func wantReason(t *testing.T, err error, reason restaurantv1.ErrorReason, field string) {
	t.Helper()
	var ae *connecterr.Error
	if !errors.As(err, &ae) {
		t.Fatalf("want *connecterr.Error %v, got %v", reason, err)
	}
	if ae.Reason != reason {
		t.Fatalf("reason = %v (%v), want %v", ae.Reason, ae, reason)
	}
	if field != "" && ae.Field != field {
		t.Fatalf("field = %q, want %q", ae.Field, field)
	}
}

func wantNotFound(t *testing.T, err error) {
	t.Helper()
	wantReason(t, err, restaurantv1.ErrorReason_ERROR_REASON_NOT_FOUND, "")
}

func wantInvalid(t *testing.T, err error, field string) {
	t.Helper()
	wantReason(t, err, restaurantv1.ErrorReason_ERROR_REASON_INVALID, field)
}

func lt(en string) *restaurantv1.LocalText { return &restaurantv1.LocalText{En: en} }
func ptr[T any](v T) *T                    { return &v }

func (f *fixture) station(name string) string {
	f.t.Helper()
	st, err := f.svc.SaveStation(f.ctx, "", lt(name))
	if err != nil {
		f.t.Fatalf("save station: %v", err)
	}
	return st.Id
}

func (f *fixture) category(name string) string {
	f.t.Helper()
	c, err := f.svc.SaveCategory(f.ctx, "", lt(name), true)
	if err != nil {
		f.t.Fatalf("save category: %v", err)
	}
	return c.Id
}

func (f *fixture) item(cat, station, name string) *restaurantv1.MenuItem {
	f.t.Helper()
	it, err := f.svc.SaveItem(f.ctx, &restaurantv1.MenuItemInput{
		CategoryId: cat, StationId: station, Name: lt(name), PricePaisa: 10000, Color: red, Active: true,
	}, nil)
	if err != nil {
		f.t.Fatalf("save item %s: %v", name, err)
	}
	return it
}

func (f *fixture) area(name string) string {
	f.t.Helper()
	a, err := f.svc.SaveArea(f.ctx, "", lt(name))
	if err != nil {
		f.t.Fatalf("save area: %v", err)
	}
	return a.Id
}

func (f *fixture) table(area, name string) *restaurantv1.Table {
	f.t.Helper()
	tb, err := f.svc.SaveTable(f.ctx, "", area, name, 4, true)
	if err != nil {
		f.t.Fatalf("save table %s: %v", name, err)
	}
	return tb
}

// --- variants / modifiers upsert ---

func TestSaveItemUpsertsVariantsByID(t *testing.T) {
	f := newFixture(t)
	st, cat := f.station("Kitchen"), f.category("Momo")
	it, err := f.svc.SaveItem(f.ctx, &restaurantv1.MenuItemInput{
		CategoryId: cat, StationId: st, Name: lt("Momo"), Color: red, Active: true,
		Variants: []*restaurantv1.VariantInput{
			{Name: lt("Half"), PricePaisa: 15000},
			{Name: lt("Full"), PricePaisa: 25000},
		},
	}, nil)
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	half, full := it.Variants[0].Id, it.Variants[1].Id

	// Edit Full (rename + reprice), drop Half, add Family; Full moves first.
	it2, err := f.svc.SaveItem(f.ctx, &restaurantv1.MenuItemInput{
		Id: ptr(it.Id), CategoryId: cat, StationId: st, Name: lt("Momo"), Color: red, Active: true,
		Variants: []*restaurantv1.VariantInput{
			{Id: ptr(full), Name: lt("Full plate"), PricePaisa: 26000},
			{Name: lt("Family"), PricePaisa: 50000},
		},
	}, ptr(it.Version))
	if err != nil {
		t.Fatalf("update: %v", err)
	}
	if len(it2.Variants) != 2 {
		t.Fatalf("variants = %d, want 2", len(it2.Variants))
	}
	if it2.Variants[0].Id != full || it2.Variants[0].Name.En != "Full plate" || it2.Variants[0].PricePaisa != 26000 {
		t.Fatalf("Full variant not updated in place: %+v", it2.Variants[0])
	}
	if it2.Variants[1].Id == half || it2.Variants[1].Id == full || it2.Variants[1].Name.En != "Family" {
		t.Fatalf("new variant wrong: %+v", it2.Variants[1])
	}
	if _, err := f.q.GetVariant(context.Background(), uuid.MustParse(half)); err == nil {
		t.Fatalf("removed variant Half still exists")
	}

	// A variant id that belongs to another item is rejected.
	other, err := f.svc.SaveItem(f.ctx, &restaurantv1.MenuItemInput{
		CategoryId: cat, StationId: st, Name: lt("Other"), Color: red, Active: true,
		Variants: []*restaurantv1.VariantInput{{Id: ptr(full), Name: lt("Stolen"), PricePaisa: 100}},
	}, nil)
	if other != nil || err == nil {
		t.Fatalf("want error for foreign variant id, got %+v", other)
	}
	wantInvalid(t, err, "variants.0.id")
}

func TestSaveModifierGroupUpsertsModifiersByID(t *testing.T) {
	f := newFixture(t)
	g, err := f.svc.SaveModifierGroup(f.ctx, &restaurantv1.ModifierGroupInput{
		Name: lt("Extras"), Min: 0, Max: 2,
		Modifiers: []*restaurantv1.ModifierInput{
			{Name: lt("Cheese"), PriceDeltaPaisa: 5000},
			{Name: lt("Egg"), PriceDeltaPaisa: 3000},
			{Name: lt("Olive"), PriceDeltaPaisa: 1000},
		},
	}, nil)
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	cheese, egg, olive := g.Modifiers[0].Id, g.Modifiers[1].Id, g.Modifiers[2].Id

	g2, err := f.svc.SaveModifierGroup(f.ctx, &restaurantv1.ModifierGroupInput{
		Id: ptr(g.Id), Name: lt("Extras"), Min: 0, Max: 2,
		Modifiers: []*restaurantv1.ModifierInput{
			{Id: ptr(olive), Name: lt("Olives"), PriceDeltaPaisa: 1500},
			{Id: ptr(cheese), Name: lt("Cheese"), PriceDeltaPaisa: 5000},
			{Name: lt("Bacon"), PriceDeltaPaisa: 8000},
		},
	}, ptr(g.Version))
	if err != nil {
		t.Fatalf("update: %v", err)
	}
	got := []string{g2.Modifiers[0].Id, g2.Modifiers[1].Id}
	if got[0] != olive || got[1] != cheese {
		t.Fatalf("ids not kept/ordered: %v want [%s %s]", got, olive, cheese)
	}
	if g2.Modifiers[0].Name.En != "Olives" || g2.Modifiers[0].PriceDeltaPaisa != 1500 {
		t.Fatalf("olive not updated: %+v", g2.Modifiers[0])
	}
	if id := g2.Modifiers[2].Id; id == egg || id == cheese || id == olive {
		t.Fatalf("Bacon should be new, got %s", id)
	}
	if _, err := f.q.GetModifierByID(context.Background(), uuid.MustParse(egg)); err == nil {
		t.Fatalf("removed modifier Egg still exists")
	}

	// A modifier id from another group is rejected.
	_, err = f.svc.SaveModifierGroup(f.ctx, &restaurantv1.ModifierGroupInput{
		Name: lt("Spice"), Min: 0, Max: 1,
		Modifiers: []*restaurantv1.ModifierInput{{Id: ptr(cheese), Name: lt("Hot")}},
	}, nil)
	wantInvalid(t, err, "modifiers.0.id")
}

// --- moves ---

func itemOrder(t *testing.T, f *fixture, cat string) []string {
	t.Helper()
	m, err := f.svc.GetMenu(f.ctx)
	if err != nil {
		t.Fatalf("get menu: %v", err)
	}
	var names []string
	for _, it := range m.Items {
		if it.CategoryId == cat {
			names = append(names, it.Name.En)
		}
	}
	return names
}

func eq(a []string, b ...string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

func TestMoveItemStaysWithinCategory(t *testing.T) {
	f := newFixture(t)
	st := f.station("Kitchen")
	catA, catB := f.category("A"), f.category("B")
	a1 := f.item(catA, st, "A1") // sort 1 in A
	f.item(catB, st, "B1")       // sort 1 in B
	a2 := f.item(catA, st, "A2") // sort 2 in A

	// Globally the list is A1(1) B1(1) A2(2); moving A2 up must swap with A1, not B1.
	if err := f.svc.MoveItem(f.ctx, a2.Id, restaurantv1.Move_MOVE_UP); err != nil {
		t.Fatalf("move: %v", err)
	}
	if got := itemOrder(t, f, catA); !eq(got, "A2", "A1") {
		t.Fatalf("category A order = %v, want [A2 A1]", got)
	}
	if got := itemOrder(t, f, catB); !eq(got, "B1") {
		t.Fatalf("category B order = %v, want [B1]", got)
	}
	// Top of its category: moving up again is a no-op.
	if err := f.svc.MoveItem(f.ctx, a2.Id, restaurantv1.Move_MOVE_UP); err != nil {
		t.Fatalf("move: %v", err)
	}
	if got := itemOrder(t, f, catA); !eq(got, "A2", "A1") {
		t.Fatalf("category A order = %v, want [A2 A1]", got)
	}

	// Moving A1 into B puts it last there.
	cur, _ := f.q.GetItem(context.Background(), uuid.MustParse(a1.Id))
	if _, err := f.svc.SaveItem(f.ctx, &restaurantv1.MenuItemInput{
		Id: ptr(a1.Id), CategoryId: catB, StationId: st, Name: lt("A1"), PricePaisa: 10000, Color: red, Active: true,
	}, ptr(cur.Version)); err != nil {
		t.Fatalf("recategorise: %v", err)
	}
	f.item(catA, st, "A3")
	if got := itemOrder(t, f, catB); !eq(got, "B1", "A1") {
		t.Fatalf("category B order = %v, want [B1 A1]", got)
	}

	wantNotFound(t, f.svc.MoveItem(f.ctx, uuid.NewString(), restaurantv1.Move_MOVE_UP))
}

func tableOrder(t *testing.T, f *fixture, area string) []string {
	t.Helper()
	fl, err := f.svc.GetFloor(f.ctx)
	if err != nil {
		t.Fatalf("get floor: %v", err)
	}
	var names []string
	for _, tb := range fl.Tables {
		if tb.AreaId == area {
			names = append(names, tb.Name)
		}
	}
	return names
}

func TestMoveTableStaysWithinArea(t *testing.T) {
	f := newFixture(t)
	hall, roof := f.area("Hall"), f.area("Roof")
	t1 := f.table(hall, "T1")
	f.table(roof, "R1")
	f.table(roof, "R2")
	t2 := f.table(hall, "T2")

	if err := f.svc.MoveTable(f.ctx, t2.Id, restaurantv1.Move_MOVE_UP); err != nil {
		t.Fatalf("move: %v", err)
	}
	if got := tableOrder(t, f, hall); !eq(got, "T2", "T1") {
		t.Fatalf("hall order = %v, want [T2 T1]", got)
	}
	if got := tableOrder(t, f, roof); !eq(got, "R1", "R2") {
		t.Fatalf("roof order = %v, want [R1 R2]", got)
	}

	// Moving T1 to the roof puts it last there.
	if _, err := f.svc.SaveTable(f.ctx, t1.Id, roof, "T1", 4, true); err != nil {
		t.Fatalf("move area: %v", err)
	}
	if got := tableOrder(t, f, roof); !eq(got, "R1", "R2", "T1") {
		t.Fatalf("roof order = %v, want [R1 R2 T1]", got)
	}
	// Then moving it up swaps with R2, inside the roof.
	if err := f.svc.MoveTable(f.ctx, t1.Id, restaurantv1.Move_MOVE_UP); err != nil {
		t.Fatalf("move: %v", err)
	}
	if got := tableOrder(t, f, roof); !eq(got, "R1", "T1", "R2") {
		t.Fatalf("roof order = %v, want [R1 T1 R2]", got)
	}

	wantNotFound(t, f.svc.MoveTable(f.ctx, uuid.NewString(), restaurantv1.Move_MOVE_DOWN))
}

// --- settings ---

func settingsInput(s *restaurantv1.Settings) *restaurantv1.SettingsInput {
	return &restaurantv1.SettingsInput{
		Name: lt("Cafe"), VatRateBp: s.VatRateBp, ServiceChargeRateBp: s.ServiceChargeRateBp,
		ReceiptWidth: s.ReceiptWidth, DefaultLanguage: s.DefaultLanguage,
		KdsYellowMinutes: s.KdsYellowMinutes, KdsRedMinutes: s.KdsRedMinutes,
	}
}

func TestSettingsGuestOrderingAndKdsOrder(t *testing.T) {
	f := newFixture(t)
	s, err := f.svc.GetSettings(f.ctx)
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	if s.GuestOrdering {
		t.Fatalf("guest_ordering should default to false")
	}

	in := settingsInput(s)
	in.GuestOrdering = ptr(true)
	s, err = f.svc.UpdateSettings(f.ctx, in, s.Version)
	if err != nil {
		t.Fatalf("update: %v", err)
	}
	if !s.GuestOrdering {
		t.Fatalf("guest_ordering not stored")
	}
	// Unset leaves it unchanged.
	in = settingsInput(s)
	s, err = f.svc.UpdateSettings(f.ctx, in, s.Version)
	if err != nil {
		t.Fatalf("update: %v", err)
	}
	if !s.GuestOrdering {
		t.Fatalf("unset guest_ordering should keep true")
	}

	in = settingsInput(s)
	in.KdsYellowMinutes, in.KdsRedMinutes = 20, 20
	_, err = f.svc.UpdateSettings(f.ctx, in, s.Version)
	wantInvalid(t, err, "kds_yellow_minutes")
}

func TestSetSettingsImage(t *testing.T) {
	f := newFixture(t)
	before, _ := f.svc.GetSettings(f.ctx)
	png := []byte("\x89PNG fake")

	s, err := f.svc.SetSettingsImage(f.ctx, restaurantv1.SettingsImageKind_SETTINGS_IMAGE_KIND_LOGO, png, "image/png")
	if err != nil {
		t.Fatalf("logo: %v", err)
	}
	if s.LogoUrl == "" || !bytes.HasPrefix([]byte(s.LogoUrl), []byte("data:image/png;base64,")) {
		t.Fatalf("logo url = %q", s.LogoUrl)
	}
	if s.Version != before.Version {
		t.Fatalf("image upload should not bump settings version")
	}
	s, err = f.svc.SetSettingsImage(f.ctx, restaurantv1.SettingsImageKind_SETTINGS_IMAGE_KIND_QR_FONEPAY, png, "image/png")
	if err != nil {
		t.Fatalf("fonepay: %v", err)
	}
	s, err = f.svc.SetSettingsImage(f.ctx, restaurantv1.SettingsImageKind_SETTINGS_IMAGE_KIND_QR_OTHER, []byte("jpg"), "image/jpeg")
	if err != nil {
		t.Fatalf("other: %v", err)
	}
	if len(s.QrImages) != 2 || s.QrImages[0].Provider != restaurantv1.QrProvider_QR_PROVIDER_FONEPAY ||
		s.QrImages[1].Provider != restaurantv1.QrProvider_QR_PROVIDER_OTHER || s.QrImages[1].Url == "" {
		t.Fatalf("qr_images = %+v", s.QrImages)
	}

	// Clearing one leaves the other.
	s, err = f.svc.SetSettingsImage(f.ctx, restaurantv1.SettingsImageKind_SETTINGS_IMAGE_KIND_QR_FONEPAY, nil, "")
	if err != nil {
		t.Fatalf("clear: %v", err)
	}
	if len(s.QrImages) != 1 || s.QrImages[0].Provider != restaurantv1.QrProvider_QR_PROVIDER_OTHER {
		t.Fatalf("after clear qr_images = %+v", s.QrImages)
	}
	if s.LogoUrl == "" {
		t.Fatalf("logo should be untouched")
	}

	_, err = f.svc.SetSettingsImage(f.ctx, restaurantv1.SettingsImageKind_SETTINGS_IMAGE_KIND_LOGO, make([]byte, maxImageBytes+1), "image/png")
	wantInvalid(t, err, "image")
	_, err = f.svc.SetSettingsImage(f.ctx, restaurantv1.SettingsImageKind_SETTINGS_IMAGE_KIND_LOGO, png, "image/gif")
	wantInvalid(t, err, "image")
	_, err = f.svc.SetSettingsImage(f.ctx, restaurantv1.SettingsImageKind_SETTINGS_IMAGE_KIND_UNSPECIFIED, png, "image/png")
	wantInvalid(t, err, "kind")

	_, err = f.svc.SetSettingsImage(f.as(auth.RoleCashier), restaurantv1.SettingsImageKind_SETTINGS_IMAGE_KIND_LOGO, png, "image/png")
	wantReason(t, err, restaurantv1.ErrorReason_ERROR_REASON_NOT_ALLOWED, "")
}

// --- server-side checks ---

func TestBlankNamesRejected(t *testing.T) {
	f := newFixture(t)
	st, cat := f.station("Kitchen"), f.category("Food")

	_, err := f.svc.SaveArea(f.ctx, "", lt("  "))
	wantInvalid(t, err, "name")
	_, err = f.svc.SaveStation(f.ctx, "", lt(""))
	wantInvalid(t, err, "name")
	_, err = f.svc.SaveCategory(f.ctx, "", lt(" "), true)
	wantInvalid(t, err, "name")
	_, err = f.svc.SaveItem(f.ctx, &restaurantv1.MenuItemInput{
		CategoryId: cat, StationId: st, Name: lt("   "), PricePaisa: 100, Color: red, Active: true,
	}, nil)
	wantInvalid(t, err, "name")
}

func TestItemColorMustBeInPalette(t *testing.T) {
	f := newFixture(t)
	st, cat := f.station("Kitchen"), f.category("Food")
	for _, c := range []string{"", "#123456", "red"} {
		_, err := f.svc.SaveItem(f.ctx, &restaurantv1.MenuItemInput{
			CategoryId: cat, StationId: st, Name: lt("Tea"), PricePaisa: 100, Color: c, Active: true,
		}, nil)
		wantInvalid(t, err, "color")
	}
	it, err := f.svc.SaveItem(f.ctx, &restaurantv1.MenuItemInput{
		CategoryId: cat, StationId: st, Name: lt("Tea"), PricePaisa: 100, Color: "#1D4ED8", Active: true,
	}, nil)
	if err != nil {
		t.Fatalf("palette colour (any case) rejected: %v", err)
	}
	if it.Color != blue {
		t.Fatalf("colour stored as %q, want %q", it.Color, blue)
	}
}

func TestMissingReferencesAreNotFound(t *testing.T) {
	f := newFixture(t)
	st, cat, area := f.station("Kitchen"), f.category("Food"), f.area("Hall")
	missing := uuid.NewString()

	_, err := f.svc.SaveArea(f.ctx, missing, lt("X"))
	wantNotFound(t, err)
	wantNotFound(t, f.svc.DeleteArea(f.ctx, missing))
	wantNotFound(t, f.svc.MoveArea(f.ctx, missing, restaurantv1.Move_MOVE_UP))

	_, err = f.svc.SaveTable(f.ctx, "", missing, "T1", 2, true)
	wantNotFound(t, err)
	_, err = f.svc.SaveTable(f.ctx, missing, area, "T1", 2, true)
	wantNotFound(t, err)

	_, err = f.svc.SaveStation(f.ctx, missing, lt("X"))
	wantNotFound(t, err)
	wantNotFound(t, f.svc.DeleteStation(f.ctx, missing))

	_, err = f.svc.SaveCategory(f.ctx, missing, lt("X"), true)
	wantNotFound(t, err)
	wantNotFound(t, f.svc.DeleteCategory(f.ctx, missing))
	wantNotFound(t, f.svc.MoveCategory(f.ctx, missing, restaurantv1.Move_MOVE_UP))

	item := func(id *string, cat, st string, groups ...string) error {
		_, err := f.svc.SaveItem(f.ctx, &restaurantv1.MenuItemInput{
			Id: id, CategoryId: cat, StationId: st, Name: lt("Tea"), PricePaisa: 100, Color: red, Active: true,
			ModifierGroupIds: groups,
		}, ptr(int64(1)))
		return err
	}
	wantNotFound(t, item(nil, missing, st))
	wantNotFound(t, item(nil, cat, missing))
	wantNotFound(t, item(nil, cat, st, missing))
	wantNotFound(t, item(ptr(missing), cat, st))

	_, err = f.svc.SetItemImage(f.ctx, missing, nil, "")
	wantNotFound(t, err)

	_, err = f.svc.SaveModifierGroup(f.ctx, &restaurantv1.ModifierGroupInput{
		Id: ptr(missing), Name: lt("G"), Min: 0, Max: 1, Modifiers: []*restaurantv1.ModifierInput{{Name: lt("M")}},
	}, ptr(int64(1)))
	wantNotFound(t, err)
	wantNotFound(t, f.svc.DeleteModifierGroup(f.ctx, missing))
}
