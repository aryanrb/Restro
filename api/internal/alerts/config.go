package alerts

import (
	"context"
	"encoding/json"
	"maps"
	"regexp"
	"slices"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgtype"

	"github.com/oneaccord/restro/api/internal/db/sqlc"
)

const (
	SeverityHigh   = "HIGH"
	SeverityNormal = "NORMAL"
)

type thresholdMeta struct {
	key   string
	value int64
}

// ruleMeta is a rule's shipped default (spec "Rules"); the owner overrides it.
type ruleMeta struct {
	rule       string
	severity   string
	thresholds []thresholdMeta
}

// Defaults, in screen order (web lib/alerts/rules.ts ALERT_RULES). Loyalty rules
// are added with their feature.
var ruleMetas = []ruleMeta{
	{"CASH_SHORT", SeverityHigh, []thresholdMeta{{"amount", 50_000}}},
	{"CANCEL_REBILL_LOWER", SeverityHigh, []thresholdMeta{{"windowMinutes", 120}}},
	{"SERVED_NOT_BILLED", SeverityHigh, nil},
	{"MANAGER_PIN_LOCKED", SeverityHigh, nil},
	{"AFTER_HOURS_LOGIN", SeverityHigh, nil},
	{"REPEATED_SHORTAGES", SeverityHigh, []thresholdMeta{{"count", 3}, {"minShort", 10_000}}},
	{"VOID_AFTER_KITCHEN", SeverityNormal, []thresholdMeta{{"minValue", 50_000}}},
	{"REPEATED_VOIDS", SeverityNormal, []thresholdMeta{{"count", 3}}},
	{"SELF_APPROVED", SeverityNormal, nil},
	{"HIGH_DISCOUNT", SeverityNormal, []thresholdMeta{{"percentBp", 2000}, {"amount", 100_000}}},
	{"DISCOUNT_TOTAL", SeverityNormal, []thresholdMeta{{"amount", 300_000}}},
	{"LATE_CANCELLATION", SeverityNormal, []thresholdMeta{{"minutes", 60}}},
	{"OLD_REPRINT", SeverityNormal, []thresholdMeta{{"ageMinutes", 60}, {"copy", 3}}},
	{"CASH_OVER", SeverityNormal, []thresholdMeta{{"amount", 100_000}}},
	{"PRICE_DIP", SeverityNormal, nil},
	{"CREDIT_OVER_LIMIT", SeverityNormal, nil},
	{"CREDIT_NEW_ACCOUNT", SeverityNormal, nil},
	{"LARGE_PAID_OUT", SeverityNormal, []thresholdMeta{{"amount", 500_000}, {"shiftTotal", 1_000_000}}},
	{"HIGH_WASTE", SeverityNormal, []thresholdMeta{{"dayWaste", 200_000}, {"countLoss", 300_000}}},
}

func metaOf(rule string) *ruleMeta {
	for i := range ruleMetas {
		if ruleMetas[i].rule == rule {
			return &ruleMetas[i]
		}
	}
	return nil
}

// RuleSetting is a rule's effective setting.
type RuleSetting struct {
	Rule       string
	Enabled    bool
	Severity   string
	Thresholds map[string]int64
}

// Config is everything the owner tunes (resolved: defaults + overrides).
type Config struct {
	Rules        []RuleSetting
	OpeningHours []*DayHours // nil = not entered (no after-hours alerts; business day = calendar day)
	Grace        int
	Quiet        *QuietHours
	PushCap      int
	Recipients   []uuid.UUID
}

func (c Config) rule(name string) *RuleSetting {
	for i := range c.Rules {
		if c.Rules[i].Rule == name {
			return &c.Rules[i]
		}
	}
	return nil
}

func (c Config) isRecipient(id uuid.UUID) bool { return slices.Contains(c.Recipients, id) }

// override is what is stored per rule: only differences from the defaults.
type override struct {
	Enabled    *bool            `json:"enabled,omitempty"`
	Severity   string           `json:"severity,omitempty"`
	Thresholds map[string]int64 `json:"thresholds,omitempty"`
}

func resolveRule(m ruleMeta, o override) RuleSetting {
	r := RuleSetting{Rule: m.rule, Enabled: true, Severity: m.severity, Thresholds: map[string]int64{}}
	if o.Enabled != nil {
		r.Enabled = *o.Enabled
	}
	if o.Severity != "" {
		r.Severity = o.Severity
	}
	for _, t := range m.thresholds {
		r.Thresholds[t.key] = t.value
		if v, ok := o.Thresholds[t.key]; ok {
			r.Thresholds[t.key] = v
		}
	}
	return r
}

// toOverrides keeps only what differs from the defaults. Rules not listed keep
// their current setting (from cur).
func toOverrides(rules []RuleSetting, cur Config) map[string]override {
	out := map[string]override{}
	for _, m := range ruleMetas {
		r := cur.rule(m.rule)
		for i := range rules {
			if rules[i].Rule == m.rule {
				r = &rules[i]
			}
		}
		if r == nil {
			continue
		}
		var o override
		if !r.Enabled {
			f := false
			o.Enabled = &f
		}
		if r.Severity != m.severity {
			o.Severity = r.Severity
		}
		for _, t := range m.thresholds {
			if v, ok := r.Thresholds[t.key]; ok && v != t.value {
				if o.Thresholds == nil {
					o.Thresholds = map[string]int64{}
				}
				o.Thresholds[t.key] = v
			}
		}
		if o.Enabled != nil || o.Severity != "" || o.Thresholds != nil {
			out[m.rule] = o
		}
	}
	return out
}

func configFromRow(row sqlc.AlertConfig) Config {
	var overrides map[string]override
	_ = json.Unmarshal(row.Overrides, &overrides)
	c := Config{Grace: int(row.AfterHoursGraceMinutes), PushCap: int(row.DailyPushCap), Recipients: row.RecipientIds}
	for _, m := range ruleMetas {
		c.Rules = append(c.Rules, resolveRule(m, overrides[m.rule]))
	}
	if len(row.OpeningHours) > 0 && string(row.OpeningHours) != "null" {
		var hours []*DayHours
		if json.Unmarshal(row.OpeningHours, &hours) == nil && len(hours) == 7 {
			c.OpeningHours = hours
		}
	}
	if row.QuietFrom.Valid && row.QuietTo.Valid {
		c.Quiet = &QuietHours{From: row.QuietFrom.String, To: row.QuietTo.String}
	}
	return c
}

func loadConfig(ctx context.Context, q *sqlc.Queries) (Config, error) {
	row, err := q.GetAlertConfig(ctx)
	if err != nil {
		return Config{}, err
	}
	return configFromRow(row), nil
}

func saveParams(c Config, cur Config) sqlc.UpdateAlertConfigParams {
	overrides, _ := json.Marshal(toOverrides(c.Rules, cur))
	p := sqlc.UpdateAlertConfigParams{
		Overrides: overrides, AfterHoursGraceMinutes: int32(c.Grace), DailyPushCap: int32(c.PushCap),
		RecipientIds: uniqueIDs(c.Recipients),
	}
	if c.OpeningHours != nil {
		p.OpeningHours, _ = json.Marshal(c.OpeningHours)
	}
	if c.Quiet != nil {
		p.QuietFrom = pgtype.Text{String: c.Quiet.From, Valid: true}
		p.QuietTo = pgtype.Text{String: c.Quiet.To, Valid: true}
	}
	return p
}

var hhmm = regexp.MustCompile(`^([01]\d|2[0-3]):[0-5]\d$`)

// validate returns the offending field, "" when valid. managers: active managers' ids.
func validate(c Config, managers []uuid.UUID) string {
	for _, r := range c.Rules {
		if metaOf(r.Rule) == nil || (r.Severity != SeverityHigh && r.Severity != SeverityNormal) {
			return "rules"
		}
		for _, v := range r.Thresholds {
			if v < 0 {
				return "rules"
			}
		}
	}
	if c.OpeningHours != nil {
		if len(c.OpeningHours) != 7 {
			return "opening_hours"
		}
		for _, h := range c.OpeningHours {
			if h != nil && (!hhmm.MatchString(h.Open) || !hhmm.MatchString(h.Close) || h.Open == h.Close) {
				return "opening_hours"
			}
		}
	}
	if c.Grace < 0 || c.Grace > 240 {
		return "after_hours_grace_minutes"
	}
	if c.Quiet != nil && (!hhmm.MatchString(c.Quiet.From) || !hhmm.MatchString(c.Quiet.To)) {
		return "quiet_hours"
	}
	if c.PushCap < 0 || c.PushCap > 50 {
		return "daily_push_cap"
	}
	for _, id := range c.Recipients {
		if !slices.Contains(managers, id) {
			return "recipient_ids"
		}
	}
	return ""
}

func uniqueIDs(ids []uuid.UUID) []uuid.UUID {
	out := make([]uuid.UUID, 0, len(ids))
	for _, id := range ids {
		if id != uuid.Nil && !slices.Contains(out, id) {
			out = append(out, id)
		}
	}
	return out
}

// changes describes what an update changed, for the audit rows.
func changes(before, after Config) (rules []string, hoursChanged, notifyChanged bool) {
	for i, r := range after.Rules {
		b := before.Rules[i]
		if r.Enabled != b.Enabled || r.Severity != b.Severity || !maps.Equal(r.Thresholds, b.Thresholds) {
			s := r.Rule
			if !r.Enabled {
				s += " off"
			}
			rules = append(rules, s)
		}
	}
	bh, _ := json.Marshal(before.OpeningHours)
	ah, _ := json.Marshal(after.OpeningHours)
	hoursChanged = string(bh) != string(ah) || before.Grace != after.Grace
	sortIDs := func(ids []uuid.UUID) []string {
		s := make([]string, 0, len(ids))
		for _, id := range ids {
			s = append(s, id.String())
		}
		slices.Sort(s)
		return s
	}
	bq, _ := json.Marshal(before.Quiet)
	aq, _ := json.Marshal(after.Quiet)
	notifyChanged = string(bq) != string(aq) || before.PushCap != after.PushCap ||
		!slices.Equal(sortIDs(before.Recipients), sortIDs(after.Recipients))
	return rules, hoursChanged, notifyChanged
}
