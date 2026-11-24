-- Collecting the platform's fees. They are added to the merchant's monthly subscription
-- charge: the POS asks the platform to reserve what is owed, charges it with the renewal, and
-- reports the payment, which settles the reserved entries.

-- Pay-at-pickup orders owe the merchant fee too. The rate is fixed when the order is placed.
ALTER TABLE merchant_orders ADD COLUMN merchant_fee_bps integer NOT NULL DEFAULT 0;

-- Fees of pay-at-pickup orders have no payment behind them.
ALTER TABLE fee_ledger ALTER COLUMN payment_id DROP NOT NULL;
ALTER TABLE fee_ledger
    ADD COLUMN source          text NOT NULL DEFAULT 'ONLINE' CHECK (source IN ('ONLINE', 'PICKUP')),
    -- The billing period that reserved this entry; released if not settled in time.
    ADD COLUMN billing_ref     text,
    ADD COLUMN reserved_at     timestamptz,
    ADD COLUMN settled_at      timestamptz,
    ADD COLUMN settlement_ref  text,
    ADD COLUMN settlement_note text;
ALTER TABLE fee_ledger DROP CONSTRAINT fee_ledger_payment_id_kind_key;
CREATE UNIQUE INDEX fee_ledger_order_kind_idx ON fee_ledger (order_id, kind);
CREATE INDEX fee_ledger_open_idx ON fee_ledger (merchant_id) WHERE settled_at IS NULL;
CREATE INDEX fee_ledger_ref_idx ON fee_ledger (merchant_id, billing_ref) WHERE billing_ref IS NOT NULL;
