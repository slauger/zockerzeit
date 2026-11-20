{{/* Chart name. */}}
{{- define "zockerzeit.name" -}}
{{- default .Chart.Name .Values.nameOverride | trunc 63 | trimSuffix "-" }}
{{- end }}

{{/* Fully qualified app name. */}}
{{- define "zockerzeit.fullname" -}}
{{- if .Values.fullnameOverride }}
{{- .Values.fullnameOverride | trunc 63 | trimSuffix "-" }}
{{- else }}
{{- $name := default .Chart.Name .Values.nameOverride }}
{{- if contains $name .Release.Name }}
{{- .Release.Name | trunc 63 | trimSuffix "-" }}
{{- else }}
{{- printf "%s-%s" .Release.Name $name | trunc 63 | trimSuffix "-" }}
{{- end }}
{{- end }}
{{- end }}

{{- define "zockerzeit.chart" -}}
{{- printf "%s-%s" .Chart.Name .Chart.Version | replace "+" "_" | trunc 63 | trimSuffix "-" }}
{{- end }}

{{- define "zockerzeit.labels" -}}
helm.sh/chart: {{ include "zockerzeit.chart" . }}
{{ include "zockerzeit.selectorLabels" . }}
app.kubernetes.io/version: {{ .Chart.AppVersion | quote }}
app.kubernetes.io/managed-by: {{ .Release.Service }}
{{- end }}

{{- define "zockerzeit.selectorLabels" -}}
app.kubernetes.io/name: {{ include "zockerzeit.name" . }}
app.kubernetes.io/instance: {{ .Release.Name }}
{{- end }}

{{- define "zockerzeit.serviceAccountName" -}}
{{- if .Values.serviceAccount.create }}
{{- default (include "zockerzeit.fullname" .) .Values.serviceAccount.name }}
{{- else }}
{{- default "default" .Values.serviceAccount.name }}
{{- end }}
{{- end }}

{{- define "zockerzeit.secretName" -}}
{{- default (include "zockerzeit.fullname" .) .Values.secret.existingSecret }}
{{- end }}

{{- define "zockerzeit.pvcName" -}}
{{- default (include "zockerzeit.fullname" .) .Values.persistence.existingClaim }}
{{- end }}
