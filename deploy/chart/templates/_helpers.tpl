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
  Database backend selection (database.mode: simple | cnpg | external).
  All three converge on ONE seam: the api reads DATABASE_URL from a Secret via the
  two helpers below, so api-deployment.yaml is identical across modes and only the
  Secret's producer changes.
    simple   -> the chart's own <fullname>-postgres Secret, key `uri`
    cnpg     -> the CNPG-generated api.database.secretName, key api.database.secretKey
    external -> the user-supplied api.database.secretName / secretKey
*/ -}}
{{- define "uzi.database.simple.name" -}}
{{- printf "%s-postgres" (include "uzi.fullname" .) -}}
{{- end -}}

{{- define "uzi.database.secretName" -}}
{{- if eq .Values.database.mode "simple" -}}
{{- include "uzi.database.simple.name" . -}}
{{- else -}}
{{- .Values.api.database.secretName -}}
{{- end -}}
{{- end -}}

{{- define "uzi.database.secretKey" -}}
{{- if eq .Values.database.mode "simple" -}}uri{{- else -}}{{ .Values.api.database.secretKey }}{{- end -}}
{{- end -}}

{{- /*
  uzi.database.simple.password: the bundled-postgres password. Explicit override wins;
  otherwise reuse the value already stored in the applied Secret (a bare `helm upgrade`
  never rotates it); otherwise generate a fresh URL-safe 32-char password. NOTE: lookup
  returns empty under `helm template` / server-side dry-run, which is fine — the simple
  mode is for `helm install` (the CNPG path serves the ArgoCD/GitOps installs).
*/ -}}
{{- define "uzi.database.simple.password" -}}
{{- if .Values.database.simple.password -}}
{{- .Values.database.simple.password -}}
{{- else -}}
{{- $existing := lookup "v1" "Secret" .Release.Namespace (include "uzi.database.simple.name" .) -}}
{{- if and $existing (hasKey ($existing.data | default dict) "POSTGRES_PASSWORD") -}}
{{- index $existing.data "POSTGRES_PASSWORD" | b64dec -}}
{{- else -}}
{{- randAlphaNum 32 -}}
{{- end -}}
{{- end -}}
{{- end -}}

{{- /*
  uzi.validateDatabaseMode: fail fast on an invalid mode or a mode/postgres.enabled
  mismatch, so the two knobs can never silently diverge (two DBs, or none).
*/ -}}
{{- define "uzi.validateDatabaseMode" -}}
{{- $m := .Values.database.mode -}}
{{- if not (has $m (list "simple" "cnpg" "external")) -}}
{{- fail (printf "database.mode must be one of simple|cnpg|external, got %q" $m) -}}
{{- end -}}
{{- if and (eq $m "simple") .Values.postgres.enabled -}}
{{- fail "database.mode: simple bundles a plain Postgres, but postgres.enabled is true (that is the CNPG operator path). Set postgres.enabled: false, or use database.mode: cnpg." -}}
{{- end -}}
{{- if and (eq $m "cnpg") (not .Values.postgres.enabled) -}}
{{- fail "database.mode: cnpg needs the CloudNativePG subchart. Set postgres.enabled: true." -}}
{{- end -}}
{{- if and (eq $m "external") .Values.postgres.enabled -}}
{{- fail "database.mode: external expects an out-of-chart DB via api.database.secretName. Set postgres.enabled: false." -}}
{{- end -}}
{{- end -}}

{{- /*
  uzi.validateCommandSandbox: fail fast on an unrecognized workers.codex.commandSandbox
  (PRD #1493 M4). The controller and the worker both parse it as a strict allow-list
  (off|required|best-effort) and refuse to boot on anything else, so this catches a typo at
  `helm template` time — where the message can name the fix — rather than as a controller
  CrashLoop after deploy. Called unconditionally from api-deployment.yaml alongside
  uzi.validateDatabaseMode, so it fires even with workers disabled; renders nothing on a
  valid value, so a good install is byte-unchanged.
*/ -}}
{{- define "uzi.validateCommandSandbox" -}}
{{- $s := .Values.workers.codex.commandSandbox | toString -}}
{{- if not (has $s (list "off" "required" "best-effort")) -}}
{{- fail (printf "workers.codex.commandSandbox must be one of off|required|best-effort (quote \"off\": bare off is YAML false), got %q" $s) -}}
{{- end -}}
{{- end -}}

{{- /*
  uzi.validateSecretMountPath (issue #1761): workers.secretMountPath is empty (the
  /run/secrets default) or ONE lower-case directory directly under /run, the same rule
  the controller enforces at boot (kube.ValidateSecretMountPath, which also refuses an
  overlap with another worker mount). With openshift.enabled the default is refused:
  CRI-O shadows a volume at /run/secrets, so every hosted worker would crash-loop on a
  missing token. Renders nothing for a valid value.
*/ -}}
{{- define "uzi.validateSecretMountPath" -}}
{{- $p := .Values.workers.secretMountPath | default "" -}}
{{- if and $p (ne $p "/run/secrets") (not (regexMatch "^/run/[a-z0-9][a-z0-9._-]*$" $p)) -}}
{{- fail (printf "workers.secretMountPath must be a single lower-case directory directly under /run (for example /run/uzi-secrets), got %q" $p) -}}
{{- end -}}
{{- /* A relocated Secret needs a worker image whose guardrails know the new directory
  (issue #1761): an older image still starts (it reads UZI_WORKER_TOKEN_FILE) but screens
  only /run/secrets/. Keep MIN in lockstep with kube.MinRelocatableMountWorkerTag; the
  controller refuses the same pairing at boot. A non-semver tag cannot be compared and is
  refused unless workers.secretMountPathAllowUnversionedImage says the image carries it. */ -}}
{{- if and $p (ne $p "/run/secrets") .Values.workers.enabled -}}
{{- $min := "0.85.0-rc.2" -}}
{{- $tag := toString (.Values.workers.image.tag | default "") -}}
{{- if regexMatch "^v?(0|[1-9][0-9]*)\\.(0|[1-9][0-9]*)\\.(0|[1-9][0-9]*)(-[0-9A-Za-z.-]+)?(\\+[0-9A-Za-z.-]+)?$" $tag -}}
{{- if lt ((semver $tag).Compare (semver $min)) 0 -}}
{{- fail (printf "workers.secretMountPath %q needs a worker image >= %s (its guardrails must know the relocated Secret directory), but workers.image.tag is %q" $p $min $tag) -}}
{{- end -}}
{{- else if not .Values.workers.secretMountPathAllowUnversionedImage -}}
{{- fail (printf "workers.secretMountPath %q needs a worker image >= %s, but workers.image.tag %q is not a semver version; set workers.secretMountPathAllowUnversionedImage=true only if that image carries the relocated-Secret support" $p $min $tag) -}}
{{- end -}}
{{- end -}}
{{- if and .Values.workers.enabled .Values.openshift.enabled (or (not $p) (eq $p "/run/secrets")) -}}
{{- fail "openshift.enabled requires workers.secretMountPath (for example /run/uzi-secrets): CRI-O on OpenShift shadows a volume mounted at /run/secrets, so hosted workers would find no join token. The worker image must carry the matching change; see docs/openshift.md." -}}
{{- end -}}
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

{{- define "uzi.controllerFetcherCADir" -}}
/etc/uzi/fetcher-ca
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

{{- /*
  uzi.workerDinDImage: the pinned DinD sidecar image, SELECTED by posture (PRD #89
  OQ-A hybrid). workers.docker.image (an explicit override) wins; otherwise
  rootlessImage when workers.docker.rootless, else nonRootlessImage. The coherence
  fail-guard in worker-invariants.yaml rejects a posture/image mismatch, so every
  caller can treat the result as coherent with workers.docker.rootless.

  This is the single definition of the selection; controller-deployment.yaml passes it
  as UZI_WORKER_DIND_IMAGE and worker-invariants.yaml validates it, so the two never drift.
*/ -}}
{{- define "uzi.workerDinDImage" -}}
{{- $d := .Values.workers.docker -}}
{{- if $d.image -}}
{{- $d.image -}}
{{- else if $d.rootless -}}
{{- $d.rootlessImage -}}
{{- else -}}
{{- $d.nonRootlessImage -}}
{{- end -}}
{{- end -}}

{{- /*
  uzi.quantityBytes: a Kubernetes resource quantity ("20Gi", "512Mi", "10G") as a
  number of bytes, so two of them can be COMPARED. Helm has no quantity type and
  sprig has no parser for one, and the strings cannot be compared directly: "20Gi" >
  "800Gi" lexically, and "20Gi" vs "20000M" is a real inequality no string compare
  finds. Callers wrap it in `float64` (it returns a rendered string) and use `gt`/`lt`.

  🔴 IT FAILS CLOSED, AND THAT IS THE WHOLE REASON IT IS A HELPER RATHER THAN THREE
  LINES AT THE CALL SITE. The obvious version of this — regex the digits, look the
  suffix up in a table, multiply — returns 0 for anything it does not understand,
  because a missing dict key multiplies to zero. Measured while writing it: "20GB"
  (a plausible typo for 20G) rendered as 0, silently. A guard built on that would let
  every malformed quantity through as "smaller than everything", i.e. it would pass
  precisely the misconfigurations it exists to catch, and it would look like it was
  working. Both the digits and the suffix are therefore checked explicitly and a
  failure is a `fail`, not a fallback.

  Binary suffixes are powers of 1024 and decimal ones powers of 1000, per the
  Kubernetes quantity spec. Exponent notation ("2e3") is NOT supported — the API
  accepts it, nothing in this chart writes it, and a silent misparse is worse than a
  refusal.

  🔴 `m` (milli) IS DELIBERATELY ABSENT FROM THE TABLE, and it is the one omission that
  is a safety fix rather than a simplification. It was here "for completeness". Every
  other wrong-case suffix fails closed — `gi`, `ki`, `mi`, `ti`, `pi`, `K`, `g`, `t`,
  `p` are all rejected naming the mistake — but `20M` and `20m` BOTH parsed, and they
  differ by 10⁹: `20M` is 2e+07 bytes while `20m` is **0.02 bytes**. A single-keystroke
  slip on a storage value therefore produced a number so small that no ceiling check
  could ever fire on it, silently. Storage quantities are never expressed in milli-
  units, so dropping `m` costs nothing real and converts the tenth typo into the same
  loud failure as the other nine. If a caller ever genuinely needs milli-units, add it
  back for THAT caller, not to this shared helper.

  AND NOTE WHAT A CALLER CAN AND CANNOT BUILD ON THIS: it supports a CEILING check
  ("is X larger than the max?") and structurally cannot support a FLOOR one. A value
  that is absurdly small is, to every `gt` comparison here, simply "not too big" — so
  a nonsense-but-tiny quantity passes every guard written on top of this helper. That
  is the direction dropping `m` closes off at the parse step, because no guard above
  could have closed it.
*/ -}}
{{- define "uzi.quantityBytes" -}}
{{- $q := . | toString | trim -}}
{{- $num := regexFind "^[0-9]+(\\.[0-9]+)?" $q -}}
{{- if eq $num "" -}}
{{- fail (printf "uzi.quantityBytes: %q is not a Kubernetes resource quantity (expected a number optionally followed by Ki/Mi/Gi/Ti/Pi or k/M/G/T/P, e.g. \"20Gi\")" $q) -}}
{{- end -}}
{{- $suffix := trimPrefix $num $q -}}
{{- $factors := dict "" 1.0 "k" 1e3 "M" 1e6 "G" 1e9 "T" 1e12 "P" 1e15 "Ki" 1024.0 "Mi" 1048576.0 "Gi" 1073741824.0 "Ti" 1099511627776.0 "Pi" 1125899906842624.0 -}}
{{- if not (hasKey $factors $suffix) -}}
{{- fail (printf "uzi.quantityBytes: %q in %q is not a valid quantity suffix (expected one of Ki/Mi/Gi/Ti/Pi, k/M/G/T/P, or none — note the unit is case-sensitive, so \"Gi\" not \"gi\" and \"G\" not \"GB\")" $suffix $q) -}}
{{- end -}}
{{- mulf (float64 $num) (get $factors $suffix) -}}
{{- end -}}

{{- /*
  uzi.forgeEgressHosts: the worker egress destinations DERIVED from forge.allowedBaseURLs
  (PRD #808), as a JSON list of {"host": ..., "port": ...}. It is the ONE derivation every
  FQDN-egress provider consumes (Antrea ANNP, OVN EgressFirewall), so a forge host is
  declared once and cannot diverge between the api SSRF allowlist and any provider.

  urlParse .hostname gives the bare host (no :port); the port is derived from the URL too
  (default 443), so a non-standard forge port does NOT diverge from the api, which preserves
  the port in NormalizeForgeBaseURL.

  Validated fail-CLOSED to mirror the api: NormalizeForgeBaseURL refuses to boot on a
  scheme-less / hostless entry, so a render that would emit an empty name (match-nothing at
  best, match-anything at worst) is a hard render failure rather than a malformed rule.
  Consumers: `fromJsonArray (include "uzi.forgeEgressHosts" .)`.
*/}}
{{- define "uzi.forgeEgressHosts" -}}
{{- $out := list }}
{{- range .Values.forge.allowedBaseURLs }}
{{- $u := urlParse . }}
{{- $host := $u.hostname }}
{{- if or (not $host) (ne $u.scheme "https") }}
{{- fail (printf "forge.allowedBaseURLs entry %q must be an absolute https URL with a host, e.g. https://github.com (it feeds both FORGE_ALLOWED_BASE_URLS and the worker egress FQDN list; the api's NormalizeForgeBaseURL rejects the same input at boot)." .) }}
{{- end }}
{{- if or (contains ":" $host) (regexMatch "^[0-9]+(\\.[0-9]+)+$" $host) }}
{{- fail (printf "forge.allowedBaseURLs entry %q resolves to an IP-literal host (%s). FQDN egress rules cannot express an IP address, and the derived port parsing breaks on a bracketed IPv6 literal (splitting the host on ':' yields an invalid port). Configure a DNS hostname, or express an IP-literal forge with an ipBlock-based egress policy instead." . $host) }}
{{- end }}
{{- /* urlParse has no .port field; .host carries host[:port], so split it out (default 443). */}}
{{- $port := "443" }}
{{- if contains ":" $u.host }}{{- $port = last (splitList ":" $u.host) }}{{- end }}
{{- $out = append $out (dict "host" $host "port" $port) }}
{{- end }}
{{- toJson $out }}
{{- end }}

{{- /*
  uzi.validateFQDNEgress: the shared preconditions for every FQDN-egress provider.
  An empty forge list would leave the api on its built-in default forge while the egress
  policy allows none, silently blocking clone/fetch on the kube-native tier.
*/}}
{{- define "uzi.validateFQDNEgress" -}}
{{- if not .Values.forge.allowedBaseURLs }}
{{- fail "workers.fqdnEgress.enabled is true but forge.allowedBaseURLs is empty. The api falls back to its built-in default forge (https://github.com), which this egress policy would NOT allow, silently blocking git clone/fetch on the kube-native worker tier. Set forge.allowedBaseURLs to your forge base URL(s) — it single-sources both FORGE_ALLOWED_BASE_URLS and this egress list." }}
{{- end }}
{{- $p := .Values.workers.fqdnEgress.provider | default "antrea" }}
{{- if not (has $p (list "antrea" "ovn")) }}
{{- fail (printf "workers.fqdnEgress.provider %q is not supported; use \"antrea\" (crd.antrea.io NetworkPolicy) or \"ovn\" (k8s.ovn.org EgressFirewall + a NetworkPolicy external allow)." $p) }}
{{- end }}
{{- end }}

{{- /*
  The isolated lane (PRD #1906 M6): a third worker namespace with no internet, and the
  uzi-fetcher Deployment that is its only content path. Everything the lane adds renders
  only when uzi.isolatedLaneEnabled is non-empty, so a default install renders no new object.
*/ -}}
{{- define "uzi.isolatedLaneEnabled" -}}
{{- if and .Values.workers.enabled .Values.workers.isolatedLane.enabled -}}
true
{{- end -}}
{{- end -}}

{{- define "uzi.fetcherName" -}}
{{- printf "%s-fetcher" (include "uzi.fullname" .) -}}
{{- end -}}

{{- /*
  uzi.fetcherURL: the base URL a lane worker dials. The FQDN in the RELEASE namespace, for
  the same two reasons as uzi.apiInClusterURL: the lane is another namespace (a short name
  does not resolve there), and the name must be one the fetcher's certificate carries.
*/ -}}
{{- define "uzi.fetcherURL" -}}
{{- printf "https://%s.%s.svc.%s:%v" (include "uzi.fetcherName" .) .Release.Namespace .Values.api.tls.clusterDomain .Values.workers.isolatedLane.fetcher.port -}}
{{- end -}}

{{- /*
  uzi.fetcherTLSSecretName: the fetcher's serving pair (tls.crt, tls.key) and ca.crt, the
  CA that signed it. Written by the fetcher Certificate when cert-manager issues the api's
  certificate; otherwise a pre-created Secret named by isolatedLane.fetcher.tls.secretName.
*/ -}}
{{- define "uzi.fetcherTLSSecretName" -}}
{{- if .Values.workers.isolatedLane.fetcher.tls.secretName -}}
{{- .Values.workers.isolatedLane.fetcher.tls.secretName -}}
{{- else if .Values.api.tls.certManager.enabled -}}
{{- printf "%s-tls" (include "uzi.fetcherName" .) -}}
{{- else -}}
{{- required "workers.isolatedLane.fetcher.tls.secretName is required when api.tls.certManager.enabled is false: the chart then issues no fetcher certificate, so point this at a PRE-CREATED Secret holding tls.crt + tls.key + ca.crt for the fetcher Service's names." .Values.workers.isolatedLane.fetcher.tls.secretName -}}
{{- end -}}
{{- end -}}

{{- /*
  uzi.fetcherToken: the fetcher's service token when the chart generates it
  (isolatedLane.fetcher.token.source: generated). Reuses the value already stored in the
  applied Secret, so a `helm upgrade` does not rotate it; otherwise a fresh 48-char token.
  `lookup` returns nothing under `helm template` and under Argo CD, which renders with
  `helm template`: there every render is a NEW token (see values.yaml, and use
  source: existing for a GitOps install).
*/ -}}
{{- define "uzi.fetcherToken" -}}
{{- $existing := lookup "v1" "Secret" .Release.Namespace (printf "%s-token" (include "uzi.fetcherName" .)) -}}
{{- if and $existing (hasKey ($existing.data | default dict) "token") -}}
{{- index $existing.data "token" | b64dec -}}
{{- else -}}
{{- randAlphaNum 48 -}}
{{- end -}}
{{- end -}}

{{- /*
  uzi.fetcherTokenSecret: the Secret and keys the fetcher token (a file in the fetcher)
  and its hex sha256 (an env var in the api) come from, as JSON
  {"name": ..., "tokenKey": ..., "hashKey": ...}.
*/ -}}
{{- define "uzi.fetcherTokenSecret" -}}
{{- $t := .Values.workers.isolatedLane.fetcher.token -}}
{{- if eq $t.source "existing" -}}
{{- toJson (dict "name" .Values.infisical.app.managedSecretName "tokenKey" $t.tokenSecretKey "hashKey" $t.tokenHashSecretKey) -}}
{{- else -}}
{{- toJson (dict "name" (printf "%s-token" (include "uzi.fetcherName" .)) "tokenKey" "token" "hashKey" "token-sha256") -}}
{{- end -}}
{{- end -}}

{{- /*
  uzi.isolatedLaneBlockedCIDRs: the cluster's pod, service and node CIDRs, in that order, as
  a JSON list. The fetcher refuses them itself (UZI_FETCHER_BLOCKED_CIDRS) and its
  NetworkPolicy excludes them from the internet allow, so one list feeds both.
*/ -}}
{{- define "uzi.isolatedLaneBlockedCIDRs" -}}
{{- $c := .Values.workers.isolatedLane.clusterCIDRs -}}
{{- toJson (concat ($c.pod | default list) ($c.service | default list) ($c.node | default list)) -}}
{{- end -}}

{{- /*
  uzi.isolatedLaneFloorEgress: the isolated lane's three in-cluster egress rules (cluster DNS,
  the api's worker port, uzi-fetcher), as a YAML list. Called with
  (dict "root" $ "antrea" false) by the lane floor (worker-isolated-networkpolicy.yaml)
  and with "antrea" true by the lane's Antrea model-egress policy, which needs the same
  three peers as named Allow rules ahead of its drop belt (see there). One definition, so
  the two can never admit different in-cluster peers.
*/ -}}
{{- define "uzi.isolatedLaneFloorEgress" -}}
{{- $r := .root -}}
# DNS, the same peer as the restricted tier's. Also what lets the FQDN provider learn
# the model host's addresses (Antrea snoops the responses).
- {{ if .antrea }}name: allow-lane-dns
  action: Allow
  {{ end }}to:
    - namespaceSelector:
        matchLabels:
          kubernetes.io/metadata.name: {{ $r.Values.workers.networkPolicy.dns.namespace }}
      podSelector:
        matchLabels:
          {{- toYaml $r.Values.workers.networkPolicy.dns.podSelector | nindent 10 }}
  ports:
    {{- range $r.Values.workers.networkPolicy.dns.ports }}
    - protocol: UDP
      port: {{ . }}
    - protocol: TCP
      port: {{ . }}
    {{- end }}
# The api's worker port (register, claim, heartbeat, the run's own routes).
# Selector-based: an FQDN rule cannot match an in-cluster Service.
- {{ if .antrea }}name: allow-lane-api
  action: Allow
  {{ end }}to:
    - namespaceSelector:
        matchLabels:
          kubernetes.io/metadata.name: {{ $r.Release.Namespace }}
      podSelector:
        matchLabels:
          {{- include "uzi.selectorLabels" $r | nindent 10 }}
          app.kubernetes.io/component: api
  ports:
    - protocol: TCP
      port: {{ include "uzi.workerAPIPort" $r }}
# uzi-fetcher, on its container port (the policy sees the post-DNAT pod address and
# port). The lane's only path to web content.
- {{ if .antrea }}name: allow-lane-fetcher
  action: Allow
  {{ end }}to:
    - namespaceSelector:
        matchLabels:
          kubernetes.io/metadata.name: {{ $r.Release.Namespace }}
      podSelector:
        matchLabels:
          {{- include "uzi.selectorLabels" $r | nindent 10 }}
          app.kubernetes.io/component: fetcher
  ports:
    - protocol: TCP
      port: {{ $r.Values.workers.isolatedLane.fetcher.port }}
{{- end -}}

{{- /*
  uzi.isolatedLaneExceptV4: what an isolated-lane IPv4 internet allow (cidr 0.0.0.0/0) must
  except, as a JSON list: every non-public IPv4 range uzi-fetcher's own address policy
  refuses (api/internal/fetcher/addrpolicy.go blockedPrefixes, the IPv4 half, entry for
  entry) followed by the lane's IPv4 cluster CIDRs. Used by the fetcher's NetworkPolicy and
  by the lane's OVN external-egress policy; scripts/assert-chart-render.sh compares the
  rendered list with addrpolicy.go, so a range added there fails CI until it is added here.
*/ -}}
{{- define "uzi.isolatedLaneExceptV4" -}}
{{- $out := list "0.0.0.0/8" "10.0.0.0/8" "100.64.0.0/10" "127.0.0.0/8" "169.254.0.0/16" "172.16.0.0/12" "192.0.0.0/24" "192.0.2.0/24" "192.88.99.0/24" "192.168.0.0/16" "198.18.0.0/15" "198.51.100.0/24" "203.0.113.0/24" "224.0.0.0/4" "240.0.0.0/4" -}}
{{- range fromJsonArray (include "uzi.isolatedLaneBlockedCIDRs" .) -}}
{{- if not (contains ":" .) -}}
{{- $out = append $out . -}}
{{- end -}}
{{- end -}}
{{- toJson (uniq $out) -}}
{{- end -}}

{{- /*
  uzi.isolatedLaneExceptV6: the except list of the fetcher's IPv6 internet allow, whose cidr
  is 2000::/3 (global unicast), not ::/0, matching addrpolicy.go, which admits IPv6 only
  inside 2000::/3. Everything outside it (loopback, IPv4-mapped and -compatible, NAT64,
  discard, SRv6, unique-local, link-local, site-local, multicast) is therefore never
  allowed; what remains to except are addrpolicy.go's ranges INSIDE 2000::/3 (Teredo,
  benchmarking, ORCHID, documentation, 6to4) and the lane's IPv6 cluster CIDRs that fall
  inside 2000::/3. A cluster CIDR outside it (fd00::/8 and the like) is already excluded,
  and must not be listed: an ipBlock except has to lie within its cidr, or the apiserver
  rejects the policy. "Inside 2000::/3" is a first hextet written with four hex digits
  starting 2 or 3 (0x2000-0x3fff); a shorter first hextet is below 0x1000.
*/ -}}
{{- define "uzi.isolatedLaneExceptV6" -}}
{{- $out := list "2001::/32" "2001:2::/48" "2001:10::/28" "2001:20::/28" "2001:db8::/32" "2002::/16" "3fff::/20" -}}
{{- range fromJsonArray (include "uzi.isolatedLaneBlockedCIDRs" .) -}}
{{- if regexMatch "^[23][0-9a-fA-F]{3}:" . -}}
{{- $out = append $out . -}}
{{- end -}}
{{- end -}}
{{- toJson (uniq $out) -}}
{{- end -}}

{{- /*
  uzi.validateIsolatedLane: every precondition of the lane, checked once, at render time.
  Each one, left unchecked, renders a lane that is either unreachable or wider than
  promised, and nothing else reports it.
*/ -}}
{{- define "uzi.validateIsolatedLane" -}}
{{- $l := .Values.workers.isolatedLane -}}
{{- if and $l.enabled (not .Values.workers.enabled) -}}
{{- fail "workers.isolatedLane.enabled is true but workers.enabled is false. The lane is a hosted-worker namespace; turn on workers.enabled (and its controller) with it." -}}
{{- end -}}
{{- if include "uzi.isolatedLaneEnabled" . -}}
{{- if not .Values.workers.fqdnEgress.enabled -}}
{{- fail "workers.isolatedLane.enabled needs workers.fqdnEgress.enabled: the lane reaches its model host (workers.isolatedLane.modelHost) only through the FQDN egress provider (Antrea or OVN). Without it the lane's default-deny floor admits DNS, the api and the fetcher only, so every run there fails at its first model call." -}}
{{- end -}}
{{- if not .Values.api.tls.enabled -}}
{{- fail "workers.isolatedLane.enabled needs api.tls.enabled: uzi-fetcher dials the api over https only (its service token and every run credential cross that hop), and workers.allowPlaintextAPI does not apply to it." -}}
{{- end -}}
{{- $c := $l.clusterCIDRs -}}
{{- if or (not $c.pod) (not $c.service) (not $c.node) -}}
{{- fail "workers.isolatedLane.enabled needs workers.isolatedLane.clusterCIDRs.pod, .service and .node (each a non-empty list): the fetcher's NetworkPolicy subtracts them from its internet allow and the fetcher refuses them itself, so an unset list leaves the fetcher able to reach in-cluster addresses at the network layer." -}}
{{- end -}}
{{- range fromJsonArray (include "uzi.isolatedLaneBlockedCIDRs" .) -}}
{{- if not (regexMatch "^([0-9]{1,3}(\\.[0-9]{1,3}){3}|[0-9a-fA-F:]*:[0-9a-fA-F:]*)/[0-9]{1,3}$" .) -}}
{{- fail (printf "workers.isolatedLane.clusterCIDRs entry %q is not a CIDR (an address, a slash and a prefix length, e.g. 10.244.0.0/16 or fd00:10:244::/56)." .) -}}
{{- end -}}
{{- end -}}
{{- if or (not $l.modelHost) (contains "*" $l.modelHost) -}}
{{- fail (printf "workers.isolatedLane.modelHost must be one exact hostname (e.g. api.anthropic.com), got %q: the lane admits its model host by exact name only, never a wildcard." ($l.modelHost | toString)) -}}
{{- end -}}
{{- if or (eq $l.namespace .Values.workers.namespace) (eq $l.namespace .Values.workers.docker.namespace) (eq $l.namespace .Release.Namespace) -}}
{{- fail (printf "workers.isolatedLane.namespace %q must be its own namespace, distinct from workers.namespace, workers.docker.namespace and the release namespace: the lane's default-deny policy selects every pod in it." $l.namespace) -}}
{{- end -}}
{{- if not (has $l.fetcher.token.source (list "generated" "existing")) -}}
{{- fail (printf "workers.isolatedLane.fetcher.token.source must be generated or existing, got %q" ($l.fetcher.token.source | toString)) -}}
{{- end -}}
{{- /*
  The fetcher parses these with strconv (a positive decimal integer) and refuses to start
  otherwise. A YAML integer reaches the template as a float64, which `toString` would
  print as 5.24288e+07, so worker-isolated-fetcher.yaml renders int64 | toString; this
  refuses what that conversion cannot carry (a unit suffix, zero, a negative number).
*/ -}}
{{- range $k := list "maxFileBytes" "maxInflight" -}}
{{- $v := index $l.fetcher $k -}}
{{- if $v -}}
{{- if or (le (int64 $v) 0) (not (regexMatch "^[0-9]+(\\.0+)?$|^[0-9](\\.[0-9]+)?e\\+[0-9]+$" (toString $v))) -}}
{{- fail (printf "workers.isolatedLane.fetcher.%s must be a positive whole number (a byte count for maxFileBytes, a count for maxInflight), got %q" $k (toString $v)) -}}
{{- end -}}
{{- end -}}
{{- end -}}
{{- end -}}
{{- end -}}
