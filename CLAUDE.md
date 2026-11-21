# zockerzeit

Go web app that shows the play time of kids from the Nintendo Switch parental controls against time windows and limits per kid, and sends notifications. It does not block anything; enforcement on the console is set in the Nintendo app. Deployed via the Helm chart in `charts/zockerzeit` on k3s.

## Layout

- `cmd/zockerzeit` - entry point, flags `-config`, `-demo`, `-version`
- `internal/rules` - time windows, quotas, overrides (pure logic)
- `internal/engine` - evaluation loop: Nintendo play time -> rules -> warnings and "time is up" notifications
- `internal/nintendo` - Nintendo parental controls client, syncer and play time attribution (console owner or "ist jetzt bei")
- `internal/notify` - notification hub with ntfy and Telegram channels
- `internal/secret` - AES-GCM box for tokens stored in the database
- `internal/store` - SQLite (modernc, no CGO), schema migrations in `migrations`
- `internal/stats` - aggregation for the charts
- `internal/web` - parent UI, kid app, JSON API; templates and static files are embedded
- `internal/demo` - sample profiles and Nintendo data for `make demo`

## Conventions

- UI texts are German, code, comments and docs English.
- New schema changes are appended to `migrations` in `internal/store/store.go`, never edited in place.
- Every console (`nintendo_devices.profile_id`) belongs to one kid; its play time counts for that kid, or for the kid it is lent to (`release` override). Changing the owner attributes the console's play time again.
- Pages load no external resources (CSP `img-src 'self'`, `Referrer-Policy: no-referrer`); external images are copied into the database.
- No real names of the kids in code, tests, docs or screenshots; use the demo kids Max and Mia.
- `make ci` must pass. Coverage gate is 80% (cmd and demo excluded).
- Chart README is generated with `make helm-docs`.
