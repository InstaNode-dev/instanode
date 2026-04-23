# Contributing

Thanks for the interest. This project is early and small — the fastest way to
help is to file a bug with a reproducer, or open a PR that's scoped tightly.

## Development setup

Prereqs: Go 1.25+, a Postgres you can create databases on, Redis (optional —
the server degrades without it).

```bash
git clone https://github.com/InstaNode-dev/instant-lite-api
cd instant-lite-api
cp config.example.yaml config.yaml   # edit the DB urls
make run
```

Or with Docker:

```bash
make docker-up
curl -s -X POST http://localhost:18080/db/new | jq
```

## Running the test suite

```bash
make test     # unit tests, no external services needed
make vet      # go vet + gofmt check
```

Unit tests stub out Razorpay via the `razorpayBaseURLOverride` pointer in
`internal/server/razorpay_client.go` — no test ever makes a real network call.

## Code layout

```
cmd/server/            entrypoint — thin main() that calls server.Run
internal/server/       everything else (handlers, auth, billing, config, db)
  paths.go             route-path constants (reuse these, don't hardcode)
  payment.go           billing-provider interface
  razorpay_client.go   razorpayPayment impl + SDK helpers
  payment_noop.go      no-op impl for self-hosts without Razorpay
  billing.go           shared billing helpers + deprecated migrate shim
  billing_orders.go    legacy one-time order flow
  billing_webhook.go   Razorpay webhook dispatcher + signature verify
  billing_subscriptions.go  subscription lifecycle handlers
  billing_change_plan.go    monthly↔annual plan switch
  billing_reconciler.go     background reconciler (polls Razorpay)
  ...
```

## PR conventions

- **Branch off master**, don't push to master directly (there's a pre-push
  hook blocking this — enable with `git config core.hooksPath .githooks`).
- **One concern per PR**. Splitting a file, a bug fix, and a feature into
  three PRs gets reviewed in three hours. Bundling them gets reviewed in
  three weeks.
- **Commit messages**: `<scope>: <what changed>` (e.g. `billing: fix INR
  rounding in receipt email`). Explain the *why* in the body when it's
  non-obvious.
- **Tests**: add them when you add behaviour. Tests that hit a real
  `httptest.Server` for external APIs, not mocks of our own types.
- **No `--no-verify` pushes**, no `gofmt` violations, no unused imports.

## Code style

- Follow `gofmt` — CI will fail otherwise.
- Prefer named constants over repeated string literals (paths, error codes,
  header names). See `internal/server/paths.go` for the pattern.
- Errors bubble up; don't swallow. Log with `slog.ErrorContext` when the
  request context is available so traces correlate.
- Return structured JSON errors via `writeError(w, status, code, message)` —
  don't hand-write error bodies.

## What we're NOT looking for

- Abstractions-for-future-flexibility without a concrete second caller today.
- Rewrites of working code to match a different style.
- AI-generated PR descriptions or commit messages — write your own so we
  can review intent, not output.

## Getting unstuck

Open an issue with the full command you ran and the full error output. We
don't need a stack trace dump of your entire server — just the failing
request + response.
