-- Suspicious activity alerts (internal/alerts). Rules run inside the transaction
-- that wrote the audit row, so every query here takes that transaction's Queries.

-- name: InsertAuditReturning :one
-- audit.Write's insert: returns the row so the alert rules can run on it.
INSERT INTO audit_log (actor_user_id, action, entity, entity_id, before, after, reason, approver_user_id, device_label)
VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9)
RETURNING *;

-- name: AlertSavepoint :exec
SAVEPOINT alert_rule;

-- name: AlertReleaseSavepoint :exec
RELEASE SAVEPOINT alert_rule;

-- name: AlertRollbackSavepoint :exec
ROLLBACK TO SAVEPOINT alert_rule;

-- --- config ---

-- name: GetAlertConfig :one
SELECT * FROM alert_config WHERE id = 1;

-- name: UpdateAlertConfig :one
UPDATE alert_config SET overrides = @overrides, opening_hours = @opening_hours,
    after_hours_grace_minutes = @after_hours_grace_minutes, quiet_from = @quiet_from, quiet_to = @quiet_to,
    daily_push_cap = @daily_push_cap, recipient_ids = @recipient_ids::uuid[], updated_at = now()
WHERE id = 1
RETURNING *;

-- --- alerts ---

-- name: AlertDedupExists :one
SELECT EXISTS (SELECT 1 FROM alerts WHERE rule = @rule AND dedup_key = @dedup_key::text)::bool;

-- name: UpsertAlert :one
-- A new alert, or a repeat firing grouped into the open alert of the same rule +
-- group (count + 1, params replaced, subject/approvers/involved/audit ids added).
-- count = 1 in the result means it was created.
INSERT INTO alerts (rule, severity, subject_user_id, approver_ids, involved_ids, subject, num_params, text_params,
                    business_date, first_at, last_at, shift_id, invoice_id, audit_ids, group_key, dedup_key)
VALUES (@rule, @severity, sqlc.narg('subject_user_id')::uuid, @approver_ids::uuid[], @involved_ids::uuid[], @subject,
        @num_params, @text_params, @business_date, @at, @at, sqlc.narg('shift_id')::uuid, sqlc.narg('invoice_id')::uuid,
        ARRAY[@audit_id::bigint], @group_key, sqlc.narg('dedup_key')::text)
ON CONFLICT (rule, group_key) WHERE status = 'OPEN' DO UPDATE SET
    count = alerts.count + 1,
    last_at = EXCLUDED.last_at,
    num_params = EXCLUDED.num_params,
    text_params = EXCLUDED.text_params,
    subject = CASE WHEN (', ' || alerts.subject || ', ') LIKE ('%, ' || EXCLUDED.subject || ', %') THEN alerts.subject
                   ELSE alerts.subject || ', ' || EXCLUDED.subject END,
    approver_ids = ARRAY(SELECT DISTINCT x FROM unnest(alerts.approver_ids || EXCLUDED.approver_ids) AS x),
    involved_ids = ARRAY(SELECT DISTINCT x FROM unnest(alerts.involved_ids || EXCLUDED.involved_ids) AS x),
    audit_ids = alerts.audit_ids || EXCLUDED.audit_ids,
    version = alerts.version + 1
RETURNING *;

-- name: ListAlerts :many
-- hide_for: a manager recipient (never sees alerts they are involved in); null = owner.
SELECT * FROM alerts
WHERE (sqlc.narg('hide_for')::uuid IS NULL OR NOT (sqlc.narg('hide_for')::uuid = ANY (involved_ids)))
  AND (@status::text = '' OR status = @status::text)
  AND (@rule::text = '' OR rule = @rule::text)
  AND (sqlc.narg('subject_user_id')::uuid IS NULL OR subject_user_id = sqlc.narg('subject_user_id')::uuid)
  AND (sqlc.narg('from_date')::date IS NULL OR business_date >= sqlc.narg('from_date')::date)
  AND (sqlc.narg('to_date')::date IS NULL OR business_date <= sqlc.narg('to_date')::date)
ORDER BY last_at DESC, seq DESC
LIMIT 500;

-- name: GetAlert :one
SELECT * FROM alerts WHERE id = $1;

-- name: LockAlerts :many
SELECT * FROM alerts WHERE id = ANY (@ids::uuid[]) ORDER BY id FOR UPDATE;

-- name: ResolveAlert :one
UPDATE alerts SET status = @status, note = @note, resolved_by_user_id = @resolved_by, resolved_at = now(), version = version + 1
WHERE id = @id
RETURNING *;

-- name: AlertCounts :one
SELECT count(*)::int AS open, count(*) FILTER (WHERE severity = 'HIGH')::int AS high
FROM alerts
WHERE status = 'OPEN'
  AND (sqlc.narg('hide_for')::uuid IS NULL OR NOT (sqlc.narg('hide_for')::uuid = ANY (involved_ids)));

-- name: DayAlertsByRule :many
SELECT rule, bool_or(severity = 'HIGH')::bool AS any_high, count(*)::int AS total,
       count(*) FILTER (WHERE status = 'OPEN')::int AS open,
       count(*) FILTER (WHERE status = 'OPEN' AND severity = 'HIGH')::int AS open_high
FROM alerts
WHERE business_date = @business_date
  AND (sqlc.narg('hide_for')::uuid IS NULL OR NOT (sqlc.narg('hide_for')::uuid = ANY (involved_ids)))
GROUP BY rule;

-- name: AlertUserNames :many
SELECT id, name FROM users WHERE id = ANY (@ids::uuid[]);

-- name: AlertAuditEntries :many
SELECT a.id, a.actor_user_id, a.action, a.entity, a.entity_id, a.after, a.reason,
       a.approver_user_id, a.device_label, a.created_at,
       COALESCE(actor.name, '')::text AS actor_name,
       COALESCE(approver.name, '')::text AS approver_name,
       COALESCE(
         CASE a.entity
           WHEN 'invoice' THEN (SELECT i.display_number FROM invoices i WHERE i.id::text = a.entity_id)
           WHEN 'order_item' THEN (SELECT oi.name_en || CASE WHEN oi.variant_name_en <> '' THEN ' (' || oi.variant_name_en || ')' ELSE '' END
                                   FROM order_items oi WHERE oi.id::text = a.entity_id)
           WHEN 'user' THEN (SELECT u.name FROM users u WHERE u.id::text = a.entity_id)
           WHEN 'shift' THEN (SELECT u.name FROM shifts s JOIN users u ON u.id = s.cashier_user_id WHERE s.id::text = a.entity_id)
           WHEN 'menu_item' THEN (SELECT m.name_en FROM menu_items m WHERE m.id::text = a.entity_id)
           WHEN 'customer' THEN (SELECT cu.name FROM customers cu WHERE cu.id::text = a.entity_id)
           WHEN 'waste_entry' THEN (SELECT w.name_en FROM waste_entries w WHERE w.id::text = a.entity_id)
           WHEN 'stock_count' THEN (SELECT 'Count #' || c.number FROM stock_counts c WHERE c.id::text = a.entity_id)
         END, '')::text AS subject
FROM audit_log a
LEFT JOIN users actor ON actor.id = a.actor_user_id
LEFT JOIN users approver ON approver.id = a.approver_user_id
WHERE a.id = ANY (@ids::bigint[])
ORDER BY a.id;

-- --- pushes ---

-- name: CountDayAlertPushes :one
SELECT count(*)::int FROM alert_pushes WHERE business_date = @business_date AND kind = @kind;

-- name: InsertAlertPush :exec
-- MORE and SUMMARY are once per business day (unique index): a repeat is a no-op.
INSERT INTO alert_pushes (at, business_date, kind, rule, amount_paisa, count, alert_id, exclude_ids)
VALUES (@at, @business_date, @kind, @rule, sqlc.narg('amount_paisa')::bigint, sqlc.narg('count')::int,
        sqlc.narg('alert_id')::uuid, @exclude_ids::uuid[])
ON CONFLICT DO NOTHING;

-- name: AlertPushExists :one
SELECT EXISTS (SELECT 1 FROM alert_pushes WHERE business_date = @business_date AND kind = @kind)::bool;

-- name: ListAlertPushes :many
-- The latest 30 for a viewer (null = owner: everything), oldest first.
SELECT * FROM (
    SELECT * FROM alert_pushes
    WHERE sqlc.narg('viewer')::uuid IS NULL OR NOT (sqlc.narg('viewer')::uuid = ANY (exclude_ids))
    ORDER BY at DESC, id DESC
    LIMIT 30
) p ORDER BY p.at, p.id;

-- --- rule look-backs ---

-- name: AlertOpenShiftOf :one
SELECT * FROM shifts WHERE cashier_user_id = $1 AND status = 'OPEN' ORDER BY opened_at DESC LIMIT 1;

-- name: AlertCountOpenShifts :one
SELECT count(*)::int FROM shifts WHERE status = 'OPEN';

-- name: AlertCountSentVoidsSince :one
-- One person's voids of items that had been sent to the kitchen, since a time.
SELECT count(*)::int FROM audit_log a
JOIN order_items oi ON oi.id::text = a.entity_id
WHERE a.action = 'order.void' AND a.actor_user_id = @actor_id AND a.created_at >= @since AND oi.sent_at IS NOT NULL;

-- name: AlertDiscountTotal :one
SELECT COALESCE(sum(discount_paisa), 0)::bigint FROM invoices
WHERE status = 'ISSUED' AND created_by_user_id = @user_id AND ad_datetime >= @from_ts AND ad_datetime < @to_ts;

-- name: AlertRebillSource :one
-- The latest bill of the same order, cancelled since @since, that had some of the
-- new bill's items and a higher total.
SELECT c.* FROM invoices c
WHERE c.status = 'CANCELLED' AND c.order_id = @order_id AND c.cancelled_at >= @since AND c.total_paisa > @total
  AND EXISTS (SELECT 1 FROM invoice_lines cl JOIN invoice_lines nl ON nl.order_item_id = cl.order_item_id
              WHERE cl.invoice_id = c.id AND nl.invoice_id = @new_invoice_id)
ORDER BY c.cancelled_at DESC
LIMIT 1;

-- name: AlertClosedShiftsSince :many
SELECT * FROM shifts WHERE cashier_user_id = $1 AND status = 'CLOSED' AND closed_at >= @since ORDER BY closed_at;

-- name: AlertServedUnbilled :many
-- Open orders with SERVED items on no issued bill: place, waiter, item count and value.
SELECT o.id, o.waiter_user_id, COALESCE(t.name, NULLIF(o.label, ''), 'Takeaway')::text AS place,
       count(oi.id)::int AS items, COALESCE(sum(oi.unit_price_paisa * oi.quantity), 0)::bigint AS value
FROM orders o
JOIN order_items oi ON oi.order_id = o.id AND oi.status = 'SERVED' AND oi.invoice_id IS NULL
LEFT JOIN dining_tables t ON t.id = o.table_id
WHERE o.status = 'OPEN'
GROUP BY o.id, t.name
ORDER BY o.created_at;

-- name: AlertEntityAudit :many
-- The audit rows of one entity and action up to (and including) @up_to_id, oldest first.
SELECT id, actor_user_id, after, created_at FROM audit_log
WHERE action = @action AND entity_id = @entity_id::text AND id <= @up_to_id
ORDER BY id;

-- name: AlertSoldBetween :one
SELECT COALESCE(sum(quantity), 0)::int FROM order_items
WHERE menu_item_id = @menu_item_id AND status <> 'VOIDED' AND sent_at >= @from_ts AND sent_at <= @to_ts;

-- name: AlertCustomer :one
SELECT id, name, created_by_user_id, created_at FROM customers WHERE id = $1;

-- name: AlertWasteTotal :one
SELECT COALESCE(sum(value_paisa), 0)::bigint FROM waste_entries WHERE at >= @from_ts AND at < @to_ts;

-- name: AlertCountLoss :one
SELECT COALESCE(sum(-LEAST(value_paisa, 0)), 0)::bigint FROM stock_count_lines WHERE count_id = $1;

-- --- day summary ---

-- name: AlertDaySales :one
SELECT count(*)::int AS bills, COALESCE(sum(total_paisa), 0)::bigint AS sales, COALESCE(sum(discount_paisa), 0)::bigint AS discounts
FROM invoices WHERE status = 'ISSUED' AND ad_datetime >= @from_ts AND ad_datetime < @to_ts;

-- name: AlertDayByMethod :many
SELECT p.method, COALESCE(sum(p.amount_paisa), 0)::bigint AS total
FROM payments p JOIN invoices i ON i.id = p.invoice_id
WHERE i.status = 'ISSUED' AND i.ad_datetime >= @from_ts AND i.ad_datetime < @to_ts
  AND p.status = 'CONFIRMED' AND p.method <> 'ON_ACCOUNT'
GROUP BY p.method ORDER BY p.method;

-- name: AlertDayVoids :one
SELECT count(*)::int AS count, COALESCE(sum(unit_price_paisa * quantity), 0)::bigint AS value
FROM order_items WHERE status = 'VOIDED' AND sent_at >= @from_ts AND sent_at < @to_ts;

-- name: AlertDayCancelled :one
SELECT count(*)::int AS count, COALESCE(sum(total_paisa), 0)::bigint AS value
FROM invoices WHERE status = 'CANCELLED' AND cancelled_at >= @from_ts AND cancelled_at < @to_ts;

-- name: AlertDayShifts :many
SELECT s.*, u.name AS user_name FROM shifts s JOIN users u ON u.id = s.cashier_user_id
WHERE s.opened_at >= @from_ts AND s.opened_at < @to_ts
ORDER BY s.opened_at, s.id;
