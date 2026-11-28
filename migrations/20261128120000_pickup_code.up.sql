-- Pickup code: four digits the customer shows so the vendor finds the order fast, and proof of pickup.
-- docs/CUSTOMER_TRUST_AND_SAFETY_PLAN.md, sprint 2B.
ALTER TABLE merchant_orders
    ADD COLUMN pickup_code          text CHECK (pickup_code ~ '^[0-9]{4}$'),
    ADD COLUMN pickup_confirmed_at  timestamptz,
    -- CODE: handed over against the customer's code. MANUAL: completed without it.
    ADD COLUMN pickup_confirmed_via text CHECK (pickup_confirmed_via IN ('CODE', 'MANUAL'));

-- A code belongs to one open order of an outlet at a time, and is free again once it closes.
CREATE UNIQUE INDEX merchant_orders_open_code_idx ON merchant_orders (outlet_id, pickup_code)
    WHERE pickup_code IS NOT NULL
      AND status IN ('UNPAID', 'CREATED', 'PENDING_ACCEPTANCE', 'ACCEPTED', 'PREPARING', 'READY');
