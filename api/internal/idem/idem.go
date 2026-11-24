// Package idem provides command idempotency: a client command UUID is recorded in
// processed_commands with its serialized response, so retries (from an offline
// queue or a double-submit) are no-ops that return the original result.
package idem

import (
	"context"
	"errors"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"google.golang.org/protobuf/proto"

	"github.com/oneaccord/restro/api/internal/connecterr"
	"github.com/oneaccord/restro/api/internal/db/sqlc"
	"github.com/oneaccord/restro/api/internal/dbx"
)

var errReplay = errors.New("idem: concurrent duplicate, replaying")

// Run records commandID so a retry is a no-op. fn populates out and writes the DB;
// both commit together. A concurrent duplicate rolls back and replays the winner's
// stored response.
func Run(ctx context.Context, pool *pgxpool.Pool, base *sqlc.Queries, commandID, userID uuid.UUID, out proto.Message, fn func(q *sqlc.Queries) error) error {
	if stored, err := base.GetProcessedCommand(ctx, commandID); err == nil {
		return proto.Unmarshal(stored, out)
	} else if !errors.Is(err, pgx.ErrNoRows) {
		return connecterr.Internal(err)
	}

	conflict := false
	err := dbx.WithTx(ctx, pool, base, func(q *sqlc.Queries) error {
		if e := fn(q); e != nil {
			return e
		}
		b, e := proto.Marshal(out)
		if e != nil {
			return connecterr.Internal(e)
		}
		_, e = q.InsertProcessedCommand(ctx, sqlc.InsertProcessedCommandParams{CommandID: commandID, UserID: dbx.UUID(userID), Response: b})
		if errors.Is(e, pgx.ErrNoRows) {
			conflict = true
			return errReplay
		}
		if e != nil {
			return connecterr.Internal(e)
		}
		return nil
	})
	if conflict {
		stored, e := base.GetProcessedCommand(ctx, commandID)
		if e != nil {
			return connecterr.Internal(e)
		}
		return proto.Unmarshal(stored, out)
	}
	return err
}
