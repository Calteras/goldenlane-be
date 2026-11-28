DROP INDEX IF EXISTS merchant_orders_open_code_idx;
ALTER TABLE merchant_orders
    DROP COLUMN IF EXISTS pickup_confirmed_via,
    DROP COLUMN IF EXISTS pickup_confirmed_at,
    DROP COLUMN IF EXISTS pickup_code;
