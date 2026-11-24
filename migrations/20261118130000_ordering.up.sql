-- Ordering without online payment: checkouts, the orders inside them, a durable queue of
-- push notifications, and reviews.

-- A checkout is what a customer submits once. Today it holds one merchant order; the shape
-- allows several (one per merchant) without a migration.
CREATE TABLE checkouts (
    id               uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    customer_id      uuid        NOT NULL REFERENCES customers (id),
    idempotency_key  text        NOT NULL,
    request_hash     text        NOT NULL,
    created_at       timestamptz NOT NULL DEFAULT now(),
    UNIQUE (customer_id, idempotency_key)
);
CREATE INDEX checkouts_customer_created_idx ON checkouts (customer_id, created_at DESC);

CREATE TABLE merchant_orders (
    id                uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    checkout_id       uuid        NOT NULL REFERENCES checkouts (id),
    customer_id       uuid        NOT NULL REFERENCES customers (id),
    merchant_id       uuid        NOT NULL REFERENCES merchants (id),
    outlet_id         uuid        NOT NULL REFERENCES outlets (id),
    -- The id the POS knows this order by: unique per link, and the POS idempotency key.
    platform_order_id text        NOT NULL UNIQUE,
    pos_order_id      text,
    status            text        NOT NULL DEFAULT 'CREATED' CHECK (status IN (
        'CREATED', 'SUBMIT_FAILED', 'PENDING_ACCEPTANCE', 'ACCEPTED', 'PREPARING', 'READY',
        'COMPLETED', 'REJECTED', 'CANCELLED', 'EXPIRED')),
    -- Version of the POS's order: a status is applied only if it is newer than this.
    pos_version       integer     NOT NULL DEFAULT 0,
    fulfillment_type  text        NOT NULL DEFAULT 'PICKUP' CHECK (fulfillment_type IN ('PICKUP', 'DINE_IN')),
    fulfillment_label text,
    notes             text,
    payment_mode      text        NOT NULL DEFAULT 'PAY_AT_PICKUP' CHECK (payment_mode IN ('PAY_AT_PICKUP')),
    -- Money in minor units (1/100 rupiah), as the POS priced it. Null until it has.
    subtotal_minor    bigint,
    tax_minor         bigint,
    service_minor     bigint,
    total_minor       bigint,
    -- What the customer expected to pay when ordering; the POS refuses if prices moved.
    expected_total_minor bigint,
    accept_by         timestamptz,
    prep_time_minutes integer,
    reason            text,
    error_code        text,
    error_detail      jsonb,
    -- Sending the order to the POS (leased worker, like the event inbox).
    submit_attempts   integer     NOT NULL DEFAULT 0,
    -- New orders wait 10 s so the request that created them can try first.
    available_at      timestamptz NOT NULL DEFAULT now() + interval '10 seconds',
    lease_until       timestamptz,
    lease_token       uuid,
    -- When the POS was last asked about this order, for the safety-net refresh.
    checked_at        timestamptz NOT NULL DEFAULT now(),
    created_at        timestamptz NOT NULL DEFAULT now(),
    updated_at        timestamptz NOT NULL DEFAULT now(),
    completed_at      timestamptz
);
CREATE INDEX merchant_orders_customer_idx ON merchant_orders (customer_id, created_at DESC);
CREATE INDEX merchant_orders_submit_idx ON merchant_orders (available_at) WHERE status = 'CREATED';
CREATE INDEX merchant_orders_open_idx ON merchant_orders (checked_at)
    WHERE status IN ('PENDING_ACCEPTANCE', 'ACCEPTED', 'PREPARING', 'READY');

CREATE TABLE merchant_order_items (
    order_id     uuid    NOT NULL REFERENCES merchant_orders (id) ON DELETE CASCADE,
    line_no      integer NOT NULL,
    product_id   text    NOT NULL,
    variant_id   text,
    modifier_ids jsonb   NOT NULL DEFAULT '[]',
    quantity     integer NOT NULL CHECK (quantity BETWEEN 1 AND 50),
    notes        text,
    -- What the customer saw when ordering.
    name         text    NOT NULL,
    variant_name text,
    PRIMARY KEY (order_id, line_no)
);

-- Push notifications to send. One per (order, status): a repeated event never pushes twice.
CREATE TABLE notification_outbox (
    id           uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    customer_id  uuid        NOT NULL REFERENCES customers (id),
    order_id     uuid        NOT NULL REFERENCES merchant_orders (id) ON DELETE CASCADE,
    kind         text        NOT NULL,
    title        text        NOT NULL,
    body         text        NOT NULL,
    data         jsonb       NOT NULL DEFAULT '{}',
    status       text        NOT NULL DEFAULT 'PENDING' CHECK (status IN ('PENDING', 'PROCESSING', 'SENT', 'FAILED')),
    attempts     integer     NOT NULL DEFAULT 0,
    available_at timestamptz NOT NULL DEFAULT now(),
    lease_until  timestamptz,
    lease_token  uuid,
    last_error   text,
    created_at   timestamptz NOT NULL DEFAULT now(),
    sent_at      timestamptz,
    UNIQUE (order_id, kind)
);
CREATE INDEX notification_outbox_due_idx ON notification_outbox (status, available_at);

CREATE TABLE reviews (
    id          uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    order_id    uuid        NOT NULL UNIQUE REFERENCES merchant_orders (id),
    customer_id uuid        NOT NULL REFERENCES customers (id),
    outlet_id   uuid        NOT NULL REFERENCES outlets (id),
    rating      smallint    NOT NULL CHECK (rating BETWEEN 1 AND 5),
    comment     text        CHECK (comment IS NULL OR char_length(comment) <= 500),
    created_at  timestamptz NOT NULL DEFAULT now()
);
CREATE INDEX reviews_outlet_idx ON reviews (outlet_id, created_at DESC);
