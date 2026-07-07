{{- define "fluctio.fullname" -}}
{{- .Release.Name | trunc 63 | trimSuffix "-" -}}
{{- end -}}

{{- define "fluctio.labels" -}}
app.kubernetes.io/name: fluctio
app.kubernetes.io/instance: {{ .Release.Name }}
app.kubernetes.io/version: {{ .Chart.AppVersion | quote }}
{{- end -}}

{{- /* DSN: prefer externalDSN, else fall back to bundled postgres. */ -}}
{{- define "fluctio.dsn" -}}
{{- if .Values.externalDSN -}}
{{ .Values.externalDSN }}
{{- else if .Values.postgres.enabled -}}
postgres://fluctio:{{ required "postgres.password is required when postgres.enabled=true" .Values.postgres.password }}@{{ include "fluctio.fullname" . }}-db:5432/fluctio?sslmode=disable
{{- else -}}
{{- fail "Either externalDSN or postgres.enabled must be set" -}}
{{- end -}}
{{- end -}}
