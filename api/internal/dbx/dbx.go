// Package dbx holds small helpers shared by every domain: pgtype conversions and
// a transaction wrapper, so services don't repeat Begin/Rollback/Commit plumbing.
package dbx

import (
	"context"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/oneaccord/restro/api/internal/connecterr"
	"github.com/oneaccord/restro/api/internal/db/sqlc"
)

func Text(s string) pgtype.Text { return pgtype.Text{String: s, Valid: true} }

// TextOpt is NULL when s is empty.
func TextOpt(s string) pgtype.Text {
	if s == "" {
		return pgtype.Text{}
	}
	return Text(s)
}

func UUID(id uuid.UUID) pgtype.UUID { return pgtype.UUID{Bytes: id, Valid: true} }

// UUIDOpt is NULL when id is the zero UUID.
func UUIDOpt(id uuid.UUID) pgtype.UUID {
	if id == uuid.Nil {
		return pgtype.UUID{}
	}
	return UUID(id)
}

// WithTx runs fn inside a transaction, committing on success. fn's error (often a
// *connecterr.Error) is returned as-is; infra failures become connecterr.Internal.
func WithTx(ctx context.Context, pool *pgxpool.Pool, base *sqlc.Queries, fn func(q *sqlc.Queries) error) error {
	tx, err := pool.Begin(ctx)
	if err != nil {
		return connecterr.Internal(err)
	}
	defer tx.Rollback(ctx)
	if err := fn(base.WithTx(tx)); err != nil {
		return err
	}
	if err := tx.Commit(ctx); err != nil {
		return connecterr.Internal(err)
	}
	return nil
}
