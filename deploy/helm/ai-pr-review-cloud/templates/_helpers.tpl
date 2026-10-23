{{- define "aprc.fullname" -}}
{{- printf "%s" .Release.Name | trunc 63 | trimSuffix "-" -}}
{{- end -}}

{{- define "aprc.labels" -}}
app.kubernetes.io/name: ai-pr-review-cloud
app.kubernetes.io/instance: {{ .Release.Name }}
app.kubernetes.io/version: {{ .Chart.AppVersion | quote }}
app.kubernetes.io/managed-by: {{ .Release.Service }}
{{- end -}}

{{- define "aprc.selector" -}}
app.kubernetes.io/name: ai-pr-review-cloud
app.kubernetes.io/instance: {{ .root.Release.Name }}
app.kubernetes.io/component: {{ .component }}
{{- end -}}

{{- define "aprc.image" -}}
{{ .root.Values.image.registry }}/ai-pr-review-cloud-{{ .component }}:{{ .root.Values.image.tag }}
{{- end -}}

{{- define "aprc.githubSecret" -}}
{{- .Values.github.existingSecret | default (printf "%s-github" (include "aprc.fullname" .)) -}}
{{- end -}}

{{- define "aprc.llmSecret" -}}
{{- .Values.llm.existingSecret | default (printf "%s-llm" (include "aprc.fullname" .)) -}}
{{- end -}}

{{- define "aprc.databaseSecret" -}}
{{- if .Values.postgres.dev.enabled -}}
{{ include "aprc.fullname" . }}-database
{{- else -}}
{{ required "database.existingSecret is required when postgres.dev.enabled is false" .Values.database.existingSecret }}
{{- end -}}
{{- end -}}

{{- define "aprc.podSecurityContext" -}}
runAsNonRoot: true
seccompProfile:
  type: RuntimeDefault
{{- end -}}

{{- define "aprc.containerSecurityContext" -}}
allowPrivilegeEscalation: false
readOnlyRootFilesystem: true
capabilities:
  drop: ["ALL"]
{{- end -}}
