-- 008: shift close-out fixes.
-- * closing_note: CloseShift no longer overwrites the opening note.
-- * shift_adjustments: a bill cancelled after the shift that took its payment had
--   closed is counted back (subtracted) in the canceller's shift that was open at
--   the time of the cancel. One row per payment, so recording is idempotent.

ALTER TABLE shifts ADD COLUMN closing_note text NOT NULL DEFAULT '';

CREATE TABLE shift_adjustments (
    id                uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    shift_id          uuid NOT NULL REFERENCES shifts (id),
    original_shift_id uuid NOT NULL REFERENCES shifts (id),
    invoice_id        uuid NOT NULL REFERENCES invoices (id),
    payment_id        uuid NOT NULL UNIQUE REFERENCES payments (id),
    method            text NOT NULL,
    amount_paisa      bigint NOT NULL,
    created_by_user_id uuid REFERENCES users (id),
    created_at        timestamptz NOT NULL
);
CREATE INDEX shift_adjustments_shift_idx ON shift_adjustments (shift_id);

---- create above / drop below ----

DROP TABLE IF EXISTS shift_adjustments;
ALTER TABLE shifts DROP COLUMN IF EXISTS closing_note;
