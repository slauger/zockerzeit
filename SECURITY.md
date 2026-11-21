# Security Policy

## Reporting a Vulnerability

Please report security issues privately via [GitHub Security Advisories](https://github.com/slauger/zockerzeit/security/advisories/new) or by email to simon@lauger.de. Do not open public issues for vulnerabilities.

## Deployment Notes

- Zockerzeit stores a session token of the Nintendo account and the tokens of the notification channels, encrypted with `ZOCKERZEIT_SESSION_SECRET`. Pass secrets via environment variables or a Kubernetes secret, never in `config.yaml` under version control.
- Always set `ZOCKERZEIT_ADMIN_PASSWORD`. Without it, everyone on the network can change the rules.
- The kid app (`/kind/<profile>`) is public by design and shows the remaining time, the play time statistics and the games of that kid.
- Serve the UI via HTTPS (e.g. an ingress with TLS) if it is reachable beyond the home network.
