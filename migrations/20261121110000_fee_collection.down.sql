-- Fails if pay-at-pickup fees exist: they cannot be represented without a payment.
DROP INDEX IF EXISTS fee_ledger_ref_idx;
DROP INDEX IF EXISTS fee_ledger_open_idx;
DROP INDEX IF EXISTS fee_ledger_order_kind_idx;
ALTER TABLE fee_ledger
    DROP COLUMN IF EXISTS settlement_note,
    DROP COLUMN IF EXISTS settlement_ref,
    DROP COLUMN IF EXISTS settled_at,
    DROP COLUMN IF EXISTS reserved_at,
    DROP COLUMN IF EXISTS billing_ref,
    DROP COLUMN IF EXISTS source;
ALTER TABLE fee_ledger ALTER COLUMN payment_id SET NOT NULL;
ALTER TABLE fee_ledger ADD CONSTRAINT fee_ledger_payment_id_kind_key UNIQUE (payment_id, kind);
ALTER TABLE merchant_orders DROP COLUMN IF EXISTS merchant_fee_bps;
