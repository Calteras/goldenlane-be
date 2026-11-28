-- Customer trust: structured order outcomes and an append-only log of trust events.
-- docs/CUSTOMER_TRUST_AND_SAFETY_PLAN.md, sprint 1.

ALTER TABLE merchant_orders
    -- When the platform learned the order was ready (its own clock, so a vendor cannot back-date it).
    ADD COLUMN ready_at           timestamptz,
    ADD COLUMN no_show_at         timestamptz,
    -- Who ended it, and why, as a code. Set on every close.
    ADD COLUMN cancelled_by       text CHECK (cancelled_by IN ('CUSTOMER', 'VENDOR', 'SYSTEM')),
    ADD COLUMN close_reason_code  text;

-- Facts about how a customer behaved, never edited. A correction is a new event.
CREATE TABLE customer_trust_events (
    id          bigserial PRIMARY KEY,
    customer_id uuid        NOT NULL REFERENCES customers (id),
    event_type  text        NOT NULL,
    -- Who says so: the platform itself, the vendor, the payment gateway, or an operator.
    source      text        NOT NULL CHECK (source IN ('SYSTEM', 'VENDOR', 'GATEWAY', 'OPERATOR', 'BACKFILL')),
    order_id    uuid        REFERENCES merchant_orders (id),
    outlet_id   uuid        REFERENCES outlets (id),
    merchant_id uuid        REFERENCES merchants (id),
    -- Timestamps, durations and codes that back the event up.
    evidence    jsonb       NOT NULL DEFAULT '{}'::jsonb,
    created_at  timestamptz NOT NULL DEFAULT now()
);
CREATE INDEX customer_trust_events_customer_idx ON customer_trust_events (customer_id, created_at DESC);
-- One event of a kind per order: a repeated delivery of the same POS event adds nothing.
CREATE UNIQUE INDEX customer_trust_events_order_kind_idx ON customer_trust_events (order_id, event_type)
    WHERE order_id IS NOT NULL;

-- Orders that ended before this change: only what is certain is carried over, at the lowest weight.
INSERT INTO customer_trust_events (customer_id, event_type, source, order_id, outlet_id, merchant_id, evidence, created_at)
SELECT customer_id, 'ORDER_COMPLETED', 'BACKFILL', id, outlet_id, merchant_id, '{}'::jsonb, COALESCE(completed_at, updated_at)
FROM merchant_orders WHERE status = 'COMPLETED'
ON CONFLICT DO NOTHING;

INSERT INTO customer_trust_events (customer_id, event_type, source, order_id, outlet_id, merchant_id, evidence, created_at)
SELECT customer_id, 'PAYMENT_EXPIRED', 'BACKFILL', id, outlet_id, merchant_id, '{}'::jsonb, updated_at
FROM merchant_orders WHERE status = 'EXPIRED' AND error_code = 'PAYMENT_EXPIRED'
ON CONFLICT DO NOTHING;
