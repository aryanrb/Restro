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
	var updated sqlc.Setting
	err = dbx.WithTx(ctx, s.pool, s.q, func(q *sqlc.Queries) error {
		before, _ := q.GetSettings(ctx)
		u, e := q.UpdateSettings(ctx, sqlc.UpdateSettingsParams{
			Version: version,
			NameEn:  in.GetName().GetEn(), NameNe: in.GetName().GetNe(),
			Address: in.GetAddress(), Phone: in.GetPhone(), PanNumber: in.GetPanNumber(),
			VatRegistered: in.GetVatRegistered(), PricesIncludeVat: in.GetPricesIncludeVat(), VatRateBp: in.GetVatRateBp(),
			ServiceChargeEnabled: in.GetServiceChargeEnabled(), ServiceChargeRateBp: in.GetServiceChargeRateBp(),
			ReceiptWidth: receiptWidthStr(in.GetReceiptWidth()), RoundTotalToRupee: in.GetRoundTotalToRupee(),
			DefaultLanguage: langStr(in.GetDefaultLanguage()), KdsYellowMinutes: in.GetKdsYellowMinutes(), KdsRedMinutes: in.GetKdsRedMinutes(),
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
			Before: audit.JSON(before), After: audit.JSON(u),
		})
	})
	if err != nil {
		return nil, err
	}
	s.emit(realtime.Event{Type: "settings.updated"})
	return toSettings(updated), nil
}
