package alerts

import (
	"context"
	"testing"

	"go.uber.org/zap"

	"github.com/oneaccord/restro/api/internal/db/sqlc"
)

// Every audit row queues an audit.created poke (id = audit id), rule or not.
func TestAuditRowQueuesAuditCreated(t *testing.T) {
	ctx, take := WithEvents(context.Background())
	(&engine{log: zap.NewNop()}).onAudit(ctx, nil, sqlc.AuditLog{ID: 42, Action: "logout"})
	evs := take()
	if len(evs) != 1 || evs[0].Type != EventAuditCreated || evs[0].ID != "42" {
		t.Fatalf("events = %+v", evs)
	}
}
