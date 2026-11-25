// Package hook is the single way an audit_log row is inserted. After the insert it
// runs the registered OnWrite function (the alert rules) with the same Queries, so
// whatever the hook writes commits or rolls back with the audited change.
//
// It depends only on sqlc so that every package that audits — including auth,
// which package audit itself depends on — can use it.
package hook

import (
	"context"
	"sync/atomic"

	"github.com/oneaccord/restro/api/internal/db/sqlc"
)

// Func runs after an audit row was written, inside the writer's transaction (or on
// the pool when the writer has none). It must never fail the write: it handles and
// logs its own errors.
type Func func(ctx context.Context, q *sqlc.Queries, row sqlc.AuditLog)

var current atomic.Pointer[Func]

// Set registers f (nil clears it) and returns a func restoring the previous one.
func Set(f Func) (restore func()) {
	var p *Func
	if f != nil {
		p = &f
	}
	prev := current.Swap(p)
	return func() { current.Store(prev) }
}

// Insert writes the audit row with q and then runs the registered hook.
func Insert(ctx context.Context, q *sqlc.Queries, p sqlc.InsertAuditParams) error {
	row, err := q.InsertAuditReturning(ctx, sqlc.InsertAuditReturningParams(p))
	if err != nil {
		return err
	}
	if f := current.Load(); f != nil {
		(*f)(ctx, q, row)
	}
	return nil
}
