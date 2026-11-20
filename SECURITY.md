# Security Policy

## Reporting a Vulnerability

Please report security issues privately via [GitHub Security Advisories](https://github.com/slauger/zockerzeit/security/advisories/new) or by email to simon@lauger.de. Do not open public issues for vulnerabilities.

## Deployment Notes

- Zockerzeit holds credentials for the UniFi controller. Use a dedicated local account and pass secrets via environment variables or a Kubernetes secret, never in `config.yaml` under version control.
- Always set `ZOCKERZEIT_ADMIN_PASSWORD`. Without it, everyone on the network can change the rules.
- The kid view (`/kind/<profile>`) is public by design and shows only the remaining time.
- Serve the UI via HTTPS (e.g. an ingress with TLS) if it is reachable beyond the home network.
