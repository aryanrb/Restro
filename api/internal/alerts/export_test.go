package alerts

import (
	"github.com/google/uuid"

	"github.com/oneaccord/restro/api/internal/db/sqlc"
)

// AddFailingRule registers a rule on trigger whose write fails in SQL (a CHECK
// violation), which would abort the caller's transaction without the savepoint.
// The returned func removes it.
func AddFailingRule(trigger string) func() {
	registry = append(registry, ruleDef{rule: "CASH_OVER", triggers: []string{trigger}, eval: func(c *ruleCtx) ([]firing, error) {
		_, err := c.q.UpsertAlert(c.ctx, sqlc.UpsertAlertParams{Rule: "BROKEN", Severity: "BOGUS", ApproverIds: []uuid.UUID{}, InvolvedIds: []uuid.UUID{},
			NumParams: []byte("{}"), TextParams: []byte("{}"), BusinessDate: dateOf(c.day), At: c.at, AuditID: c.row.ID, GroupKey: "x"})
		return nil, err
	}})
	n := len(registry)
	return func() { registry = registry[:n-1] }
}
