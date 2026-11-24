# StreetFood customer API

The backend for the customer app of StreetFood OS. Customers find street food vendors and kiosks
near them, see what they sell, order, pay, and watch their order progress. Merchants keep using
their POS (`ansilum-be`). This service holds a copy of what the POS publishes and serves it to
customers; the POS stays the source of truth.

It is a Go service (Fiber, pgx, golang-migrate) built from the infrastructure of `goldenlane-be`,
with none of its HR code.

**How the two backends and the vendor tracking talk to each other is explained in
[`docs/HOW_IT_WORKS.md`](docs/HOW_IT_WORKS.md).** Read it after the setup below.

## Contents

1. [What exists](#what-exists)
2. [The two systems in one picture](#the-two-systems-in-one-picture)
3. [Development setup](#development-setup)
4. [Run the whole loop: POS, API and a customer](#run-the-whole-loop-pos-api-and-a-customer)
5. [Configuration reference](#configuration-reference)
6. [Tests](#tests)
7. [Layout](#layout)
8. [Troubleshooting](#troubleshooting)
9. [Deploying to staging and production (Neon)](#deploying-to-staging-and-production-neon)
10. [Operating notes](#operating-notes)

## What exists

| Area | Status |
|---|---|
| Signed link to each merchant's POS (HMAC both ways, byte-compatible with the POS) | done |
| POS events: store first, apply in order, safe to repeat, healed by reconciliation | done |
| Outlets, hours, working days and live positions of mobile vendors | done |
| Menus (as the POS publishes them), search by outlet, merchant or dish | done |
| `GET /v1/outlets/nearby`, `/v1/outlets/{id}`, `/v1/outlets/{id}/menu` | done |
| Customer accounts from Firebase sign-in: `/v1/me`, `/v1/devices` | done |
| Operator API to register merchants: `/v1/admin/merchants` | done |
| Ordering at pickup: `/v1/checkouts`, `/v1/orders`, cancel, status mirrored from the POS | done |
| Order status push (FCM) and live order stream (SSE) | done |
| Reviews and outlet ratings: `/v1/orders/{id}/review`, `/v1/outlets/{id}/reviews` | done |
| Online payment with the merchant's own Xendit or Midtrans account: pay, retry, expire, refund | done (stub-tested; not run against real gateways) |
| The platform's fee (2.5% from the merchant on every order, 2.5% more from the customer on online orders) recorded per completed order | done |
| Fees collected with the merchant's monthly subscription charge (the POS adds them and reports the payment) | done; stub-tested, not run with real Midtrans |

Plans and the contract with the POS live in `../ansilum-be/docs/streetfood-os/`
(`CUSTOMER_API_PLAN.md`, `INTEGRATION_CONTRACT.md`). The API for apps is `api/openapi.yaml`.

## The two systems in one picture

```
                        signed HTTP, both ways (HMAC, one key per merchant)

  vendor phone ──GPS──▶ ┌──────────────┐  events (positions, menus, orders…)  ┌──────────────────┐ ◀── customer app
  cashier tablet ─────▶ │  POS         │ ───────────────────────────────────▶ │  This API (Go)   │      (Firebase
                        │  ansilum-be  │ ◀─────────────────────────────────── │                  │       sign-in)
                        │  NestJS      │  orders, price quotes, state reads   │  PostgreSQL      │
                        │  PostgreSQL  │ ─── fees reserve / report ─────────▶ │  (read model)    │ ◀── Xendit / Midtrans
                        └──────────────┘                                      └──────────────────┘     (merchant's own account)
```

- The **POS** owns outlets, hours, prices, stock, the vendor's working day, GPS trails and the
  merchant's orders and sales.
- This **API** owns customer accounts, the customers' orders and payments, the push queue, reviews
  and the platform's fee ledger. Everything it knows about outlets is a copy that the POS can
  rebuild at any time.
- The **tracking engine** is the part of the POS that turns a vendor's phone GPS into a position
  customers can trust (validation, throttling, sharing choices), and the part of this API that turns
  those positions into search results. The vendor's phone talks only to the POS.

## Development setup

### Prerequisites

| Tool | Why |
|---|---|
| Go 1.26 or newer | build and run |
| PostgreSQL 13 or newer with the `cube` and `earthdistance` extensions (they ship with PostgreSQL; the first migration creates them, so the database user needs permission to create extensions) | storage and nearby search |
| Docker | the shared PostgreSQL container, and optionally the API in a container |
| `openssl`, `curl` | generating secrets, trying the API |
| Python 3 with `pyjwt` and `cryptography` (optional) | the local sign-in helper, if you have no Firebase project |
| The POS repo `../ansilum-be` with Node 24 (optional) | to run the full loop |

### 1. A database

Development uses the shared PostgreSQL container `local-dev-postgres-1` that `ansilum-be` also uses
(PostgreSQL 15, host port 5433, Docker network `local-dev`, role `developer`). Start it from the
shared local infrastructure first, then create this project's database once:

```sh
docker exec local-dev-postgres-1 psql -U developer -d postgres -c 'CREATE DATABASE streetfood'
```

`developer` is a superuser there, so the first migration can create the `cube` and `earthdistance`
extensions. The role's password is in `../ansilum-be/secrets/development/`. Without that container,
any PostgreSQL 13+ with an empty `streetfood` database works.

### 2. Configure

```sh
cp .env.example .env
```

Fill the values the service refuses to start without. Everything else has a development default.

| Setting | How to get it |
|---|---|
| `PG_URL` | `postgres://developer:<password>@localhost:5433/streetfood?sslmode=disable` (for `make run`, from your machine) |
| `DOCKER_PG_URL` | the same with host `postgres:5432` instead of `localhost:5433` (for `docker compose up`, from the `local-dev` network) |
| `ADMIN_API_TOKEN` | `openssl rand -hex 32` (at least 32 characters; guards `/v1/admin`) |
| `ENCRYPTION_KEY` | `openssl rand -base64 32` (32 bytes; encrypts merchants' POS secrets and gateway keys at rest) |
| `FIREBASE_PROJECT_ID` | your Firebase project, or any name when you use the local sign-in helper (step 4) |

For development also set `PUBLIC_BASE_URL=http://localhost:8080`. It only appears in the callback
address the operator API shows merchants for their payment gateway.

The file is ignored by git. Any `NAME` can instead be given as `NAME_FILE=/path/to/secret`.

### 3. Migrate and run

```sh
make run                  # applies the migrations, then serves on :8080
curl localhost:8080/healthz          # 200
```

`make migrate` only migrates, `make help` lists the other targets. The API never migrates itself.
Startup downloads Firebase's token-signing keys once, so it needs network access to
`FIREBASE_JWKS_URL` (or the local helper below).

To run the migrations and the API in containers instead (on the shared database):

```sh
docker compose up --build          # API on 127.0.0.1:18080 (APP_PORT=… to change)
```

### 4. Sign in as a customer without Firebase

Customers sign in with the Firebase SDK in the app, and the API only verifies the ID token. For
development, `scripts/dev-jwks.py` stands in for Firebase: it serves a signing key and writes two
ready-made customer tokens (`rina`, `budi`) into `.dev-tokens/`.

```sh
pip install pyjwt cryptography
FIREBASE_PROJECT_ID=dev python3 scripts/dev-jwks.py &        # keys on http://127.0.0.1:55996
```

Put these in `.env` and restart the API:

```
FIREBASE_PROJECT_ID=dev
FIREBASE_JWKS_URL=http://127.0.0.1:55996
```

```sh
curl localhost:8080/v1/me -H "Authorization: Bearer $(cat .dev-tokens/rina)"
```

The first valid token creates the account. Tokens last 24 hours. Never run the helper anywhere but
your own machine: anyone who can reach it can sign in as anyone.

With a real Firebase project, leave `FIREBASE_JWKS_URL` unset and use tokens from the Firebase SDK.

### 5. The operator API

```sh
export ADMIN=$(grep ^ADMIN_API_TOKEN .env | cut -d= -f2)
curl localhost:8080/v1/admin/merchants -H "Authorization: Bearer $ADMIN"      # {"merchants":[]}
```

## Run the whole loop: POS, API and a customer

This is what you do once to see a vendor appear on the customers' map and an order travel to the
cashier and back. [`docs/HOW_IT_WORKS.md`](docs/HOW_IT_WORKS.md) explains what happens at each step.

### A. Run the POS with StreetFood switched on

In the POS repo, add these to the environment the POS runs with:

```
STREETFOOD_ENABLED=true                 # everything is off without this
STREETFOOD_WORKERS_ENABLED=true         # sends events, expires orders, scans for changes
STREETFOOD_ENCRYPTION_KEY=<openssl rand -base64 32>     # encrypts each link's secret
STREETFOOD_CHANGE_SCAN_SECONDS=5        # optional: notice menu and hours changes sooner (default 15)
```

Apply the POS migrations as usual (`npx prisma migrate deploy`), then start it.

**How the two reach each other decides how you run the POS.** The POS refuses a platform address
that is plain `http` unless its host is `localhost` or `127.0.0.1`, and a `localhost` address inside a
container points at the container itself. So:

- **Run the POS directly on your machine** (`npm run start:dev`, port 8068 unless `PORT` is set).
  Then the API is `http://localhost:8080` for the POS, and the POS is `http://localhost:8068` for the
  API. This is the simple path.
- **Run the POS in Docker** (`sh scripts/compose.sh dev …`, published on port 18068): the API can
  still reach it at `http://localhost:18068`, but the POS cannot reach `http://localhost:8080` from
  inside its container. Give the API an `https` address (a tunnel such as ngrok) and add its hostname
  to the POS's `STREETFOOD_ALLOWED_PLATFORM_HOSTS`.

### B. Link the business (in the POS)

**In the POS web app (`ansilum-web`): open `StreetFood` in the menu** (Orders group). It connects the
business, shows the one-time secret together with the details to pass on to the operator, and lets
the owner publish outlets and set their status and hours. The calls below are what that screen does.

As the business owner, with your usual POS session and `x-tenant-id` header:

```sh
POS=http://localhost:8068/app/terrapos/integrations/streetfood

curl -X POST $POS/link   -d '{"platformUrl":"http://localhost:8080"}' …    # returns tenantId, keyId and a ONE-TIME secret
curl -X PUT  $POS/outlets/<outletId>/settings    -d '{"onlineStatus":"OPEN"}' …
curl -X PUT  $POS/outlets/<outletId>/publication -d '{"enabled":true}' …
```

(Add `-H 'Content-Type: application/json'`, the owner's `Authorization` and `x-tenant-id` headers to
each.) The secret is shown once; it is the shared key for everything below. Without `OPEN` the
outlet follows its weekly hours (`PUT $POS/outlets/<outletId>/hours`).

### C. Register the business here (as the operator)

```sh
curl -X POST localhost:8080/v1/admin/merchants \
  -H "Authorization: Bearer $ADMIN" -H 'Content-Type: application/json' \
  -d '{"name":"Warung Sri","posTenantId":"<tenantId>","keyId":"sfk_…","secret":"sfs_…","posBaseUrl":"http://localhost:8068"}'
```

The credentials are tested against the POS before anything is stored, and the outlets and menus are
loaded at once. For a business that is already registered (it created a new secret, or disconnected and
connected again), replace its credentials instead:
`PUT /v1/admin/merchants/<id>/link` with `keyId`, `secret` and `posBaseUrl`. In development `localhost` and `127.0.0.1` are accepted; anything else must be `https`
and listed in `POS_ALLOWED_HOSTS`.

### D. See it, order from it

```sh
curl "localhost:8080/v1/outlets/nearby?lat=-6.25&lng=106.85&radius_m=2000"      # the outlet, with its menu link and position
curl localhost:8080/v1/outlets/<id>/menu

curl -X POST localhost:8080/v1/checkouts \
  -H "Authorization: Bearer $(cat .dev-tokens/rina)" -H 'Idempotency-Key: try-1' -H 'Content-Type: application/json' \
  -d '{"outletId":"<id>","items":[{"productId":"<productId from the menu>","quantity":2}]}'
```

The order appears in the cashier's list in the POS (`GET /app/terrapos/streetfood-orders`); accept,
start, ready and complete it there, and `GET /v1/orders/<orderId>` follows along. Open
`/v1/orders/<orderId>/stream` for the live version.

### E. A mobile vendor on the map

Switch the outlet to a mobile one (`PUT $POS/outlets/<outletId>/settings` with
`{"locationMode":"MOBILE"}`), then, as the vendor's staff, open the day and send positions:

```sh
VENDOR=http://localhost:8068/app/terrapos/streetfood-vendor
curl -X POST $VENDOR/session/start    -d '{"outletId":"<id>","latitude":-6.2001,"longitude":106.8001}' …
curl -X POST $VENDOR/location/pings   -d '{"outletId":"<id>","pings":[{"latitude":-6.2010,"longitude":106.8001,"accuracyM":8,"recordedAt":"<now, ISO 8601>"}]}' …
```

A vendor that has not opened its day is not on the map. The position reaches
`/v1/outlets/nearby` within seconds of the POS publishing it (at most one update per 15 seconds and
only after a 20 m move). The full story is in the tracking section of `docs/HOW_IT_WORKS.md`.

### F. Online payment in development

1. Create a sandbox account with Xendit or Midtrans for the merchant.
2. `PUT /v1/admin/merchants/<id>/gateway` with the keys (Xendit also needs its callback token). The
   keys are checked against the gateway before they are saved. The answer contains `callbackUrl`.
3. Enter that URL in the gateway's dashboard. Gateways call it over the internet, so use a tunnel and
   set `PUBLIC_BASE_URL` to the tunnel's address.
4. Check out with `"payment":{"mode":"ONLINE"}`.

Without a tunnel the API still finds payments on its own: a poller asks the gateway about every open
payment that has had no news for 20 seconds, so a paid order moves on within about half a minute instead of instantly. To run
without any gateway at all, point `XENDIT_BASE_URL` (or `MIDTRANS_SNAP_URL` and `MIDTRANS_API_URL`)
at a stub you control.

## Configuration reference

All settings come from the environment (see `.env.example`). Required ones stop startup when missing.

| Setting | Default | Meaning |
|---|---|---|
| `APP_ENV` | `development` | `development`, `staging` or `production`. Outside development, browser origins must be `https`. In production, `localhost` POS addresses are refused too |
| `HTTP_PORT` | `8080` | listen port |
| `HTTP_CORS_ALLOW_ORIGINS` | `http://localhost:5173` | comma-separated browser origins |
| `LOG_LEVEL` | `info` | |
| `PG_URL` | **required** | PostgreSQL connection string |
| `PG_POOL_MAX` | `10` | connection pool size |
| `FIREBASE_PROJECT_ID` | **required** | the Firebase project whose ID tokens are accepted |
| `FIREBASE_JWKS_URL` | Google's | where to fetch the token-signing keys (point at `scripts/dev-jwks.py` locally) |
| `FIREBASE_ISSUER` | derived from the project | override only if you must |
| `FCM_SERVICE_ACCOUNT_FILE` | unset | Firebase service account (JSON) for push notifications. Without it orders and streams work, push is skipped |
| `ADMIN_API_TOKEN` | **required** | at least 32 characters |
| `ENCRYPTION_KEY` | **required** | base64, 32 bytes |
| `POS_ALLOWED_HOSTS` | empty | comma-separated hostnames merchants' POS may be on (https only) |
| `INTEGRATION_WORKER_INTERVAL` | `1s` | how often stored POS events are applied while idle |
| `INTEGRATION_RECONCILE_INTERVAL` | `15m` | how often each merchant is re-read from its POS |
| `FEE_CUSTOMER_BPS` | `250` | the customer's fee on online orders, in basis points (250 = 2.5%) |
| `FEE_MERCHANT_BPS` | `250` | the merchant's fee on every order |
| `PAYMENT_WINDOW` | `15m` | how long a customer has to pay an online order |
| `PUBLIC_BASE_URL` | unset | this API's public address, shown as the gateway callback address |
| `PAYMENT_RETURN_URL` | unset | where gateways send customers after paying (an app deep link) |
| `XENDIT_BASE_URL`, `MIDTRANS_SNAP_URL`, `MIDTRANS_API_URL` | the real gateways | point at a stub for tests or staging |
| `METRICS_ENABLED` | `true` | Prometheus metrics at `/metrics` |

## Tests

```sh
make test                              # unit tests
TEST_PG_URL=postgres://…/empty_db make test-db   # everything, against a throwaway database
```

Database tests apply the migrations to the database in `TEST_PG_URL` and empty its tables: use a
dedicated one. Packages run one at a time (`-p 1`) because they share it.

`internal/testutil/testdata/pos/` holds what the real POS produced in a scripted day.
`TestReplayRealPOSEvents` replays it, so a change to the POS wire format shows up here. The signing
code is checked against vectors generated by the POS implementation.

## Layout

```
cmd/{app,migrate,healthcheck}    entry points
config/                          environment settings
internal/entity/                 domain types and rules (open now, where is the vendor)
internal/usecase/{integration,discovery,identity,merchant,ordering,payment}
internal/repository/persistent/  PostgreSQL
internal/repository/webapi/      the POS client, payment gateways, Firebase token verification
internal/controller/http/        routes, middleware, handlers
internal/controller/poller/      the background workers
pkg/                             http server, logger, postgres, migrator, signing, secretbox
migrations/                      SQL, applied by cmd/migrate (the API never migrates itself)
scripts/dev-jwks.py              local stand-in for Firebase sign-in (development only)
api/openapi.yaml                 the API for apps
docs/HOW_IT_WORKS.md             how the POS, this API and the tracking engine exchange information
```

## Troubleshooting

| Symptom | Likely cause |
|---|---|
| `docker compose up` says network `local-dev` was not found | Start the shared local infrastructure (it creates the `local-dev` network and `local-dev-postgres-1`) |
| The API exits at startup with a Firebase error | It cannot download Firebase's keys. Check network access, or use `scripts/dev-jwks.py` and set `FIREBASE_JWKS_URL` |
| The API exits at startup complaining about `ADMIN_API_TOKEN` or `ENCRYPTION_KEY` | Too short, or not 32 base64 bytes. Generate them with the commands above |
| Migrations fail on `CREATE EXTENSION` | The database user cannot create extensions. Use a superuser for development, or install `cube` and `earthdistance` ahead of time |
| Registering a merchant fails with "the POS did not accept these credentials" | Wrong `keyId` or `secret`, the POS is not running with `STREETFOOD_ENABLED=true`, the POS address is unreachable from the API, or the link was created for a different tenant |
| Registering fails with "the POS host is not allowed" | Outside `localhost`, the address must be `https` and listed in `POS_ALLOWED_HOSTS` |
| The outlet registers but never shows in `/v1/outlets/nearby` | It is not published, its status is `CLOSED`, it is a mobile vendor without an open day, or it has no coordinates. Check `GET /v1/admin/merchants` and the outlet in the POS |
| Menu or hours changes in the POS take a while to appear | The POS looks for changes every `STREETFOOD_CHANGE_SCAN_SECONDS` (15 s by default) and delivers events from a worker that needs `STREETFOOD_WORKERS_ENABLED=true` |
| Orders stay `CREATED` or end as `MERCHANT_UNREACHABLE` | The API cannot reach the POS. Check `posBaseUrl`, that the POS is up, and that the key is still active |
| A paid order stays `UNPAID` | The gateway's callback cannot reach the API (no tunnel). The poller will find it within about half a minute; check the gateway keys and `callbackUrl` |
| Signature errors in the POS or API logs (`SIGNATURE_INVALID`, 401) | The secret differs between the two sides, or the machines' clocks differ by more than 5 minutes. After a new secret or a reconnect in the POS, the operator must pass it on with `PUT /v1/admin/merchants/<id>/link` (registering again is refused: the business is already registered) |

## Deploying to staging and production (Neon)

Staging and production run the same two images, built by CI on `main` (`.gitlab-ci.yml`):

| Image | Target | Job |
|---|---|---|
| `$CI_REGISTRY_IMAGE/api:<sha>` | `production` | serves the API on `HTTP_PORT` |
| `$CI_REGISTRY_IMAGE/migrate:<sha>` | `migration` | applies `migrations/` and exits; the API never migrates itself |

The two environments differ only in their configuration and their database. Use a separate Neon
database for each, and never point staging at the production database.

### 1. Create the Neon databases

1. In the [Neon console](https://console.neon.tech), create a project (pick the region closest to
   where the API runs, and PostgreSQL 15 or newer).
2. Use one of these layouts:
   - **Two projects**, `streetfood-staging` and `streetfood-production`. Strongest isolation, separate
     credentials and billing views. Recommended.
   - **One project, two branches** (`production` is the default branch, `staging` a child of it).
     Cheaper, but a child branch starts with a copy of the parent's data, so create `staging` before
     production holds real customer data, or create it from an empty point in time.
3. Create a database named `streetfood` and a dedicated role for the API in each. Do not use the
   project's owner role for the running API.
4. Copy two connection strings from **Connect** in each environment:

| Name | Neon setting | Looks like | Used by |
|---|---|---|---|
| Direct | "Connection pooling" off | `postgres://user:pass@ep-xxxx.<region>.aws.neon.tech/streetfood?sslmode=require` | migrations |
| Pooled | "Connection pooling" on | `postgres://user:pass@ep-xxxx-pooler.<region>.aws.neon.tech/streetfood?sslmode=require` | the API (`PG_URL`) |

Keep `sslmode=require`; Neon refuses unencrypted connections. Migrations use the direct string
because they run DDL and should not go through PgBouncer.

The first migration runs `CREATE EXTENSION cube` and `earthdistance`. Neon supports both, and the
default owner role (a member of `neon_superuser`) may create them. If you migrate with a more
limited role, run this once as the owner in the Neon SQL editor first:

```sql
CREATE EXTENSION IF NOT EXISTS cube;
CREATE EXTENSION IF NOT EXISTS earthdistance;
```

### 2. Create the configuration and Docker secrets

Each stage has two kinds of settings, kept apart like in `ansilum-be`:

| What | Where | In git |
|---|---|---|
| Non-secret settings (image tags, origins, Firebase project, POS hosts, public URL) | `.env.<stage>` in the repo root | no (ignored; template in `docker/env/<stage>.env.example`) |
| `app.env`: `PG_URL` (Neon **pooled**), `ADMIN_API_TOKEN`, `ENCRYPTION_KEY` | `secrets/<stage>/app.env` | no |
| `migration.env`: `PG_URL` (Neon **direct**) only | `secrets/<stage>/migration.env` | no |

The two secret files are Docker secrets (`docker-compose.deploy.yml`). They are mounted read-only
at `/run/secrets/` and read by the app through `ENV_FILE`. They are not in the image, in
`docker inspect`, or in the container's environment list, and the API and the migration job get
separate files, so the API never holds the DDL-capable database role.

On the deploy host:

```sh
git clone <repo> && cd streetfood-customer-api      # or copy docker-compose.deploy.yml, scripts/, docker/

sh scripts/new-secrets.sh staging        # creates secrets/staging/{app,migration}.env with fresh random
                                         # ADMIN_API_TOKEN and ENCRYPTION_KEY (never overwrites)
chmod u+w secrets/staging/*.env          # edit PG_URL in both with the Neon strings, then:
chmod 400 secrets/staging/*.env
sudo chown 1000:1000 secrets/staging/*.env     # Linux: the containers run as uid 1000

cp docker/env/staging.env.example .env.staging # set the image tags, origins, Firebase project, hosts
```

Repeat with `production` (`docker/env/production.env.example` to `.env.production`). Use a
different `ADMIN_API_TOKEN` and `ENCRYPTION_KEY` per stage (the script does), and keep a backup of
the production `ENCRYPTION_KEY` in your password manager: without it every stored merchant secret
and gateway key is unreadable.

`scripts/compose.sh <staging|production> …` runs `docker compose` for exactly one stage
(project `streetfood-<stage>`). It stops early when a secret file is missing, readable by every user,
or not readable by uid 1000 (checked on Linux; Docker Desktop does not enforce host permissions).

Notes:

- Outside `development`, CORS origins must be `https`, and in `production` a `localhost` POS address
  is refused.
- Keep `PG_POOL_MAX` at or below what the Neon compute allows. The pooler accepts many client
  connections, but each Neon plan limits connections to the database.
- Dotenv values are not shell-evaluated. Quote values that contain `#` or spaces.
- Other secrets follow the same pattern: add a line to `app.env`, or mount a file as a further secret
  and point a `NAME_FILE` setting at it (for example `FCM_SERVICE_ACCOUNT_FILE=/run/secrets/fcm`).
  Setting both `NAME` and `NAME_FILE` fails startup.
- Rotate a secret by editing its file and running `sh scripts/compose.sh <stage> up -d app`.
- Never commit `.env.<stage>` or `secrets/`; `.gitignore` and `.dockerignore` already exclude them.

### 3. Run the migrations

Set `MIGRATION_IMAGE` in `.env.<stage>` to the release's `migrate:<sha>` image, then:

```sh
sh scripts/compose.sh staging run --rm migrate              # applies pending migrations
sh scripts/compose.sh staging run --rm migrate version      # current version
sh scripts/compose.sh staging run --rm migrate force <n>    # after you fixed a failed migration by hand
```

Do staging first. A failed migration leaves the database "dirty": fix the cause, then `force` the
last good version and run it again. You can also run it from a checkout against Neon:
`PG_URL='<direct string>' go run ./cmd/migrate -path migrations up`.

### 4. Start the API

Set `APP_IMAGE` to the matching `api:<sha>` image, then:

```sh
sh scripts/compose.sh staging up -d app
sh scripts/compose.sh staging ps          # "healthy" after the start period
sh scripts/compose.sh staging logs -f app
```

The container runs from a `scratch` image as uid 1000 with a read-only root filesystem, all Linux
capabilities dropped, `no-new-privileges`, and memory, CPU and process limits. It listens on
`127.0.0.1:18080` (`APP_BIND_ADDRESS`, `APP_PORT`); put a TLS reverse proxy (Caddy, nginx, a cloud
load balancer) in front of it on `PUBLIC_BASE_URL` and set rate limits there (the API has none yet).
Do not expose `/metrics` to the internet; restrict it at the proxy.

Other hosts work the same way: run the `migrate` image as a one-off job or release command, then
roll out the `api` image with the same variables (Kubernetes secrets, Cloud Run, ECS, and so on).

### 5. Verify

```sh
curl -fsS https://api-staging.example.com/healthz                    # 200
curl -fsS https://api-staging.example.com/v1/admin/merchants \
  -H "Authorization: Bearer $(sed -n 's/^ADMIN_API_TOKEN=//p' secrets/staging/app.env)"                        # {"merchants":[]} on a new database
```

Then, on staging, register a test merchant and run an order end to end (see
[the whole loop](#run-the-whole-loop-pos-api-and-a-customer)), with the POS's platform address set to
the staging API. Promote to production by putting the same image tags in `.env.production` and running steps 3 and 4
with `production` instead of `staging`.

### 6. Releases and rollbacks

1. Merge to `main`; CI publishes `api:<sha>` and `migrate:<sha>`.
2. Deploy that tag to staging (migrate, then API), check it, then deploy the same tag to production.
3. Roll the API back by starting the previous `api` tag. Migrations have no down step in the release
   process, so write each one to work with the previous API version too (add columns before using
   them, drop them a release later).
4. Before a risky production migration, create a Neon branch of the production database as a restore
   point (Neon also keeps point-in-time history, whose length depends on your plan).

### Neon specifics

| Topic | What to know |
|---|---|
| Scale to zero | A suspended compute needs a moment to wake. The first request after idle time can be slow. On production, turn scale-to-zero off (paid plans) or accept the delay; staging can keep it on |
| Startup | The API connects at startup. If the database is waking, a single failed start is normal: restart policy `unless-stopped` retries it |
| Pooled connections | If you see prepared-statement errors (`prepared statement … does not exist`) through the pooler, use the direct string for `PG_URL` and lower `PG_POOL_MAX` |
| Background workers | The integration and payment workers run inside the API process, so every API instance runs them. Start with one instance, and scale out only after testing on staging |
| Credentials | Rotate the role's password in Neon, update `PG_URL` in the stage's secret files, then `sh scripts/compose.sh <stage> up -d app`. Nothing else stores it |
| Backups | Rely on Neon's history plus periodic `pg_dump` from the direct string for production |

## Operating notes

- All times in responses are UTC.
- Set `PUBLIC_BASE_URL` so the operator API can show merchants their gateway callback address.
- Push notifications use FCM when `FCM_SERVICE_ACCOUNT_FILE` is set; without it orders still work and
  the stream still updates, only push is skipped.
- Orders are sent to the POS by a worker. If the POS cannot be reached for 2 minutes the order fails
  with `MERCHANT_UNREACHABLE`.
- Startup fetches Google's token-signing keys once and refreshes them in the background; with no
  network at boot the API refuses to start.
- Secrets can be given as `NAME_FILE=/path` (Docker or Kubernetes secrets).
- `/metrics` (Prometheus) is on unless `METRICS_ENABLED=false`; put it behind your proxy.
- No rate limiting yet. Put the public endpoints behind your proxy's limits until it is added.

