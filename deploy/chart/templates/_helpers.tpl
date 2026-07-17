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
{{- if .Values.api.tls.secretName -}}
{{- .Values.api.tls.secretName -}}
{{- else if .Values.api.tls.certManager.enabled -}}
{{- printf "%s-api-tls" (include "uzi.fullname" .) -}}
{{- else -}}
{{- /*
  cert-manager off AND no secretName: nothing creates the Secret, so defaulting the
  name would render a pod mounting a Secret that does not exist — it templates fine,
  installs fine, and then hangs in ContainerCreating with nothing saying why. Fail at
  TEMPLATE time instead, where the message can name the fix.
*/ -}}
{{- required "api.tls.secretName is required when api.tls.certManager.enabled is false: with cert-manager off the chart creates no Certificate, so you must point this at a PRE-CREATED Secret holding tls.crt + tls.key (and ca.crt for the clients). Otherwise the api pod mounts a Secret nobody creates and hangs in ContainerCreating." .Values.api.tls.secretName -}}
{{- end -}}
{{- end -}}

{{- /*
  uzi.apiTLSDir: where the api's TLS pair is mounted. The api reads PATHS
  (API_TLS_CERT/API_TLS_KEY), never the material, so this is the one place the
  layout is decided.
*/ -}}
{{- define "uzi.apiTLSDir" -}}
/etc/uzi/tls
{{- end -}}

{{- /*
  uzi.workerAPIPort: the api port the CONTROLLER and the HOSTED WORKERS dial
  (PRD #58). They reach the api directly, with no nginx in the path, and a claim
  response carries the user's DECRYPTED forge PAT and Anthropic token — so once
  api.tls.enabled is on, that is the TLS listener and nothing else.

  Selected rather than hardcoded so the NetworkPolicies are correct BOTH before and
  after M4's TLS rollout: 8080 while TLS is off (the same plain port the existing api
  policy admits web on), api.tls.port once it is on. M4 deliberately made the two
  listeners separate ports precisely so a policy could admit the worker namespace to
  one and not the other.
*/ -}}
{{- define "uzi.workerAPIPort" -}}
{{- if .Values.api.tls.enabled -}}
{{- .Values.api.tls.port -}}
{{- else -}}
8080
{{- end -}}
{{- end -}}
