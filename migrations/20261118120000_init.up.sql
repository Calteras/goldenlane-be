-- StreetFood customer API: customers, merchants linked to a POS, the outlet and menu
-- read model, and the inbox of POS events. The read model is rebuildable from the POS.
CREATE EXTENSION IF NOT EXISTS cube;
CREATE EXTENSION IF NOT EXISTS earthdistance;

-- ---------------------------------------------------------------- customers
CREATE TABLE customers (
    id          uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    phone       text,
    name        text        NOT NULL DEFAULT '',
    avatar_url  text,
    status      text        NOT NULL DEFAULT 'active' CHECK (status IN ('active', 'suspended', 'deleted')),
    created_at  timestamptz NOT NULL DEFAULT now(),
    updated_at  timestamptz NOT NULL DEFAULT now(),
    deleted_at  timestamptz
);

-- The sign-in provider's user id. Only Firebase today; the table lets the provider change.
CREATE TABLE external_identities (
    provider          text        NOT NULL CHECK (provider IN ('firebase')),
    provider_user_id  text        NOT NULL,
    customer_id       uuid        NOT NULL REFERENCES customers (id) ON DELETE CASCADE,
    created_at        timestamptz NOT NULL DEFAULT now(),
    PRIMARY KEY (provider, provider_user_id)
);
CREATE INDEX external_identities_customer_idx ON external_identities (customer_id);

CREATE TABLE devices (
    id           uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    customer_id  uuid        NOT NULL REFERENCES customers (id) ON DELETE CASCADE,
    platform     text        NOT NULL CHECK (platform IN ('ios', 'android', 'web')),
    fcm_token    text        NOT NULL UNIQUE,
    last_seen_at timestamptz NOT NULL DEFAULT now(),
    created_at   timestamptz NOT NULL DEFAULT now()
);
CREATE INDEX devices_customer_idx ON devices (customer_id);

-- ---------------------------------------------------------------- merchants
CREATE TABLE merchants (
    id            uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    pos_tenant_id text        NOT NULL UNIQUE,
    name          text        NOT NULL,
    status        text        NOT NULL DEFAULT 'active' CHECK (status IN ('active', 'suspended')),
    created_at    timestamptz NOT NULL DEFAULT now(),
    updated_at    timestamptz NOT NULL DEFAULT now()
);

-- How to reach and verify one merchant's POS. The secret is encrypted at rest.
CREATE TABLE merchant_links (
    merchant_id  uuid PRIMARY KEY REFERENCES merchants (id) ON DELETE CASCADE,
    key_id       text        NOT NULL UNIQUE,
    secret_enc   text        NOT NULL,
    pos_base_url text        NOT NULL,
    status       text        NOT NULL DEFAULT 'active' CHECK (status IN ('active', 'revoked')),
    created_at   timestamptz NOT NULL DEFAULT now(),
    updated_at   timestamptz NOT NULL DEFAULT now()
);

-- ------------------------------------------------------------------ outlets
CREATE TABLE outlets (
    id                uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    merchant_id       uuid        NOT NULL REFERENCES merchants (id) ON DELETE CASCADE,
    pos_outlet_id     text        NOT NULL,
    name              text        NOT NULL,
    address           text,
    phone             text,
    base_lat          double precision CHECK (base_lat BETWEEN -90 AND 90),
    base_lng          double precision CHECK (base_lng BETWEEN -180 AND 180),
    location_mode     text        NOT NULL DEFAULT 'FIXED' CHECK (location_mode IN ('FIXED', 'MOBILE')),
    location_sharing  text        NOT NULL DEFAULT 'LIVE' CHECK (location_sharing IN ('LIVE', 'SPOT', 'HIDDEN')),
    timezone          text        NOT NULL DEFAULT 'Asia/Jakarta',
    online_status     text        NOT NULL DEFAULT 'AUTO' CHECK (online_status IN ('AUTO', 'OPEN', 'CLOSED', 'PAUSED')),
    prep_time_minutes integer     NOT NULL DEFAULT 15,
    pricing_valid     boolean     NOT NULL DEFAULT true,
    active            boolean     NOT NULL DEFAULT true,
    published         boolean     NOT NULL DEFAULT false,
    -- Ordering of POS events: apply only what is newer than what we hold.
    state_version     integer     NOT NULL DEFAULT 0,
    catalog_version   integer     NOT NULL DEFAULT 0,
    published_event_at timestamptz,
    -- Today's working day of a mobile vendor or kiosk.
    session_active    boolean     NOT NULL DEFAULT false,
    session_id        text,
    session_started_at timestamptz,
    session_sharing   text        CHECK (session_sharing IN ('LIVE', 'SPOT', 'HIDDEN')),
    session_spot      jsonb,
    session_event_at  timestamptz,
    -- Latest position reported by the POS (mobile vendors).
    cur_lat           double precision,
    cur_lng           double precision,
    cur_accuracy_m    double precision,
    cur_heading_deg   double precision,
    cur_moving        boolean     NOT NULL DEFAULT false,
    cur_recorded_at   timestamptz,
    cur_cleared_at    timestamptz,
    -- Where customers find the outlet right now, and how we know (derived on every change).
    pos_lat           double precision,
    pos_lng           double precision,
    pos_mode          text        CHECK (pos_mode IN ('FIXED', 'LIVE', 'SPOT')),
    created_at        timestamptz NOT NULL DEFAULT now(),
    updated_at        timestamptz NOT NULL DEFAULT now(),
    UNIQUE (merchant_id, pos_outlet_id)
);

-- Radius search: only outlets that are listed and have a position.
CREATE INDEX outlets_position_gist ON outlets USING gist (ll_to_earth(pos_lat, pos_lng))
    WHERE pos_lat IS NOT NULL AND published AND active;

CREATE TABLE outlet_hours (
    outlet_id    uuid     NOT NULL REFERENCES outlets (id) ON DELETE CASCADE,
    day_of_week  smallint NOT NULL CHECK (day_of_week BETWEEN 0 AND 6),
    opens_at     text     NOT NULL CHECK (opens_at ~ '^([01][0-9]|2[0-3]):[0-5][0-9]$'),
    closes_at    text     NOT NULL CHECK (closes_at ~ '^([01][0-9]|2[0-3]):[0-5][0-9]$'),
    PRIMARY KEY (outlet_id, day_of_week, opens_at)
);

-- ------------------------------------------------------------------- menus
-- The full snapshot as the POS served it, plus a flat table for search.
CREATE TABLE outlet_menus (
    outlet_id       uuid PRIMARY KEY REFERENCES outlets (id) ON DELETE CASCADE,
    catalog_version integer     NOT NULL,
    payload         jsonb       NOT NULL,
    fetched_at      timestamptz NOT NULL DEFAULT now()
);

CREATE TABLE menu_items (
    outlet_id      uuid    NOT NULL REFERENCES outlets (id) ON DELETE CASCADE,
    pos_product_id text    NOT NULL,
    name           text    NOT NULL,
    category       text    NOT NULL DEFAULT '',
    price_minor    bigint  NOT NULL,
    sold_out       boolean NOT NULL DEFAULT false,
    PRIMARY KEY (outlet_id, pos_product_id)
);

-- ------------------------------------------------------------- POS events
-- Events are stored first and applied after, so the POS gets its 2xx quickly and a crash
-- never loses an event. (key_id, event_id) makes redelivery harmless.
CREATE TABLE integration_inbox (
    id           uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    key_id       text        NOT NULL,
    event_id     text        NOT NULL,
    type         text        NOT NULL,
    outlet_id    text,
    version      integer     NOT NULL DEFAULT 0,
    occurred_at  timestamptz NOT NULL,
    payload      jsonb       NOT NULL,
    status       text        NOT NULL DEFAULT 'PENDING' CHECK (status IN ('PENDING', 'PROCESSING', 'DONE', 'FAILED')),
    attempts     integer     NOT NULL DEFAULT 0,
    available_at timestamptz NOT NULL DEFAULT now(),
    lease_until  timestamptz,
    lease_token  uuid,
    last_error   text,
    received_at  timestamptz NOT NULL DEFAULT now(),
    processed_at timestamptz,
    UNIQUE (key_id, event_id)
);
CREATE INDEX integration_inbox_due_idx ON integration_inbox (status, available_at);
