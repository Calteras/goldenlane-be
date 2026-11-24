-- Online payment. Each merchant has its own gateway account (Xendit or Midtrans), so the money
-- goes straight to the merchant. The platform keeps a ledger of the fees it is owed.

CREATE TABLE merchant_gateways (
    id              uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    merchant_id     uuid        NOT NULL UNIQUE REFERENCES merchants (id),
    provider        text        NOT NULL CHECK (provider IN ('XENDIT', 'MIDTRANS')),
    -- Sealed JSON: the secret key, and for Xendit the callback verification token.
    credentials_enc text        NOT NULL,
    sandbox         boolean     NOT NULL DEFAULT false,
    active          boolean     NOT NULL DEFAULT true,
    verified_at     timestamptz,
    created_at      timestamptz NOT NULL DEFAULT now(),
    updated_at      timestamptz NOT NULL DEFAULT now()
);

-- An order paid online waits as UNPAID until the gateway confirms the payment, then goes to
-- the POS like any other. It expires if not paid by payment_deadline.
ALTER TABLE merchant_orders DROP CONSTRAINT merchant_orders_status_check;
ALTER TABLE merchant_orders ADD CONSTRAINT merchant_orders_status_check CHECK (status IN (
    'UNPAID', 'CREATED', 'SUBMIT_FAILED', 'PENDING_ACCEPTANCE', 'ACCEPTED', 'PREPARING', 'READY',
    'COMPLETED', 'REJECTED', 'CANCELLED', 'EXPIRED'));
ALTER TABLE merchant_orders DROP CONSTRAINT merchant_orders_payment_mode_check;
ALTER TABLE merchant_orders ADD CONSTRAINT merchant_orders_payment_mode_check
    CHECK (payment_mode IN ('PAY_AT_PICKUP', 'ONLINE'));
ALTER TABLE merchant_orders ADD COLUMN payment_deadline timestamptz;
CREATE INDEX merchant_orders_unpaid_idx ON merchant_orders (payment_deadline) WHERE status = 'UNPAID';

-- One row per attempt to pay an order. A failed attempt leaves the order UNPAID so the customer
-- can try again until the deadline.
CREATE TABLE payments (
    id                 uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    order_id           uuid        NOT NULL REFERENCES merchant_orders (id),
    gateway_id         uuid        NOT NULL REFERENCES merchant_gateways (id),
    provider           text        NOT NULL CHECK (provider IN ('XENDIT', 'MIDTRANS')),
    attempt            integer     NOT NULL,
    -- The id we gave the gateway (unique per attempt), and the gateway's own id when it has one.
    external_id        text        NOT NULL UNIQUE,
    provider_charge_id text,
    status             text        NOT NULL DEFAULT 'PENDING'
        CHECK (status IN ('PENDING', 'PAID', 'FAILED', 'EXPIRED', 'CANCELLED')),
    -- Money in minor units (1/100 rupiah). amount = what the customer pays = order total + customer fee.
    amount_minor       bigint      NOT NULL CHECK (amount_minor > 0),
    order_total_minor  bigint      NOT NULL,
    customer_fee_minor bigint      NOT NULL CHECK (customer_fee_minor >= 0),
    merchant_fee_minor bigint      NOT NULL CHECK (merchant_fee_minor >= 0),
    customer_fee_bps   integer     NOT NULL,
    merchant_fee_bps   integer     NOT NULL,
    pay_url            text,
    expires_at         timestamptz,
    paid_at            timestamptz,
    -- How it was paid, as the POS names it (QRIS, E_WALLET, BANK_TRANSFER, OTHER).
    method             text,
    error_code         text,
    error_message      text,
    checked_at         timestamptz NOT NULL DEFAULT now(),
    -- Refund of a paid order that will not be fulfilled.
    refund_status      text CHECK (refund_status IN ('PENDING', 'DONE', 'FAILED')),
    refund_ref         text,
    refund_attempts    integer     NOT NULL DEFAULT 0,
    refund_error       text,
    refund_available_at timestamptz NOT NULL DEFAULT now(),
    refund_lease_until timestamptz,
    refunded_at        timestamptz,
    created_at         timestamptz NOT NULL DEFAULT now(),
    updated_at         timestamptz NOT NULL DEFAULT now(),
    UNIQUE (order_id, attempt)
);
-- An order has at most one open attempt. Several can end up paid (a late payment on an old
-- attempt); only the first paid one counts and the others are refunded.
CREATE UNIQUE INDEX payments_one_pending_idx ON payments (order_id) WHERE status = 'PENDING';
CREATE INDEX payments_pending_idx ON payments (checked_at) WHERE status = 'PENDING';
CREATE INDEX payments_refund_idx ON payments (refund_available_at)
    WHERE status = 'PAID' AND (refund_status IS NULL OR refund_status = 'PENDING');
CREATE INDEX payments_order_idx ON payments (order_id, attempt DESC);

-- What each merchant owes the platform: both fees of every online order that was completed.
CREATE TABLE fee_ledger (
    id           uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    merchant_id  uuid        NOT NULL REFERENCES merchants (id),
    order_id     uuid        NOT NULL REFERENCES merchant_orders (id),
    payment_id   uuid        NOT NULL REFERENCES payments (id),
    kind         text        NOT NULL CHECK (kind IN ('CUSTOMER_FEE', 'MERCHANT_FEE')),
    amount_minor bigint      NOT NULL CHECK (amount_minor >= 0),
    created_at   timestamptz NOT NULL DEFAULT now(),
    UNIQUE (payment_id, kind)
);
CREATE INDEX fee_ledger_merchant_idx ON fee_ledger (merchant_id, created_at);
