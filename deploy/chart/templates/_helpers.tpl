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

  IT REFUSES TO RENDER 8080 FOR HOSTED WORKERS, and that is the whole point of the
  guard below. This helper is only ever reached when workers.enabled, and the two
  flags have no coupling: they are separate values blocks, and M6's job is to flip
  workers.enabled on a cluster where api.tls.enabled defaults false. That
  combination WORKS PERFECTLY AND IS SILENTLY INSECURE — no error, no failed probe,
  nothing to notice — because port 8080 serves the FULL router with NO stripXFF
  (cmd/server/main.go puts both layers on the TLS listener only). A worker admitted
  there gets back /api/auth/* and /api/admin/*, and its pod IP sits inside the
  cluster's TRUSTED_PROXIES (the pod CIDR), so it can forge X-Forwarded-For and
  defeat the login rate limit — the exact bypass measured at 12x401/zero-429s. It
  also contradicts Decision 5(a) verbatim ("to the TLS port only") and puts the
  decrypted PAT on the pod network in the clear, which is Decision 4's whole reason
  for existing.

  Prose in values.yaml cannot hold this: "turn it on together with hosting" is
  guidance, and guidance is true by bookkeeping rather than by construction — the
  same failure mode this PRD already rejected twice (narrowing TRUSTED_PROXIES, and
  the CIDR-vs-FQDN allowlist). So it is a template error instead.

  There is NO legitimate k8s configuration that wants plaintext here: a cluster
  without cert-manager still sets api.tls.enabled: true and supplies a pre-created
  api.tls.secretName. "TLS off + hosted workers on" is only ever a mistake — except
  on KinD, which has no cert-manager at all and is a TEST target, never a deploy
  one. That deviation gets an explicit opt-in (workers.allowPlaintextAPI) rather
  than a silent default, so it is visible in the values file that chose it.
*/ -}}
{{- define "uzi.workerAPIPort" -}}
{{- if .Values.api.tls.enabled -}}
{{- .Values.api.tls.port -}}
{{- else if .Values.workers.allowPlaintextAPI -}}
8080
{{- else -}}
{{- fail "workers.enabled is true but api.tls.enabled is false: hosted workers would be admitted to the api's PLAINTEXT port 8080, which serves the full router (including /api/auth/* and /api/admin/*) and does NOT strip X-Forwarded-For. A worker pod's IP is inside TRUSTED_PROXIES, so it could forge its rate-limit key and defeat the login brute-force control, and its claim traffic — carrying the user's decrypted forge PAT and Anthropic token — would cross the pod network in the clear. Set api.tls.enabled: true (with api.tls.secretName if you have no cert-manager). If you genuinely intend plaintext — a throwaway test cluster, never a deployment — set workers.allowPlaintextAPI: true to say so out loud." -}}
{{- end -}}
{{- end -}}

{{- /*
  uzi.apiInClusterURL: the base URL the CONTROLLER and the HOSTED WORKERS dial
  (PRD #58 M6). Always the FQDN, and always the RELEASE namespace.

  Both halves are the easy things to get wrong, and both fail obscurely:
    * a SHORT name (api:8443) resolves for the controller (same namespace) and NOT
      for a worker (another namespace) — so it would work in every render you look
      at and break only the pods you cannot see;
    * the WORKER's namespace in the name would be a name the certificate never
      carried (api-certificate.yaml templates its SANs off .Release.Namespace), so
      it fails as an opaque TLS verification error rather than a DNS one.
  One helper for both clients means there is one thing to get right.

  The scheme follows api.tls.enabled, and the port comes from uzi.workerAPIPort —
  so the plaintext guard above governs this URL too, and http is only ever
  reachable through the explicit workers.allowPlaintextAPI opt-in.
*/ -}}
{{- define "uzi.apiInClusterURL" -}}
{{- $scheme := ternary "https" "http" .Values.api.tls.enabled -}}
{{- printf "%s://%s.%s.svc.%s:%v" $scheme (include "uzi.apiServiceName" .) .Release.Namespace .Values.api.tls.clusterDomain (include "uzi.workerAPIPort" .) -}}
{{- end -}}

{{- /*
  Where the controller's own two mounted files live. It reads PATHS
  (UZI_CONTROLLER_TOKEN_FILE / UZI_API_CA_FILE) and never takes either as an env
  var, so these are the one place the layout is decided.

  The token is file-mounted rather than env-injected on purpose: an env-borne
  secret is readable through /proc/<pid>/environ, the leak class
  docs/proc-hardening.md closed for the worker. The controller's config has no
  env fallback to be tempted by.
*/ -}}
{{- define "uzi.controllerTokenDir" -}}
/etc/uzi/controller
{{- end -}}

{{- define "uzi.controllerCADir" -}}
/etc/uzi/ca
{{- end -}}

{{- /*
  uzi.apiHostingEnabled: whether the API turns its hosted-worker surface on
  (WORKER_HOSTING_ENABLED — the provision endpoints, the quota setting, the UI's
  provision card). Non-empty = true, per Helm's truthiness.

  It follows the CONTROLLER, not workers.enabled, and that is the whole point of the
  helper. `workers.enabled` alone means the envelope exists; without a controller
  nothing materializes a pod, so hosting-on would let users provision workers that
  never appear — a row pending until its token expires, surfacing only as a worker
  that never comes online (Decision 10), with the cause invisible.

  The api and the controller therefore switch on together, from one place.
*/ -}}
{{- define "uzi.apiHostingEnabled" -}}
{{- if and .Values.workers.enabled .Values.workers.controller.enabled -}}
true
{{- end -}}
{{- end -}}
