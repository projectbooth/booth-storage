{{/*
Standard name/label helpers, the same shape `helm create` scaffolds — mirrors
booth-core's and booth-module-store's own chart helpers, nothing booth-storage-specific here.
*/}}

{{- define "booth-storage.name" -}}
{{- default .Chart.Name .Values.nameOverride | trunc 63 | trimSuffix "-" -}}
{{- end -}}

{{- define "booth-storage.fullname" -}}
{{- if .Values.fullnameOverride -}}
{{- .Values.fullnameOverride | trunc 63 | trimSuffix "-" -}}
{{- else -}}
{{- $name := default .Chart.Name .Values.nameOverride -}}
{{- if contains $name .Release.Name -}}
{{- .Release.Name | trunc 63 | trimSuffix "-" -}}
{{- else -}}
{{- printf "%s-%s" .Release.Name $name | trunc 63 | trimSuffix "-" -}}
{{- end -}}
{{- end -}}
{{- end -}}

{{- define "booth-storage.labels" -}}
app.kubernetes.io/name: {{ include "booth-storage.name" . }}
app.kubernetes.io/instance: {{ .Release.Name }}
app.kubernetes.io/version: {{ .Chart.AppVersion | quote }}
app.kubernetes.io/managed-by: {{ .Release.Service }}
{{- end -}}

{{- define "booth-storage.selectorLabels" -}}
app.kubernetes.io/name: {{ include "booth-storage.name" . }}
app.kubernetes.io/instance: {{ .Release.Name }}
{{- end -}}

{{/*
The Secret holding the database connection string: core's, or the operator's own.
*/}}
{{- define "booth-storage.dsnSecretName" -}}
{{- if .Values.postgres.provisionedByCore -}}
{{- default "booth-database-credentials" .Values.postgres.dsnSecret.name -}}
{{- else -}}
{{- required "postgres.dsnSecret.name is required when postgres.provisionedByCore=false: name the Secret holding your database connection string (ADR 0014)" .Values.postgres.dsnSecret.name -}}
{{- end -}}
{{- end -}}

{{- define "booth-storage.serviceAccountName" -}}
{{- if .Values.serviceAccount.create -}}
{{- default (include "booth-storage.fullname" .) .Values.serviceAccount.name -}}
{{- else -}}
{{- default "default" .Values.serviceAccount.name -}}
{{- end -}}
{{- end -}}
