DROP TABLE IF EXISTS fee_ledger;
DROP TABLE IF EXISTS payments;
DROP INDEX IF EXISTS merchant_orders_unpaid_idx;
ALTER TABLE merchant_orders DROP COLUMN IF EXISTS payment_deadline;
ALTER TABLE merchant_orders DROP CONSTRAINT merchant_orders_payment_mode_check;
ALTER TABLE merchant_orders ADD CONSTRAINT merchant_orders_payment_mode_check CHECK (payment_mode IN ('PAY_AT_PICKUP'));
ALTER TABLE merchant_orders DROP CONSTRAINT merchant_orders_status_check;
ALTER TABLE merchant_orders ADD CONSTRAINT merchant_orders_status_check CHECK (status IN (
    'CREATED', 'SUBMIT_FAILED', 'PENDING_ACCEPTANCE', 'ACCEPTED', 'PREPARING', 'READY',
    'COMPLETED', 'REJECTED', 'CANCELLED', 'EXPIRED'));
DROP TABLE IF EXISTS merchant_gateways;
