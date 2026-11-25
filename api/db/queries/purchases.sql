-- Suppliers --------------------------------------------------------------------

-- name: CreateSupplier :one
INSERT INTO suppliers (name, phone, address, pan, vat_registered, contact_person, notes, opening_balance_paisa, created_by_user_id)
VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9)
RETURNING *;

-- name: UpdateSupplier :one
UPDATE suppliers SET name = $2, phone = $3, address = $4, pan = $5, vat_registered = $6,
    contact_person = $7, notes = $8, updated_at = now()
WHERE id = $1
RETURNING *;

-- name: SetSupplierActive :one
UPDATE suppliers SET active = $2, updated_at = now() WHERE id = $1 RETURNING *;

-- name: GetSupplier :one
SELECT * FROM suppliers WHERE id = $1;

-- Serialises balance checks (payments, bills, returns) per supplier.
-- name: LockSupplier :one
SELECT * FROM suppliers WHERE id = $1 FOR UPDATE;

-- name: SupplierNameTaken :one
SELECT EXISTS (SELECT 1 FROM suppliers WHERE lower(name) = lower(@name::text) AND id <> @self_id::uuid);

-- name: GetSupplierWithBalance :one
SELECT s.*, b.balance_paisa
FROM suppliers s JOIN supplier_balances b ON b.supplier_id = s.id
WHERE s.id = $1;

-- name: ListSuppliersWithBalance :many
SELECT s.*, b.balance_paisa
FROM suppliers s JOIN supplier_balances b ON b.supplier_id = s.id
ORDER BY lower(s.name), s.id;

-- name: SupplierBalance :one
SELECT balance_paisa FROM supplier_balances WHERE supplier_id = $1;

-- Live payments less refunds handed back: the pool that settles debts oldest first.
-- name: SupplierPaidNet :one
SELECT (COALESCE((SELECT sum(sp.amount_paisa) FROM supplier_payments sp WHERE sp.supplier_id = @supplier_id AND sp.voided_at IS NULL), 0)
      - COALESCE((SELECT sum(r.refund_paisa) FROM purchase_returns r JOIN purchases p ON p.id = r.purchase_id
                  WHERE p.supplier_id = @supplier_id), 0))::bigint;

-- Counters -----------------------------------------------------------------------

-- name: AllocatePurchaseCounter :one
UPDATE purchase_counters SET next_number = next_number + 1 WHERE kind = $1 RETURNING (next_number - 1)::int;

-- Purchases ----------------------------------------------------------------------

-- name: SupplierBillTaken :one
SELECT EXISTS (SELECT 1 FROM purchases WHERE supplier_id = $1 AND status = 'RECORDED'
    AND supplier_bill_no <> '' AND lower(supplier_bill_no) = lower(@bill_no::text));

-- name: InsertPurchase :one
INSERT INTO purchases (number, supplier_id, supplier_name, supplier_pan, supplier_bill_no, bill_date, bill_date_bs, fiscal_year,
    is_vat_bill, discount_paisa, taxable_paisa, exempt_paisa, vat_paisa, total_paisa, recorded_by_user_id, at)
VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12, $13, $14, $15, $16)
RETURNING *;

-- name: InsertPurchaseLine :one
INSERT INTO purchase_lines (purchase_id, sort, inventory_item_id, name_en, name_ne, stock_unit, quantity_milli, unit_key,
    unit_label_en, unit_label_ne, factor_milli, unit_cost_paisa, vat_applies, amount_paisa, discount_paisa, vat_paisa, stock_unit_cost_e4)
VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12, $13, $14, $15, $16, $17)
RETURNING *;

-- name: GetPurchaseRow :one
SELECT p.*, COALESCE(u.name, '')::text AS recorded_by_name
FROM purchases p LEFT JOIN users u ON u.id = p.recorded_by_user_id
WHERE p.id = $1;

-- name: LockPurchase :one
SELECT * FROM purchases WHERE id = $1 FOR UPDATE;

-- name: VoidPurchase :exec
UPDATE purchases SET status = 'VOIDED', void_reason = $2, voided_by_user_id = $3, voided_at = now()
WHERE id = $1 AND status = 'RECORDED';

-- Bill date window (AD, inclusive), newest first; NULL filters = any.
-- name: ListPurchaseRows :many
SELECT p.*, COALESCE(u.name, '')::text AS recorded_by_name
FROM purchases p LEFT JOIN users u ON u.id = p.recorded_by_user_id
WHERE p.bill_date >= @from_date::date AND p.bill_date <= @to_date::date
  AND (@include_voided::boolean OR p.status = 'RECORDED')
  AND (sqlc.narg('supplier_id')::uuid IS NULL OR p.supplier_id = sqlc.narg('supplier_id')::uuid)
  AND (sqlc.narg('inventory_item_id')::uuid IS NULL OR EXISTS (
      SELECT 1 FROM purchase_lines l WHERE l.purchase_id = p.id AND l.inventory_item_id = sqlc.narg('inventory_item_id')::uuid))
ORDER BY p.bill_date DESC, p.at DESC, p.number DESC;

-- name: ListPurchaseLinesFor :many
SELECT * FROM purchase_lines WHERE purchase_id = ANY(@ids::uuid[]) ORDER BY purchase_id, sort;

-- name: ListPurchaseReturnsFor :many
SELECT r.*, COALESCE(u.name, '')::text AS by_name
FROM purchase_returns r LEFT JOIN users u ON u.id = r.by_user_id
WHERE r.purchase_id = ANY(@ids::uuid[])
ORDER BY r.at, r.id;

-- name: ListPurchaseReturnLinesFor :many
SELECT rl.* FROM purchase_return_lines rl JOIN purchase_returns r ON r.id = rl.return_id
WHERE r.purchase_id = ANY(@ids::uuid[]);

-- name: InsertPurchaseReturn :one
INSERT INTO purchase_returns (purchase_id, note_no, reason, taxable_paisa, exempt_paisa, vat_paisa, total_paisa,
    refund_paisa, refund_shift_id, by_user_id, at, bs_date)
VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12)
RETURNING *;

-- name: InsertPurchaseReturnLine :exec
INSERT INTO purchase_return_lines (return_id, purchase_line_id, quantity_milli, net_paisa, vat_paisa)
VALUES ($1, $2, $3, $4, $5);

-- name: AddPurchaseLineReturned :exec
UPDATE purchase_lines SET returned_milli = returned_milli + @qty WHERE id = @id;

-- name: CountPurchaseReturns :one
SELECT count(*)::int FROM purchase_returns WHERE purchase_id = $1;

-- What the supplier is owed for: recorded bills net of their returns, oldest first.
-- name: ListSupplierDebts :many
SELECT p.id, p.bill_date, p.at,
       (p.total_paisa - COALESCE((SELECT sum(r.total_paisa) FROM purchase_returns r WHERE r.purchase_id = p.id), 0))::bigint AS amount_paisa
FROM purchases p
WHERE p.supplier_id = $1 AND p.status = 'RECORDED'
ORDER BY p.bill_date, p.at, p.number;

-- Statement events ---------------------------------------------------------------

-- name: ListSupplierStatementBills :many
SELECT id, number, supplier_bill_no, bill_date, at, total_paisa
FROM purchases WHERE supplier_id = $1 AND status = 'RECORDED';

-- name: ListSupplierStatementReturns :many
SELECT r.id, r.note_no, r.reason, r.total_paisa, r.refund_paisa, r.at, p.number
FROM purchase_returns r JOIN purchases p ON p.id = r.purchase_id
WHERE p.supplier_id = $1 AND p.status = 'RECORDED';

-- Supplier payments ----------------------------------------------------------------

-- name: InsertSupplierPayment :one
INSERT INTO supplier_payments (number, supplier_id, supplier_name, amount_paisa, method, reference, note, shift_id,
    paid_by_user_id, approver_user_id, bs_date, balance_after_paisa)
VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12)
RETURNING *;

-- name: GetSupplierPaymentRow :one
SELECT sp.*, COALESCE(pu.name, '')::text AS paid_by_name, COALESCE(au.name, '')::text AS approver_name
FROM supplier_payments sp
LEFT JOIN users pu ON pu.id = sp.paid_by_user_id
LEFT JOIN users au ON au.id = sp.approver_user_id
WHERE sp.id = $1;

-- name: LockSupplierPayment :one
SELECT * FROM supplier_payments WHERE id = $1 FOR UPDATE;

-- name: VoidSupplierPayment :exec
UPDATE supplier_payments SET voided_at = now(), voided_by_user_id = $2, void_reason = $3, void_shift_id = $4
WHERE id = $1 AND voided_at IS NULL;

-- name: ListSupplierPaymentRows :many
SELECT sp.*, COALESCE(pu.name, '')::text AS paid_by_name, COALESCE(au.name, '')::text AS approver_name
FROM supplier_payments sp
LEFT JOIN users pu ON pu.id = sp.paid_by_user_id
LEFT JOIN users au ON au.id = sp.approver_user_id
WHERE sp.supplier_id = $1
ORDER BY sp.at DESC, sp.number DESC;

-- name: LastSupplierPaymentAt :one
SELECT at FROM supplier_payments WHERE supplier_id = $1 AND voided_at IS NULL ORDER BY at DESC LIMIT 1;

-- Purchase settings / buy list ---------------------------------------------------

-- name: ListItemPurchaseSettings :many
SELECT * FROM item_purchase_settings ORDER BY inventory_item_id;

-- name: UpsertItemPurchaseSettings :one
INSERT INTO item_purchase_settings (inventory_item_id, preferred_supplier_id, default_unit_key, buy_up_to_milli, updated_at)
VALUES ($1, $2, $3, $4, now())
ON CONFLICT (inventory_item_id) DO UPDATE SET preferred_supplier_id = EXCLUDED.preferred_supplier_id,
    default_unit_key = EXCLUDED.default_unit_key, buy_up_to_milli = EXCLUDED.buy_up_to_milli, updated_at = now()
RETURNING *;

-- The last price paid per item (newest recorded bill line).
-- name: ListLastPurchasePrices :many
SELECT DISTINCT ON (l.inventory_item_id) l.inventory_item_id, l.unit_cost_paisa, l.unit_label_en, l.unit_label_ne
FROM purchase_lines l JOIN purchases p ON p.id = l.purchase_id
WHERE p.status = 'RECORDED' AND l.inventory_item_id IS NOT NULL
ORDER BY l.inventory_item_id, p.at DESC, p.number DESC, l.sort;

-- The purchase posting moves the item's moving-average unit cost.
-- name: SetInventoryUnitCost :exec
UPDATE inventory_items SET unit_cost_e4 = $2, version = version + 1, updated_at = now() WHERE id = $1;

-- Reports -----------------------------------------------------------------------

-- name: ListVatPurchasesInRange :many
SELECT * FROM purchases
WHERE is_vat_bill AND status = 'RECORDED' AND bill_date >= @from_date::date AND bill_date <= @to_date::date
ORDER BY bill_date, at, number;

-- Returns on recorded VAT bills, by return date (Kathmandu).
-- name: ListVatReturnsInRange :many
SELECT r.*, p.supplier_bill_no, p.supplier_name, p.supplier_pan
FROM purchase_returns r JOIN purchases p ON p.id = r.purchase_id
WHERE p.is_vat_bill AND p.status = 'RECORDED'
  AND (r.at AT TIME ZONE 'Asia/Kathmandu')::date >= @from_date::date
  AND (r.at AT TIME ZONE 'Asia/Kathmandu')::date <= @to_date::date
ORDER BY r.at, r.id;

-- name: RecordedPurchasesTotalBetween :one
SELECT COALESCE(sum(total_paisa), 0)::bigint FROM purchases
WHERE status = 'RECORDED' AND at >= @from_ts AND at < @to_ts;

-- name: TillPaidBetween :one
SELECT COALESCE(sum(amount_paisa), 0)::bigint FROM supplier_payments
WHERE method = 'TILL' AND voided_at IS NULL AND at >= @from_ts AND at < @to_ts;

-- name: OwedToSuppliers :one
SELECT COALESCE(sum(GREATEST(balance_paisa, 0)), 0)::bigint FROM supplier_balances;

-- Alerts (LARGE_PAID_OUT) ----------------------------------------------------------

-- One person's till payments counted in a shift (voided while the shift was open = never paid out).
-- name: AlertTillPaidOutsInShift :one
SELECT count(*)::int AS payments, COALESCE(sum(amount_paisa), 0)::bigint AS total
FROM supplier_payments
WHERE method = 'TILL' AND shift_id = @shift_id::uuid AND paid_by_user_id = @paid_by_user_id
  AND NOT (voided_at IS NOT NULL AND void_shift_id IS NULL);
