package alerts

import (
	"context"
	"time"

	"go.uber.org/zap"

	"github.com/oneaccord/restro/api/internal/db/sqlc"
	"github.com/oneaccord/restro/api/internal/dbx"
)

// summaryGrace: the day summary goes out at closing time + 30 min (or when the
// last open shift closes after closing, whichever is later — see queueSummary).
const summaryGrace = 30 * time.Minute

// RunSummaryTimer is the in-process daily job (one deployment = one process): every
// interval it sends the summary push for the previous and the current business
// day once closing + 30 min has passed and no shift is open. On a restart it so
// catches up a missed summary for the previous day. Blocks until ctx is done.
func (s *Service) RunSummaryTimer(ctx context.Context, interval time.Duration) {
	t := time.NewTicker(interval)
	defer t.Stop()
	for {
		if err := s.SendDueSummaries(ctx, time.Now()); err != nil && ctx.Err() == nil {
			s.log.Error("alerts: day summary", zap.Error(err))
		}
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
	}
}

// SendDueSummaries does one timer tick at now.
func (s *Service) SendDueSummaries(ctx context.Context, now time.Time) error {
	return dbx.WithTx(ctx, s.pool, s.q, func(q *sqlc.Queries) error {
		cfg, err := loadConfig(ctx, q)
		if err != nil {
			return err
		}
		open, err := q.AlertCountOpenShifts(ctx)
		if err != nil || open > 0 {
			return err
		}
		today := businessDate(now, cfg.OpeningHours)
		for _, day := range []string{addDays(today, -1), today} {
			closeAt := businessDayClose(day, cfg.OpeningHours)
			if closeAt == nil || now.Before(closeAt.Add(summaryGrace)) {
				continue
			}
			if err := insertSummaryPush(ctx, q, cfg, day, now); err != nil {
				return err
			}
		}
		return nil
	})
}
