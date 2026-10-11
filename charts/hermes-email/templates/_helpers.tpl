{{- define "hermes-email.name" -}}
{{- default .Chart.Name .Values.nameOverride | trunc 63 | trimSuffix "-" -}}
{{- end -}}

{{- define "hermes-email.fullname" -}}
{{- if .Values.fullnameOverride -}}
{{- .Values.fullnameOverride | trunc 63 | trimSuffix "-" -}}
{{- else if contains (include "hermes-email.name" .) .Release.Name -}}
{{- .Release.Name | trunc 63 | trimSuffix "-" -}}
{{- else -}}
{{- printf "%s-%s" .Release.Name (include "hermes-email.name" .) | trunc 63 | trimSuffix "-" -}}
{{- end -}}
{{- end -}}

{{- define "hermes-email.labels" -}}
helm.sh/chart: {{ printf "%s-%s" .Chart.Name .Chart.Version | replace "+" "_" }}
app.kubernetes.io/name: {{ include "hermes-email.name" . }}
app.kubernetes.io/instance: {{ .Release.Name }}
app.kubernetes.io/version: {{ .Chart.AppVersion | quote }}
app.kubernetes.io/managed-by: {{ .Release.Service }}
{{- end -}}

{{- define "hermes-email.selectorLabels" -}}
app.kubernetes.io/name: {{ include "hermes-email.name" . }}
app.kubernetes.io/instance: {{ .Release.Name }}
app.kubernetes.io/component: watch
{{- end -}}

{{- define "hermes-email.image" -}}
{{- $tag := default .Chart.AppVersion .Values.image.tag -}}
{{- if .Values.image.digest -}}
{{- printf "%s@%s" .Values.image.repository .Values.image.digest -}}
{{- else -}}
{{- printf "%s:%s" .Values.image.repository $tag -}}
{{- end -}}
{{- end -}}

{{- define "hermes-email.binDir" -}}
{{- default (printf "%s/.local/bin" .Values.hermes.home) .Values.paths.binDir -}}
{{- end -}}
{{- define "hermes-email.configDir" -}}
{{- default (printf "%s/.config/email" .Values.hermes.home) .Values.paths.configDir -}}
{{- end -}}
{{- define "hermes-email.stateDir" -}}
{{- default (printf "%s/.local/state/email" .Values.hermes.home) .Values.paths.stateDir -}}
{{- end -}}
{{- define "hermes-email.policyFile" -}}
{{- printf "%s/policy.toml" (trimSuffix "/" .Values.paths.policyMount) -}}
{{- end -}}

{{/* EMAIL_* for every container that runs email */}}
{{- define "hermes-email.env" -}}
- {name: EMAIL_POLICY_FILE, value: {{ include "hermes-email.policyFile" . | quote }}}
- {name: EMAIL_CONFIG_DIR, value: {{ include "hermes-email.configDir" . | quote }}}
- {name: EMAIL_STATE_DIR, value: {{ include "hermes-email.stateDir" . | quote }}}
- {name: HOME, value: {{ .Values.hermes.home | quote }}}
{{- end -}}

{{- define "hermes-email.mounts" -}}
- {name: data, mountPath: {{ .Values.hermes.home | quote }}}
- {name: config, mountPath: {{ .Values.paths.policyMount | quote }}, readOnly: true}
{{- end -}}
