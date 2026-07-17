{{- define "uzi.name" -}}
{{- default .Chart.Name .Values.nameOverride | trunc 63 | trimSuffix "-" -}}
{{- end -}}

{{- define "uzi.fullname" -}}
{{- if .Values.fullnameOverride -}}
{{- .Values.fullnameOverride | trunc 63 | trimSuffix "-" -}}
{{- else -}}
{{- include "uzi.name" . | trunc 63 | trimSuffix "-" -}}
{{- end -}}
{{- end -}}

{{- define "uzi.labels" -}}
app.kubernetes.io/name: {{ include "uzi.name" . }}
app.kubernetes.io/instance: {{ .Release.Name }}
app.kubernetes.io/managed-by: {{ .Release.Service }}
{{- if .Chart.AppVersion }}
app.kubernetes.io/version: {{ .Chart.AppVersion | quote }}
{{- end }}
helm.sh/chart: {{ printf "%s-%s" .Chart.Name .Chart.Version | replace "+" "_" | trunc 63 | trimSuffix "-" }}
{{- end -}}

{{- define "uzi.selectorLabels" -}}
app.kubernetes.io/name: {{ include "uzi.name" . }}
app.kubernetes.io/instance: {{ .Release.Name }}
{{- end -}}

{{- /*
  uzi.apiServiceName: the in-cluster name of the api Service. LOAD-BEARING: the web
  nginx reverse-proxies `/api/*` to this exact name (same-origin, no CORS), so it
  MUST resolve to the api pods in the release namespace. Defaults to "api" (what the
  compose service is called), overridable via api.service.name.
*/ -}}
{{- define "uzi.apiServiceName" -}}
{{- default "api" .Values.api.service.name -}}
{{- end -}}

{{- /*
  uzi.apiTLSSecretName: the Secret holding the api's TLS pair (PRD #58 Decision 4).
  Written by the cert-manager Certificate, mounted by the api (tls.crt + tls.key)
  and — by ca.crt alone — by everything that dials the api's TLS listener. Defaults
  to <fullname>-api-tls; api.tls.secretName points it at a pre-created Secret when
  cert-manager is not doing the issuing.
*/ -}}
{{- define "uzi.apiTLSSecretName" -}}
{{- default (printf "%s-api-tls" (include "uzi.fullname" .)) .Values.api.tls.secretName -}}
{{- end -}}

{{- /*
  uzi.apiTLSDir: where the api's TLS pair is mounted. The api reads PATHS
  (API_TLS_CERT/API_TLS_KEY), never the material, so this is the one place the
  layout is decided.
*/ -}}
{{- define "uzi.apiTLSDir" -}}
/etc/uzi/tls
{{- end -}}
