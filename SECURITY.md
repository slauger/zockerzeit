# Security Policy

## Reporting a Vulnerability

Please report security issues privately via [GitHub Security Advisories](https://github.com/slauger/zockerzeit/security/advisories/new) or by email to simon@lauger.de. Do not open public issues for vulnerabilities.

## Deployment Notes

- Zockerzeit stores a session token of the Nintendo account and the tokens of the notification channels, encrypted with `ZOCKERZEIT_SESSION_SECRET`. Pass secrets via environment variables or a Kubernetes secret, never in `config.yaml` under version control.
- Always set `ZOCKERZEIT_ADMIN_PASSWORD`. Without it, everyone on the network can change the rules.
- Devices without a password use secret links created under Einstellungen: a parent link (`/zugang/<token>`) signs a device in, a kid link (`/k/<token>`) opens only that kid's app. Only a hash of the token is stored; each link can be revoked. Treat the links like passwords.
- Failed logins are limited per client address (5 per 15 minutes) and logged. Behind a proxy the address is taken from the last `X-Forwarded-For` entry.
- Serve the UI via HTTPS (e.g. an ingress with TLS) if it is reachable beyond the home network.
