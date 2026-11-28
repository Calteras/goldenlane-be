-- Customer trust: the computed profile and the tunable rules (shadow mode first).
-- docs/CUSTOMER_TRUST_AND_SAFETY_PLAN.md, sprint 3.

-- Where each customer stands. A cache of what the events say, rebuilt at any time.
CREATE TABLE customer_trust_profile (
    customer_id      uuid PRIMARY KEY REFERENCES customers (id),
    score            numeric(8, 2) NOT NULL DEFAULT 0,
    tier             text          NOT NULL DEFAULT 'NEW' CHECK (tier IN ('NEW', 'GOOD', 'WATCH', 'RESTRICTED', 'SUSPENDED')),
    finished_orders  integer       NOT NULL DEFAULT 0,
    completed_orders integer       NOT NULL DEFAULT 0,
    -- The readable reasons behind the score: [{code, count, points}].
    reasons          jsonb         NOT NULL DEFAULT '[]'::jsonb,
    computed_at      timestamptz   NOT NULL DEFAULT now()
);
CREATE INDEX customer_trust_profile_tier_idx ON customer_trust_profile (tier);

-- Weights and thresholds as JSON, so they change without a deploy. An empty table means the defaults.
CREATE TABLE trust_config (
    key        text PRIMARY KEY,
    value      jsonb       NOT NULL,
    updated_at timestamptz NOT NULL DEFAULT now()
);
