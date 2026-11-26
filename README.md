# StreetFood customer API

The backend for the customer app of StreetFood OS. Customers find street food vendors and kiosks
near them, see what they sell, order, pay, and watch their order progress. Merchants keep using
their POS (`ansilum-be`). This service holds a copy of what the POS publishes and serves it to
customers; the POS stays the source of truth.

It is a Go service (Fiber, pgx, golang-migrate) built from the infrastructure of `goldenlane-be`,
with none of its HR code.

**How the two backends and the vendor tracking talk to each other is explained in
[`docs/HOW_IT_WORKS.md`](docs/HOW_IT_WORKS.md).** Read it after the setup below. **App developers:
[`docs/MOBILE_API.md`](docs/MOBILE_API.md) compares the two apps and their APIs, with a guide for each:
[customer app (Goldenlane, Flutter)](docs/MOBILE_API_CUSTOMER.md) and
[vendor / POS app (Ansilum)](docs/MOBILE_API_VENDOR.md).
[`docs/USER_JOURNEY.md`](docs/USER_JOURNEY.md) explains how vendors and customers use the system.
[`docs/NEON_COST_STRATEGY.md`](docs/NEON_COST_STRATEGY.md) is the plan to cut the Neon bill of both backends,
and [`docs/NEON_PLAN_POS_BACKEND.md`](docs/NEON_PLAN_POS_BACKEND.md) the plan for the POS backend (done in `ansilum-be`, except the parts it lists as skipped).**

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
10. [Redis (optional): signals between instances](#redis-optional-signals-between-instances)
11. [Operating notes](#operating-notes)

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

![The two systems in one picture](docs/images/system-overview.svg)

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

### C. Register the business here

Normally nobody does this by hand. The owner taps **Connect to Goldenlane** in the POS app (Map
tab), and the POS calls `POST /v1/merchants/connect` itself with the same body as below. That route
needs no operator token: the POS must accept the credentials and confirm the same `posTenantId`
(the `tenantId` field of its `GET integrations/streetfood/outlets` answer), and only hosts in
`POS_ALLOWED_HOSTS` are contacted. Repeating it with new credentials updates the merchant; a
suspended merchant cannot come back this way. It is rate limited to 20 calls a minute per address.

The operator route below still works, for example to register a business by hand:

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
| `WORKER_SWEEP_INTERVAL` | `1h` | the longest a background worker sleeps with nothing scheduled (1m to 24h): a safety net, since workers run when work is created and when a retry or deadline falls due |
| `INTEGRATION_RECONCILE_INTERVAL` | `1h` | how often each merchant is re-read from its POS, a safety net |
| `REDIS_URL` | empty | Redis for signals between several instances (see [Redis](#redis-optional-signals-between-instances)). Empty with one instance |
| `FEE_CUSTOMER_BPS` | `250` | the customer's fee on online orders, in basis points (250 = 2.5%) |
| `FEE_MERCHANT_BPS` | `250` | the merchant's fee on every order |
| `PAYMENT_WINDOW` | `15m` | how long a customer has to pay an online order |
| `PUBLIC_BASE_URL` | unset | this API's public address, shown as the gateway callback address |
| `PAYMENT_RETURN_URL` | unset | where gateways send customers after paying (an app deep link) |
| `XENDIT_BASE_URL`, `MIDTRANS_SNAP_URL`, `MIDTRANS_API_URL` | the real gateways | point at a stub for tests or staging |
| `METRICS_ENABLED` | `true` | Prometheus metrics at `/metrics` |

### Every setting in detail

Durations use Go's format: `500ms`, `1s`, `15m`, `1h`. Where a deployed stage keeps each value is
described in [deploying](#deploying-to-staging-and-production-neon): secrets go in
`secrets/<stage>/app.env`, the rest in `.env.<stage>` (development: `.env`).

#### The deployment

**`APP_ENV`**: which stage this process is. Accepts `development`, `staging` or `production`; anything
else stops startup. Nothing to obtain. In deployed stages `scripts/compose.sh` sets it from the stage
you name, so you do not write it in `.env.<stage>`. It changes three rules: outside `development`
every browser origin must be `https`; in `production` a merchant's POS may not be `localhost`; and the
`.env` file is read only in development.

**`HTTP_PORT`**: the port the API listens on inside its process or container. Leave `8080`. The
container always uses 8080; to change the port on the host use `APP_PORT` in `.env.<stage>`, not this.

**`HTTP_CORS_ALLOW_ORIGINS`**: the web origins allowed to call the API from a browser. An origin is
scheme, host and port, with no path or trailing slash: `https://app.example.com`. List several with
commas. Get them from wherever your web front end is hosted (the address in the browser's address
bar). Native mobile apps send no `Origin` header and need no entry. Outside development every entry
must be `https`. The default `http://localhost:5173` is the local web dev server (Vite).

**`LOG_LEVEL`**: how much the API logs: `debug`, `info`, `warn` or `error`. Use `debug` while testing
and `info` in production.

#### The database

**`PG_URL`** (required): the PostgreSQL connection string, `postgres://USER:PASSWORD@HOST:PORT/DATABASE?sslmode=…`.
- Development: `postgres://developer:<password>@localhost:5433/streetfood?sslmode=disable` (the shared
  `local-dev-postgres-1` container; the password is in `../ansilum-be/secrets/development/`).
- Neon: in the Neon console open your project, choose the branch and database, click **Connect**, pick
  the role, and copy the string. For the running API choose the **pooled** one (host contains
  `-pooler`); for migrations choose the **direct** one (no `-pooler`). Keep `?sslmode=require`.
- A password containing `@`, `:`, `/` or `#` must be percent-encoded in the URL (`@` is `%40`).
- It is a secret: put it in `secrets/<stage>/app.env` (and the direct one in `migration.env`).

**`PG_POOL_MAX`**: how many database connections one API process keeps open (default 10). The total
across all running instances must stay under the connection limit of your Neon compute. Ten is
enough for one instance; raise it only if you see requests waiting for a connection.

#### Customer sign-in (Firebase)

Customers sign in with the Firebase SDK in the app. This API never sees passwords; it only checks
that the ID token the app sends was issued by your Firebase project.

**`FIREBASE_PROJECT_ID`** (required): the project ID. Get it in the
[Firebase console](https://console.firebase.google.com), Project settings, General, **Project ID**
(for example `posterra-cceaa`). It must be the project the customer app is configured with, or every
request fails with 401. Use a separate Firebase project for staging if you have one. In local
development with `scripts/dev-jwks.py` any name works, as long as the helper is started with the same.

**`FIREBASE_JWKS_URL`**: where the API downloads the public keys that verify tokens. The default is
Google's published keys and is right for staging and production: leave it unset. Set it only in
development, to the local helper (`http://127.0.0.1:55996`). Startup fails if the address cannot be
reached, so never point a deployed stage at a helper.

**`FIREBASE_ISSUER`**: the expected `iss` claim of the tokens. The default,
`https://securetoken.google.com/<FIREBASE_PROJECT_ID>`, is what Firebase issues. Leave it unset. It
exists for tests that sign their own tokens.

**`FCM_SERVICE_ACCOUNT_FILE`**: path to a Firebase service account key (JSON) that lets the API send
push notifications about order status. Without it everything works and only push is skipped. To get
it: Firebase console, Project settings, **Service accounts**, **Generate new private key**, and
download the JSON. Keep it out of git. On a deployed stage give it to the container as a Docker
secret, then point the setting at the mounted path:

```yaml
# docker-compose.deploy.yml
services:
  app:
    environment:
      FCM_SERVICE_ACCOUNT_FILE: /run/secrets/fcm
    secrets: [app_env, fcm]
secrets:
  fcm:
    file: ./secrets/staging/fcm.json     # mode 400, owner uid 1000 on Linux
```

#### Operators and encryption

**`ADMIN_API_TOKEN`** (required): the bearer token for the operator API (`/v1/admin`, where merchants
are registered and gateways configured). Anyone holding it controls every merchant, so treat it like
a root password. Generate it, at least 32 characters:

```sh
openssl rand -hex 32
```

Use a different one per stage. To rotate it, change the file and restart the API; update whoever
uses it.

**`ENCRYPTION_KEY`** (required): encrypts, at rest in the database, every merchant's POS secret and
payment gateway keys. It must be 32 random bytes in base64 (a 44-character string ending in `=`):

```sh
openssl rand -base64 32
```

Use a different one per stage and keep a backup in your password manager. If it is lost or changed,
the stored secrets cannot be decrypted and every merchant must be registered again with new
credentials.

#### Links with the POS

**`POS_ALLOWED_HOSTS`**: the hostnames a merchant's POS may be registered with. It stops an operator
(or a stolen operator token) from pointing the API at arbitrary servers. Write hostnames only, no
scheme, port or path, comma-separated: `pos.example.com,pos-eu.example.com`. The value is the host of
your `ansilum-be` deployment (the address in `posBaseUrl` when you register a merchant). Only
`https` is accepted, except `localhost` outside production. Empty means no remote POS can be
registered, so set it on every deployed stage.

**`WORKER_SWEEP_INTERVAL`** (default `1h`, between `1m` and `24h`): the longest a background worker
sleeps when nothing is scheduled. Workers do not poll. They run right after a commit that wrote rows
(new POS events, a checkout, a payment, a push to send), when a retry, lease or payment deadline falls
due, and otherwise only on this sweep, which catches work another instance created or a wake-up that
was missed. Every sweep touches the database, and Neon suspends it after 5 idle minutes, so a short
sweep costs compute: keep it long on Neon, and shorten it (for example `1m`) only while debugging.

**`INTEGRATION_RECONCILE_INTERVAL`** (default `1h`): how often the API re-reads every merchant's
outlets and menus from its POS, as a safety net that repairs anything a lost event left wrong. The POS
pushes every change, so this rarely finds anything. Raise it with many merchants; lower it only while
debugging.

**`REDIS_URL`** (optional): the address of a Redis that carries signals between several instances of this
service, `redis://[:password@]host:6379/0` or `rediss://…` for TLS. Leave it empty with one instance: nothing
is lost. It holds no data, only "something changed" messages. Setup, checks and troubleshooting are in
[Redis](#redis-optional-signals-between-instances). It is a secret when it has a password.

#### Online payment and the platform fee

**`FEE_CUSTOMER_BPS`** (default `250`): the fee added on top of an online order, charged to the
customer, in basis points of the order total. 100 basis points is 1%, so `250` is 2.5%. Allowed
range 0 to 2000. It is a business decision.

**`FEE_MERCHANT_BPS`** (default `250`): the fee the merchant owes on every completed order (online or
at the counter), in basis points. Same range. Changing either value affects orders from then on; it
does not rewrite completed ones.

**`PAYMENT_WINDOW`** (default `15m`): how long a customer has to pay an online order before it
expires. Gateway payment pages are created to match.

**`PUBLIC_BASE_URL`**: the public `https` address of this API with no trailing slash, for example
`https://api.example.com`. It is what you serve behind your reverse proxy. The API shows it to
merchants as their gateway callback address (`<PUBLIC_BASE_URL>/…`, returned as `callbackUrl` when
you configure a merchant's gateway), which the merchant enters in the Xendit or Midtrans dashboard.
Wrong or unset means gateways cannot call back, and paid orders are found only by the poller (about
half a minute late). Locally it can be `http://localhost:8080`, or a tunnel address when testing real
gateways.

**`PAYMENT_RETURN_URL`**: where gateways send the customer's browser after paying or failing: a deep
link into the customer app (for example `streetfood://payment-result`) or an https page that opens
it. Take it from your mobile app's URL scheme or universal-link configuration. Optional: without it
the customer stays on the gateway's own page.

**`XENDIT_BASE_URL`, `MIDTRANS_SNAP_URL`, `MIDTRANS_API_URL`**: addresses of the payment gateways.
Leave all three unset: the API then uses the real Xendit and Midtrans. Set them only to point at a
stub server you run for tests or a staging without gateway accounts. The merchants' own gateway keys
are not environment settings; the operator stores them per merchant with
`PUT /v1/admin/merchants/<id>/gateway`.

#### Monitoring

**`METRICS_ENABLED`** (default `true`): serves Prometheus metrics at `/metrics`. Point your monitoring
at it from inside your network and block `/metrics` at the reverse proxy so the internet cannot read
it. Set `false` if you do not scrape it.

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

Staging and production do not build the application themselves. They run the same two images,
built once from one commit (step 2):

| Image | Dockerfile target | Job |
|---|---|---|
| `streetfood-api:<tag>` | `production` | serves the API on `HTTP_PORT` |
| `streetfood-migrate:<tag>` | `migration` | applies `migrations/` and exits; the API never migrates itself |

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

### 2. Build and push the release images

Build one release from a commit and give it an immutable identifier, the commit hash. Both images
must come from the same commit.

```sh
export IMAGE_TAG="$(git rev-parse --short=12 HEAD)"
export REGISTRY="dolanddev"          # your Docker Hub user or organisation, or a private registry

docker build --target production --build-arg APP_VERSION="$IMAGE_TAG" \
  -t "$REGISTRY/streetfood-api:$IMAGE_TAG" .

docker build --target migration \
  -t "$REGISTRY/streetfood-migrate:$IMAGE_TAG" .

docker login                         # once; add the registry host for a private registry
docker push "$REGISTRY/streetfood-api:$IMAGE_TAG"
docker push "$REGISTRY/streetfood-migrate:$IMAGE_TAG"
```

Build from a clean checkout of the commit you mean to release (`git status` shows nothing). If you
build on an Apple Silicon Mac for a Linux amd64 server, add `--platform linux/amd64` to both builds.
`APP_VERSION` is shown by the app as its version.

For stronger immutability, deploy registry digests instead of tags. Read them after the push:

```sh
docker inspect --format '{{index .RepoDigests 0}}' "$REGISTRY/streetfood-api:$IMAGE_TAG"
docker inspect --format '{{index .RepoDigests 0}}' "$REGISTRY/streetfood-migrate:$IMAGE_TAG"
# dolanddev/streetfood-api@sha256:…
```

**Where the values of `APP_IMAGE` and `MIGRATION_IMAGE` come from:** these are the two image
references you just pushed, in `.env.<stage>`:

```sh
APP_IMAGE=dolanddev/streetfood-api:0123456789ab              # or ...@sha256:<digest>
MIGRATION_IMAGE=dolanddev/streetfood-migrate:0123456789ab    # same commit as APP_IMAGE
```

The same two lines go into `.env.staging` first and, once staging is verified, into `.env.production`.
The host pulls them (`docker login` there too for a private repository).

**With GitLab CI instead:** a merge to `main` runs the `images` job in `.gitlab-ci.yml`, which does
the same builds and pushes `$CI_REGISTRY_IMAGE/api:<sha>` and `$CI_REGISTRY_IMAGE/migrate:<sha>`
(`<sha>` is the first 12 characters of the commit) to the project's GitLab container registry.
Use those references as `APP_IMAGE` and `MIGRATION_IMAGE`, for example
`registry.gitlab.com/<group>/streetfood-customer-api/api:<sha>`, and `docker login registry.gitlab.com`
on the host with a deploy token that has `read_registry`.

### 3. Create the configuration and Docker secrets

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

#### What to put in `.env.<stage>`

| Setting | Required | What to fill | Used for |
|---|---|---|---|
| `APP_IMAGE` | yes | The API image you pushed in step 2, for example `dolanddev/streetfood-api:0123456789ab` (or `@sha256:` digest). Must not be a moving tag such as `latest` | The container the `app` service runs |
| `MIGRATION_IMAGE` | yes | The migration image from the **same commit** as `APP_IMAGE`, for example `dolanddev/streetfood-migrate:0123456789ab` | The one-off `migrate` job (step 4) |
| `APP_SECRETS_PATH` | yes | Path to the API's secret file, `./secrets/<stage>/app.env` (relative to the repo root, or absolute) | Mounted as the Docker secret `app_env` |
| `MIGRATION_SECRETS_PATH` | yes | Path to the migration's secret file, `./secrets/<stage>/migration.env` | Mounted as the Docker secret `migration_env` |
| `APP_BIND_ADDRESS` | no, `127.0.0.1` | The host address the port is published on. Keep `127.0.0.1` when a reverse proxy on the same host serves the public traffic. Use `0.0.0.0` only if nothing in front runs on this host (then firewall it) | Which network interfaces can reach the container |
| `APP_PORT` | no, `18080` | The host port the reverse proxy forwards to. The container always listens on 8080 inside | Port mapping `APP_BIND_ADDRESS:APP_PORT:8080` |
| `LOG_LEVEL` | no, `info` | `debug` on staging, `info` on production (`debug`, `info`, `warn`, `error`) | Log verbosity |
| `HTTP_CORS_ALLOW_ORIGINS` | yes | The `https` origins of the web apps that call this API from a browser, comma-separated, no trailing slash: `https://app.example.com`. Mobile apps do not need an entry. Plain `http` is refused outside development | Browser access (CORS) |
| `FIREBASE_PROJECT_ID` | yes | The ID of the Firebase project the customer app signs in with (Firebase console, Project settings, Project ID). Use a separate project for staging if you have one | The API accepts only ID tokens issued by this project |
| `POS_ALLOWED_HOSTS` | yes | Hostnames only (no scheme or path) of the POS backends merchants may be registered with, comma-separated: `pos.example.com`. Must be `https` hosts. In production `localhost` is refused | Guards `POST /v1/admin/merchants` against pointing the API at arbitrary hosts |
| `PUBLIC_BASE_URL` | yes | The public `https` address of this API as customers and gateways reach it, no trailing slash: `https://api.example.com` | Shown to merchants as the callback URL to enter in Xendit or Midtrans |
| `PG_POOL_MAX` | no, `10` | Maximum database connections per API instance. Keep it below the connection limit of the Neon compute, divided by the number of instances | Database connection pool |

Optional settings, only when you need to change the default:

| Setting | Default | What it does |
|---|---|---|
| `APP_MEMORY_LIMIT` | `256m` | Container memory cap |
| `APP_CPUS` | `1.0` | Container CPU cap |
| `FEE_CUSTOMER_BPS` | `250` | Fee added to online orders, in basis points (250 = 2.5%, maximum 2000) |
| `FEE_MERCHANT_BPS` | `250` | Fee the merchant owes on completed orders, in basis points |
| `PAYMENT_WINDOW` | `15m` | How long a customer has to pay an online order |
| `WORKER_SWEEP_INTERVAL` | `1h` | The longest a worker sleeps with nothing scheduled (a safety net; keep it long on Neon) |
| `INTEGRATION_RECONCILE_INTERVAL` | `1h` | How often each merchant's outlets and menus are re-read from its POS |
| `METRICS_ENABLED` | `true` | Prometheus metrics at `/metrics` (keep it off the public internet) |

Values that are not in the list, such as `PAYMENT_RETURN_URL` (the app deep link gateways send
customers back to) or `FCM_SERVICE_ACCOUNT_FILE` (push notifications), are not passed through
`.env.<stage>`: put them in `secrets/<stage>/app.env`, or add them to the `environment:` list in
`docker-compose.deploy.yml`.

Values in `.env.<stage>` are not secrets, but they are specific to a stage: do not copy staging's
`PUBLIC_BASE_URL`, `FIREBASE_PROJECT_ID`, origins or POS hosts into production.

#### What to put in the secret files

| File | Setting | What to fill |
|---|---|---|
| `secrets/<stage>/app.env` | `PG_URL` | Neon connection string of the **restricted API role**, using the **pooled** host (contains `-pooler`) and `?sslmode=require` |
| | `ADMIN_API_TOKEN` | At least 32 random characters, generated by `scripts/new-secrets.sh` (`openssl rand -hex 32`). It is the bearer token for `/v1/admin`; give it only to operators |
| | `ENCRYPTION_KEY` | 32 random bytes in base64, generated by `scripts/new-secrets.sh` (`openssl rand -base64 32`). Encrypts merchants' POS secrets and gateway keys. **Never change it after merchants are registered**, and back it up |
| `secrets/<stage>/migration.env` | `PG_URL` | Neon connection string of a role that can run DDL and create extensions (the owner role), using the **direct** host (no `-pooler`) and `?sslmode=require` |

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

### 4. Run the migrations

Set `MIGRATION_IMAGE` in `.env.<stage>` to the release's `migrate:<sha>` image, then:

```sh
sh scripts/compose.sh staging run --rm migrate              # applies pending migrations
sh scripts/compose.sh staging run --rm migrate version      # current version
sh scripts/compose.sh staging run --rm migrate force <n>    # after you fixed a failed migration by hand
```

Do staging first. A failed migration leaves the database "dirty": fix the cause, then `force` the
last good version and run it again. You can also run it from a checkout against Neon:
`PG_URL='<direct string>' go run ./cmd/migrate -path migrations up`.

### 5. Start the API

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

### 6. Verify

```sh
curl -fsS https://api-staging.example.com/healthz                    # 200
curl -fsS https://api-staging.example.com/v1/admin/merchants \
  -H "Authorization: Bearer $(sed -n 's/^ADMIN_API_TOKEN=//p' secrets/staging/app.env)"                        # {"merchants":[]} on a new database
```

Then, on staging, register a test merchant and run an order end to end (see
[the whole loop](#run-the-whole-loop-pos-api-and-a-customer)), with the POS's platform address set to
the staging API. Promote to production by putting the same image tags in `.env.production` and running steps 4 and 5
with `production` instead of `staging`.

### 7. Releases and rollbacks

1. Build and push both images from one commit (step 2, or merge to `main` and let CI do it).
2. Deploy that tag to staging (migrate, then API), check it, then deploy the same tag to production.
3. Roll the API back by starting the previous `api` tag. Migrations have no down step in the release
   process, so write each one to work with the previous API version too (add columns before using
   them, drop them a release later).
4. Before a risky production migration, create a Neon branch of the production database as a restore
   point (Neon also keeps point-in-time history, whose length depends on your plan).

### Shipping an update: development, staging, production

The same commit goes through all three; only configuration and database differ.

**Development** (the shared local database):

```sh
git pull
make test                                  # add TEST_PG_URL and use `make test-db` for the database tests
make run                                   # migrates, then serves on :8080 (or: docker compose up --build)
```

**Staging**, then **production** (use `staging` first, then `production` with the same tag):

1. Build and push both images from the commit (step 2). CI does this on `main`.
2. Compare `.env.<stage>` with `docker/env/<stage>.env.example` and add new settings. Recent releases changed these:
   `WORKER_SWEEP_INTERVAL` and `INTEGRATION_RECONCILE_INTERVAL` (both default `1h`) replace the removed
   `INTEGRATION_WORKER_INTERVAL`; `REDIS_URL` (in `secrets/<stage>/app.env`) is optional and only needed with several instances.
3. Set `MIGRATION_IMAGE` and `APP_IMAGE` to the new tag, then:

```sh
sh scripts/compose.sh <stage> config --quiet
sh scripts/compose.sh <stage> pull
sh scripts/compose.sh <stage> run --rm migrate      # stop here if it fails
sh scripts/compose.sh <stage> up -d app
sh scripts/compose.sh <stage> ps                    # healthy
curl -fsS https://<api host>/healthz
```

4. On staging, run an order end to end before touching production. Then check Neon's Monitoring page: with
   nobody using the app the compute should suspend after about 5 minutes.
5. Roll back by putting the previous `APP_IMAGE` tag back and running `up -d app`. Migrations have no down
   step, so keep each one compatible with the previous release.

With more than one instance, start Redis and set `REDIS_URL` (see [Redis](#redis-optional-signals-between-instances)).

### Neon specifics

| Topic | What to know |
|---|---|
| Scale to zero | A suspended compute needs a moment to wake. The first request after idle time can be slow. On production, turn scale-to-zero off (paid plans) or accept the delay; staging can keep it on |
| Startup | The API connects at startup. If the database is waking, a single failed start is normal: restart policy `unless-stopped` retries it |
| Pooled connections | Use the pooled (`-pooler`) string for `PG_URL`: the service holds no `LISTEN` connection any more. If you see prepared-statement errors (`prepared statement … does not exist`) through the pooler, use the direct string and lower `PG_POOL_MAX` |
| Background workers | They run inside the API process and do not poll: they run after a commit that wrote rows, when a retry or deadline falls due, and on a `WORKER_SWEEP_INTERVAL` sweep (one hour). An idle API sends no statements, so Neon can suspend after 5 minutes. Every instance runs them. With one instance nothing else is needed. With several, set `REDIS_URL` so a change made through one reaches the others at once; without it they catch up at the next sweep or safety re-read |
| Reads | Outlets, menus, ratings and a signed-in customer's account are answered from memory (the snapshot reloads within 5 seconds of a write to them, and at least every 10 minutes; an account is remembered for 5 minutes). A customer with the map open sends the database no statements |
| Which service is awake | Tag the connections (`application_name`, set to `streetfood-customer-api` by default) and read Neon's Monitoring page: see `docs/NEON_COST_STRATEGY.md` |
| Credentials | Rotate the role's password in Neon, update `PG_URL` in the stage's secret files, then `sh scripts/compose.sh <stage> up -d app`. Nothing else stores it |
| Backups | Rely on Neon's history plus periodic `pg_dump` from the direct string for production |

## Redis (optional): signals between instances

**You do not need Redis to run this service.** With one instance of the API, leave `REDIS_URL` empty and
skip this section. Redis is for the day you run **two or more instances** (a second container, a rolling
deploy with an overlap, several hosts).

### What it does, and what it never does

Each instance acts on its own commits directly. Redis only lets an instance *tell the others* what it
just did, over one pub/sub channel:

| Signal | The other instances then |
|---|---|
| Rows were committed (with the tables written) | wake their background workers, and drop the discovery snapshot if outlets, hours, menus, ratings or merchants changed |
| An order changed | wake the open order streams of that order, so a customer connected to instance B hears a change made through instance A at once |
| A customer account was changed or deleted | forget their remembered copy of it |

Redis **stores nothing**: no orders, no sessions, no cache of data. A lost message, or Redis being down,
only delays the other instances to their next sweep (`WORKER_SWEEP_INTERVAL`, one hour), the snapshot's
10-minute age limit, an open stream's 2-minute re-read, or the 5-minute account memory. The service starts,
runs and logs one warning if Redis cannot be reached.

### With and without it

| | No Redis (one instance) | Redis, several instances |
|---|---|---|
| Worker wake-up after a commit | this instance, 30 ms | all instances, about 0.1 s |
| Order stream hears a change | this instance's changes | every instance's changes |
| A customer sees a menu or position change | within 5 seconds | within 5 seconds, whichever instance wrote it |
| A deleted account is refused | at once here, within 5 minutes elsewhere | at once everywhere |
| Cost | none | one small Redis (about 10 MB of memory, a few commands a second at busy times) |

### The setting

```
REDIS_URL=redis://:PASSWORD@HOST:6379/0       # TLS: rediss://…
```

It is a secret when it carries a password: in a deployed stage put it in `secrets/<stage>/app.env`, not in
`.env.<stage>`. The channel is named after the service and the stage (`streetfood-customer-api:<APP_ENV>:signals`),
so staging and production never hear each other even on one Redis, but **use a separate Redis (or database
number) per stage** anyway.

### Set it up

**A. Local development (the shared container).** `local-dev-redis-1` from the shared infrastructure is on
host port 6380 and on the `local-dev` network as `redis`:

```
# .env
REDIS_URL=redis://localhost:6380/0           # make run, from your machine
DOCKER_REDIS_URL=redis://redis:6379/0        # docker compose up, from the container
```

To try two instances: `make run` in one terminal and `make run HTTP_PORT=8081` in another, with the same
`REDIS_URL` (a setting given on the `make` line wins over `.env`).

**B. Staging and production, Redis on the same host (bundled).** `docker-compose.deploy.yml` has a `redis`
service behind the `redis` profile: Redis 7 on Alpine, password from a Docker secret, no published port,
no disk, 64 MB memory cap, read-only, non-root. `scripts/new-secrets.sh <stage>` creates the password in
`secrets/<stage>/redis_password` and a commented `REDIS_URL` line in `secrets/<stage>/app.env`.

```sh
# 1. In secrets/<stage>/app.env, uncomment the generated line:
#      REDIS_URL=redis://:<password>@redis:6379/0
# 2. Start Redis, then the API (more instances: add --scale app=2 and put a load balancer in front):
sh scripts/compose.sh staging --profile redis up -d redis
sh scripts/compose.sh staging up -d app
# 3. Check:
sh scripts/compose.sh staging logs app | grep signals        # "signals - subscribed"
```

If you created the secrets before this section existed, add the password yourself:
`openssl rand -hex 24 > secrets/<stage>/redis_password` (mode 400, owner uid 1000 on Linux), and build the
URL from it. The password is hex, so it needs no URL escaping.

**C. A managed Redis.** Any Redis 6 or newer works (Upstash, Redis Cloud, a cloud provider's). Use the
`rediss://` (TLS) address it gives you, put it in `secrets/<stage>/app.env` as `REDIS_URL`, and pick the
region closest to the API. A managed Redis that bills per command is cheap here: the service sends one
`PUBLISH` per burst of commits (never more than about ten a second) and keeps one idle subscription with a
health ping every 30 seconds. Check the plan's command allowance before you rely on a free tier.

### Connect and check

1. Set `REDIS_URL` and restart every instance.
2. Each instance logs `signals - subscribed` once. If it logs `redis is not reachable`, the address,
   password or network is wrong; the service keeps working without it.
3. Count the listeners: `redis-cli -a "$PASSWORD" PUBSUB NUMSUB streetfood-customer-api:staging:signals`
   should say as many subscribers as instances.
4. Try it: sign in on instance B (`GET /v1/me`), change the name through instance A
   (`PATCH /v1/me`), then `GET /v1/me` on B returns the new name at once. Without Redis B would keep the
   old name for up to 5 minutes.

### Security

- Never publish port 6379 to the internet. The bundled service has no `ports:` and is reachable only from
  the compose network. For a Redis on another host, use a private network or `rediss://` with a password.
- The signals carry table names, order ids and customer ids, no personal data and no prices.
- Rotate the password by editing `secrets/<stage>/redis_password` and the `REDIS_URL`, then restarting
  Redis and the API.

### Connecting the POS backend

The POS backend (`ansilum-be`) does not use Redis yet. When it does (a shared session store and wake-ups
across its instances, see [`docs/NEON_PLAN_POS_BACKEND.md`](docs/NEON_PLAN_POS_BACKEND.md), WP5 and WP8), it
can use **the same Redis server**: give it its own database number (`redis://…/1`) or key prefix, so a
flush or a change on one side never touches the other. The customer API only publishes and listens on its
own channel and sets no keys, so there is nothing to clean up.

### Troubleshooting

| Symptom | Cause |
|---|---|
| `REDIS_URL is not a valid redis:// or rediss:// address` and the service stops at start | The URL is malformed (a `#`, `@` or `/` in the password must be percent-encoded, or use a hex password) |
| `signals - redis is not reachable` | Wrong host, port, password or network; the service continues without cross-instance signals |
| `NOAUTH` or `WRONGPASS` in the Redis log | The password in `REDIS_URL` differs from the one Redis runs with |
| Other instances still see old data for minutes | Redis is not configured on every instance, or they use different Redis servers or database numbers |
| The Redis container restarts | Its memory cap (64 MB) was hit by something else using it; give the API its own Redis |

---

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

