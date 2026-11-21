# zockerzeit

Go web app that tracks play time of kids from the Nintendo parental controls, sees their devices in a UniFi WLAN and can optionally block them (UniFi or FRITZ!Box). Deployed via the Helm chart in `charts/zockerzeit` on k3s next to a self-hosted UniFi Network Application.

## Layout

- `cmd/zockerzeit` - entry point, flags `-config`, `-demo`, `-version`
- `internal/rules` - time windows, quotas, overrides (pure logic)
- `internal/engine` - poll loop: UniFi -> samples -> rules -> block/unblock
- `internal/unifi` - minimal UniFi Network API client (classic and UniFi OS paths, session or API key)
- `internal/fritzbox` - TR-064 client for internet-only blocking
- `internal/nintendo` - Nintendo parental controls client, syncer and play time attribution (console owner or "ist jetzt bei")
- `internal/notify` - notification hub with ntfy and Telegram channels
- `internal/secret` - AES-GCM box for tokens stored in the database
- `internal/store` - SQLite (modernc, no CGO), schema migrations in `migrations`
- `internal/stats` - aggregation for the charts
- `internal/web` - parent UI, kid view, JSON API; templates and static files are embedded
- `internal/demo` - simulated controller and sample data for `make demo`

## Conventions

- UI texts are German, code, comments and docs English.
- New schema changes are appended to `migrations` in `internal/store/store.go`, never edited in place.
- `dryRun` defaults to true; nothing may block devices that are not assigned to a profile.
- Console budgets count Nintendo play time; WLAN time is presence only. Budgets never block, only time windows of devices with schedule blocking.
- Pages load no external resources (CSP `img-src 'self'`, `Referrer-Policy: no-referrer`); external images are copied into the database.
- `make ci` must pass. Coverage gate is 80% (cmd and demo excluded).
- Chart README is generated with `make helm-docs`.
