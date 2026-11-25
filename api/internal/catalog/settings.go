package catalog

import (
	"context"
	"errors"

	"github.com/jackc/pgx/v5"

	restaurantv1 "github.com/oneaccord/restro/api/gen/restaurant/v1"
	"github.com/oneaccord/restro/api/internal/audit"
	"github.com/oneaccord/restro/api/internal/auth"
	"github.com/oneaccord/restro/api/internal/connecterr"
	"github.com/oneaccord/restro/api/internal/db/sqlc"
	"github.com/oneaccord/restro/api/internal/dbx"
	"github.com/oneaccord/restro/api/internal/realtime"
)

func (s *Service) GetSettings(ctx context.Context) (*restaurantv1.Settings, error) {
	if _, err := auth.RequireActor(ctx); err != nil {
		return nil, err
	}
	row, err := s.q.GetSettings(ctx)
	if err != nil {
		return nil, connecterr.Internal(err)
	}
	return toSettings(row), nil
}

func (s *Service) UpdateSettings(ctx context.Context, in *restaurantv1.SettingsInput, version int64) (*restaurantv1.Settings, error) {
	actor, err := auth.RequireManager(ctx)
	if err != nil {
		return nil, err
	}
	if in.GetKdsYellowMinutes() >= in.GetKdsRedMinutes() {
		return nil, connecterr.Invalid("kds_yellow_minutes", "the yellow KDS time must be less than the red one")
	}
	var updated sqlc.Setting
	err = dbx.WithTx(ctx, s.pool, s.q, func(q *sqlc.Queries) error {
		before, e := q.GetSettings(ctx)
		if e != nil {
			return connecterr.Internal(e)
		}
		guest := before.GuestOrdering
		if in.GuestOrdering != nil {
			guest = in.GetGuestOrdering()
		}
		u, e := q.UpdateSettings(ctx, sqlc.UpdateSettingsParams{
			Version: version,
			NameEn:  in.GetName().GetEn(), NameNe: in.GetName().GetNe(),
			Address: in.GetAddress(), Phone: in.GetPhone(), PanNumber: in.GetPanNumber(),
			VatRegistered: in.GetVatRegistered(), PricesIncludeVat: in.GetPricesIncludeVat(), VatRateBp: in.GetVatRateBp(),
			ServiceChargeEnabled: in.GetServiceChargeEnabled(), ServiceChargeRateBp: in.GetServiceChargeRateBp(),
			ReceiptWidth: receiptWidthStr(in.GetReceiptWidth()), RoundTotalToRupee: in.GetRoundTotalToRupee(),
			DefaultLanguage: langStr(in.GetDefaultLanguage()), KdsYellowMinutes: in.GetKdsYellowMinutes(), KdsRedMinutes: in.GetKdsRedMinutes(),
			GuestOrdering: guest,
		})
		if errors.Is(e, pgx.ErrNoRows) {
			return connecterr.StaleVersion("settings changed; reload and retry")
		}
		if e != nil {
			return connecterr.Internal(e)
		}
		updated = u
		return audit.Write(ctx, q, audit.Entry{
			ActorID: actor.User.ID, Action: "settings.update", Entity: "settings", EntityID: "1",
			Before: audit.JSON(settingsAudit(before)), After: audit.JSON(settingsAudit(u)),
		})
	})
	if err != nil {
		return nil, err
	}
	s.emit(realtime.Event{Type: "settings.updated"})
	return toSettings(updated), nil
}

// settingsAudit drops the (data-URL) images so audit rows stay small.
func settingsAudit(st sqlc.Setting) sqlc.Setting {
	st.LogoUrl, st.QrFonepayUrl, st.QrOtherUrl = "", "", ""
	return st
}

// SetSettingsImage sets (or, with an empty image, clears) the logo or a payment
// QR. Same limits as item images. Does not bump the settings version.
func (s *Service) SetSettingsImage(ctx context.Context, kind restaurantv1.SettingsImageKind, image []byte, contentType string) (*restaurantv1.Settings, error) {
	actor, err := auth.RequireManager(ctx)
	if err != nil {
		return nil, err
	}
	var name string
	switch kind {
	case restaurantv1.SettingsImageKind_SETTINGS_IMAGE_KIND_LOGO:
		name = "logo"
	case restaurantv1.SettingsImageKind_SETTINGS_IMAGE_KIND_QR_FONEPAY:
		name = "qr_fonepay"
	case restaurantv1.SettingsImageKind_SETTINGS_IMAGE_KIND_QR_OTHER:
		name = "qr_other"
	default:
		return nil, connecterr.Invalid("kind", "unknown image kind")
	}
	url, verr := imageDataURL(image, contentType)
	if verr != nil {
		return nil, verr
	}
	var updated sqlc.Setting
	err = dbx.WithTx(ctx, s.pool, s.q, func(q *sqlc.Queries) error {
		set := q.SetSettingsQrOther
		switch kind {
		case restaurantv1.SettingsImageKind_SETTINGS_IMAGE_KIND_LOGO:
			set = q.SetSettingsLogo
		case restaurantv1.SettingsImageKind_SETTINGS_IMAGE_KIND_QR_FONEPAY:
			set = q.SetSettingsQrFonepay
		}
		u, e := set(ctx, url)
		if e != nil {
			return connecterr.Internal(e)
		}
		updated = u
		return audit.Write(ctx, q, audit.Entry{
			ActorID: actor.User.ID, Action: "settings.image", Entity: "settings", EntityID: "1",
			After: audit.JSON(map[string]any{"kind": name, "cleared": url == ""}),
		})
	})
	if err != nil {
		return nil, err
	}
	s.emit(realtime.Event{Type: "settings.updated"})
	return toSettings(updated), nil
}
