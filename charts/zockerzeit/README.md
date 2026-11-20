# zockerzeit

![Version: 0.0.0](https://img.shields.io/badge/Version-0.0.0-informational?style=flat-square) ![Type: application](https://img.shields.io/badge/Type-application-informational?style=flat-square) ![AppVersion: 0.0.0](https://img.shields.io/badge/AppVersion-0.0.0-informational?style=flat-square)
Play time tracking and limits for kids by blocking devices on a UniFi WLAN

## Overview

Deploys Zockerzeit next to a UniFi Network Application. Zockerzeit polls the controller, records play time per device and blocks devices on the WLAN when the agreed play time is used up.

Only a single replica is supported because the data lives in SQLite on a persistent volume.

## Installation

```bash
helm install zockerzeit oci://ghcr.io/slauger/charts/zockerzeit \
  --namespace zockerzeit --create-namespace \
  --set config.unifi.url=https://unifi.unifi.svc.cluster.local:8443 \
  --set secret.existingSecret=zockerzeit
```

The secret must contain `ZOCKERZEIT_UNIFI_USERNAME` and `ZOCKERZEIT_UNIFI_PASSWORD` (or `ZOCKERZEIT_UNIFI_API_KEY`), `ZOCKERZEIT_ADMIN_PASSWORD` and `ZOCKERZEIT_SESSION_SECRET`.

## Maintainers

| Name | Email | Url |
| ---- | ------ | --- |
| Simon Lauger | <simon@lauger.de> | <https://lauger.de> |
## Source Code

* <https://github.com/slauger/zockerzeit>
## Requirements

Kubernetes: `>=1.22.0-0`

## Values

| Key | Type | Default | Description |
|-----|------|---------|-------------|
| affinity | object | `{}` | Affinity and anti-affinity rules |
| config.dryRun | bool | `true` | Test mode: record and log, but never block. Disable once the history looks right. |
| config.minBytesPerMinute | int | `150000` | Traffic threshold per minute for devices in traffic mode (phones) |
| config.notify.ntfyURL | string | `""` | ntfy topic URL for push notifications (optional) |
| config.pollInterval | string | `"60s"` | How often the UniFi controller is polled |
| config.retentionDays | int | `400` | Days of usage history to keep |
| config.timezone | string | `"Europe/Berlin"` | Time zone used for schedules and statistics |
| config.unifi.insecureSkipVerify | bool | `true` | Skip TLS verification (self-signed controller certificate) |
| config.unifi.site | string | `"default"` | UniFi site name |
| config.unifi.unifiOS | bool | `false` | Set to true for UniFi OS consoles (UDM, UCG, Cloud Key Gen2+) |
| config.unifi.url | string | `""` | URL of the UniFi Network Application, e.g. https://unifi.unifi.svc.cluster.local:8443 |
| config.warnMinutes | list | `[10,5]` | Remaining minutes at which a push warning is sent |
| extraContainers | list | `[]` | Additional sidecar containers |
| extraEnv | object | `{}` | Additional environment variables as key-value pairs |
| extraEnvSecret | string | `""` | Existing secret to use as additional environment variables |
| extraInitContainers | list | `[]` | Additional init containers |
| extraVolumeMounts | list | `[]` | Additional volume mounts |
| extraVolumes | list | `[]` | Additional volumes |
| fullnameOverride | string | `""` | Override the fully qualified app name |
| image.pullPolicy | string | `"IfNotPresent"` | Image pull policy |
| image.repository | string | `"ghcr.io/slauger/zockerzeit"` | Container image repository |
| image.tag | string | `""` | Container image tag (defaults to the chart appVersion) |
| imagePullSecrets | list | `[]` | Image pull secrets for private registries |
| ingress.annotations | object | `{}` | Ingress annotations |
| ingress.className | string | `""` | Ingress class name |
| ingress.enabled | bool | `false` | Enable an ingress |
| ingress.hosts | list | `[{"host":"zockerzeit.local","paths":[{"path":"/","pathType":"Prefix"}]}]` | Ingress hosts |
| ingress.tls | list | `[]` | Ingress TLS configuration |
| livenessProbe | object | `{"httpGet":{"path":"/healthz","port":"http"},"periodSeconds":30}` | Liveness probe |
| nameOverride | string | `""` | Override the chart name |
| nodeSelector | object | `{}` | Node selector for pod assignment |
| persistence.accessModes | list | `["ReadWriteOnce"]` | Access modes of the volume |
| persistence.enabled | bool | `true` | Store the SQLite database on a persistent volume |
| persistence.existingClaim | string | `""` | Use an existing PersistentVolumeClaim |
| persistence.size | string | `"1Gi"` | Size of the volume |
| persistence.storageClass | string | `""` | Storage class (empty uses the cluster default) |
| podAnnotations | object | `{}` | Additional pod annotations |
| podDisruptionBudget.enabled | bool | `false` | Enable a PodDisruptionBudget |
| podDisruptionBudget.maxUnavailable | int | `1` | Maximum unavailable pods |
| podDisruptionBudget.minAvailable | string | `""` | Minimum available pods |
| podLabels | object | `{}` | Additional pod labels |
| podSecurityContext | object | `{"fsGroup":1001,"runAsGroup":1001,"runAsNonRoot":true,"runAsUser":1001,"seccompProfile":{"type":"RuntimeDefault"}}` | Pod security context |
| priorityClassName | string | `""` | Priority class name |
| readinessProbe | object | `{"httpGet":{"path":"/readyz","port":"http"},"periodSeconds":10}` | Readiness probe |
| replicaCount | int | `1` | Number of replicas. Zockerzeit uses SQLite, so only 1 is supported. |
| resources | object | `{"limits":{"memory":"128Mi"},"requests":{"cpu":"10m","memory":"32Mi"}}` | Container resource requests and limits |
| secret.adminPassword | string | `""` | Password for the parent web UI |
| secret.existingSecret | string | `""` | Name of an existing secret with the keys listed above |
| secret.ntfyToken | string | `""` | ntfy access token (optional) |
| secret.sessionSecret | string | `""` | Secret used to sign session cookies (random if empty, sessions then end on restart) |
| secret.unifiApiKey | string | `""` | UniFi API key (alternative to username/password) |
| secret.unifiPassword | string | `""` | UniFi password |
| secret.unifiUsername | string | `""` | UniFi username (local controller account) |
| securityContext | object | `{"allowPrivilegeEscalation":false,"capabilities":{"drop":["ALL"]},"readOnlyRootFilesystem":true}` | Container security context |
| service.annotations | object | `{}` | Service annotations |
| service.labels | object | `{}` | Service labels |
| service.port | int | `8080` | Service port |
| service.type | string | `"ClusterIP"` | Service type |
| serviceAccount.annotations | object | `{}` | Service account annotations |
| serviceAccount.create | bool | `true` | Create a service account |
| serviceAccount.name | string | `""` | Service account name (generated if empty) |
| startupProbe | object | `{"failureThreshold":30,"httpGet":{"path":"/healthz","port":"http"},"periodSeconds":2}` | Startup probe |
| terminationGracePeriodSeconds | int | `30` | Termination grace period in seconds |
| tolerations | list | `[]` | Tolerations for pod assignment |
| topologySpreadConstraints | list | `[]` | Topology spread constraints |
| updateStrategy | object | `{"type":"Recreate"}` | Update strategy. Recreate avoids two pods writing the same SQLite file. |

----------------------------------------------
Autogenerated from chart metadata using [helm-docs v1.14.2](https://github.com/norwoodj/helm-docs/releases/v1.14.2)
