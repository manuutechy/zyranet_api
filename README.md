# Zyra Net API

Backend for the Zyra Net MikroTik Hotspot & PPPoE billing platform. Built with **Go**, the **Fiber** web framework, **GORM**/MySQL, and integrates with **MikroTik RouterOS**, **Safaricom M-Pesa (Daraja)**, and SMS providers (HostPinnacle / Africa's Talking).

It is the single backend serving all three frontends in this repo: `customer` (captive portal / self-service), `zyranet-admin` (admin dashboard), and `website` (public marketing site, settings only).

---

## Tech Stack

- **Go 1.22**
- **Fiber v2** (HTTP framework)
- **GORM** with the MySQL driver (SQLite driver also vendored, unused by default)
- **JWT** (`golang-jwt/jwt`) for both admin and customer auth, delivered via httpOnly cookies
- **Sentry** for error monitoring
- **go-routeros** for MikroTik RouterOS API calls (hotspot/PPPoE provisioning, live session stats)

## Requirements

- Go 1.22+
- A MySQL database
- (Optional for full functionality) Safaricom Daraja sandbox/production credentials, an SMS provider account, and a Sentry DSN

## Setup

```bash
cp .env.example .env   # fill in DB credentials at minimum
./setup.sh              # runs `go mod tidy`
```

Then run the server:

```bash
go run main.go
```

On startup the app loads config, connects to MySQL, and runs `AutoMigrate` for all models — no separate migration step is needed for local dev. It listens on `APP_PORT` (default `8080`) and exposes a health check at `GET /health`.

### Environment variables

See `.env.example` for the full list. Key groups:

- **App** — `APP_ENV` (`local` disables CORS lockdown and payment-endpoint rate limiting), `APP_PORT`
- **Database** — `DB_HOST`, `DB_PORT`, `DB_NAME`, `DB_USER`, `DB_PASS`
- **JWT** — `JWT_SECRET`, `JWT_EXPIRY`, `COOKIE_DOMAIN` (shared cookie domain across subdomains in production)
- **M-Pesa** — `MPESA_ENV`, `MPESA_CONSUMER_KEY`, `MPESA_CONSUMER_SECRET`, `MPESA_SHORTCODE`, `MPESA_PASSKEY`, `MPESA_CALLBACK_URL`
- **SMS** — `SMS_PROVIDER` (`hostpinnacle` or `africastalking`) plus the matching provider credentials
- **Sentry** — `SENTRY_DSN` (blank disables reporting)

## Project Structure

```
config/      App config loading + database connection
handlers/    HTTP handlers, one file per resource (zones, packages, vouchers, payments, ...)
middleware/  JWT auth (admin + customer), RBAC, Sentry
models/      GORM models, auto-migrated on boot
routes/      Single Register(app) call wiring all routes
services/    Business logic: M-Pesa STK push, SMS sending, voucher generation, MikroTik scripting/API
utils/       Shared helpers (e.g. phone number normalization)
```

## API Overview

All routes are mounted under `/api/v1`. Three auth tiers:

- **Public** — settings/packages lookup, OTP request/verify, M-Pesa STK push + callback, voucher redemption, hotspot pay/status/session/logout
- **Customer** (JWT cookie via `/customer/auth/*`) — profile, payments, voucher redemption, reconnect, tickets, top-up
- **Admin** (JWT cookie via `/auth/login`, role-gated with `middleware.RequireRoles`) — zones, packages, vouchers, customers, payments, reports, settings, users, tickets

See `routes/routes.go` for the full route table.

## Testing

Run the existing Go tests with:

```bash
go test ./...
```

Coverage is currently limited to `utils/phone_test.go` and `services/mpesa_test.go` — most handlers don't have tests yet.

## Deployment

A multi-stage `Dockerfile` is included (Go build stage → Alpine runtime, listens on `8080`).
