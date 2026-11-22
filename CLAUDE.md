# zockerzeit

Go web app for the Nintendo Switch parental controls: kids follow plans (window and limit per weekday) that Zockerzeit writes to their consoles, it reads back play time per console and game, and sends notifications. Deployed via the Helm chart in `charts/zockerzeit` on k3s.

## Layout

- `cmd/zockerzeit` - entry point, flags `-config`, `-demo`, `-version`
- `internal/rules` - time windows, quotas, overrides (pure logic)
- `internal/engine` - evaluation loop: Nintendo play time -> rules -> warnings and "time is up" notifications
- `internal/nintendo` - Nintendo parental controls client, syncer, play time attribution to the console owner, and writing plans and extra time to the consoles
- `internal/notify` - shared Telegram bot with one recipient per parent (events per recipient, connect via /start code), optional static ntfy topic
- `internal/secret` - AES-GCM box for tokens stored in the database
- `internal/store` - SQLite (modernc, no CGO), schema migrations in `migrations`
- `internal/stats` - aggregation for the charts
- `internal/web` - parent UI, kid app, JSON API; templates and static files are embedded
- `internal/demo` - sample profiles and Nintendo data for `make demo`

## Conventions

- UI texts are German, code, comments and docs English.
- New schema changes are appended to `migrations` in `internal/store/store.go`, never edited in place.
- Every console (`nintendo_devices.profile_id`) belongs to one kid; its play time counts for that kid and it gets the kid's plan in force (`store.ActivePlan`: newest `plan` override, else the standard plan). `nintendo.Syncer.Apply` writes it when the fingerprint changed and checks for drift every sync. Changing the owner attributes the console's play time again.
- Plans must fit the Nintendo app: one window per day, start 05:00-09:00, end 16:00-23:00 or 24:00, limit 0-360 (0 = no play, -1 = no limit). Frei and Gesperrt are built-in plans.
- Pages load no external resources (CSP `img-src 'self'`, `Referrer-Policy: no-referrer`); external images are copied into the database.
- No real names of the kids in code, tests, docs or screenshots; use the demo kids Max and Mia.
- `make ci` must pass. Coverage gate is 80% (cmd and demo excluded).
- Chart README is generated with `make helm-docs`.
