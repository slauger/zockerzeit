# zockerzeit

![Version: 0.0.0](https://img.shields.io/badge/Version-0.0.0-informational?style=flat-square) ![Type: application](https://img.shields.io/badge/Type-application-informational?style=flat-square) ![AppVersion: 0.0.0](https://img.shields.io/badge/AppVersion-0.0.0-informational?style=flat-square)
Play time dashboard for parents based on the Nintendo Switch parental controls

## Overview

Deploys Zockerzeit, a play time dashboard for parents. It fetches the play time per console and game from the Nintendo Switch parental controls, counts it per kid against the time windows and limits, and notifies when the time is almost up.

Only a single replica is supported because the data lives in SQLite on a persistent volume.

## Installation

```bash
helm install zockerzeit oci://ghcr.io/slauger/charts/zockerzeit \
  --namespace zockerzeit --create-namespace \
  --set secret.existingSecret=zockerzeit
```

The secret must contain `ZOCKERZEIT_ADMIN_PASSWORD` and `ZOCKERZEIT_SESSION_SECRET`, optionally `ZOCKERZEIT_NTFY_TOKEN`.

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
| config.notify.ntfyURL | string | `""` | ntfy topic URL for push notifications (optional) |
| config.timezone | string | `"Europe/Berlin"` | Time zone used for schedules and statistics |
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
| secret.sessionSecret | string | `""` | Signs session cookies and encrypts the Nintendo tokens and notification channels. Required for Nintendo; keep it stable |
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
