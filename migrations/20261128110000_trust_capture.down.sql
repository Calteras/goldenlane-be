DROP TABLE IF EXISTS customer_trust_events;
ALTER TABLE merchant_orders
    DROP COLUMN IF EXISTS close_reason_code,
    DROP COLUMN IF EXISTS cancelled_by,
    DROP COLUMN IF EXISTS no_show_at,
    DROP COLUMN IF EXISTS ready_at;
