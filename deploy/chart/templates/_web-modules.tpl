{{- /*
Runtime web modules (PRD #4686): validate web.modules[] and render the two files the web nginx
loads from /etc/nginx/uzi-modules/ (modules.json, the public list the SPA fetches, and
locations.conf, one proxy location per module). Both are keys of the web ConfigMap
(web-configmap.yaml), so checksum/nginx rolls the web pods when the list changes.

Order, per module: validate, strip an optional http://, split host from port, expand a dotless
non-IP host to <host>.<release namespace>.svc.cluster.local, then emit. Nothing is emitted for a
list that fails any rule; the render fails with a message naming web.modules[<index>].
The same rules are applied by scripts/web-modules-gen.py (compose) and share the fixtures in
scripts/fixtures/web-modules/.
*/ -}}

{{- /*
The icon vocabulary. Keep it EQUAL to MODULE_ICON_NAMES in web/src/modules/icons.tsx (the SPA
re-validates the list and skips an entry naming any other icon). A parity test pins the two.
*/ -}}
{{- define "uzi.webModules.icons" -}}
{{- list "activity" "board" "book" "bot" "bug" "chat" "clock" "factory" "file-text" "gear" "git" "home" "key" "package" "scale" "search" "server" "shield" "skill" "terminal" | toJson -}}
{{- end -}}

{{- /*
The validated, normalised module list as JSON (internal: it carries the upstream). Fails the
render on the first invalid rule. Call with the root context.
*/ -}}
{{- define "uzi.webModules.list" -}}
{{- /* Absent or null is an empty list; any other non-list value (false, 0, "", {}) is refused, so no falsy value is coerced into "no modules". */ -}}
{{- $mods := .Values.web.modules -}}
{{- if kindIs "invalid" $mods -}}
{{- $mods = list -}}
{{- else if not (kindIs "slice" $mods) -}}
{{- fail "web.modules must be a list" -}}
{{- end -}}
{{- $out := list -}}
{{- if $mods -}}
{{- $resolver := .Values.web.nginx.resolver | default "" -}}
{{- if not (kindIs "string" $resolver) -}}
{{- fail "web.nginx.resolver must be a string" -}}
{{- end -}}
{{- if eq (trim $resolver) "" -}}
{{- fail "web.modules requires web.nginx.resolver (the cluster DNS server IP, e.g. kube-dns): a module upstream is resolved per request, which nginx can only do with a resolver" -}}
{{- end -}}
{{- if not (regexMatch `^[A-Za-z0-9.:\[\]=-]+( [A-Za-z0-9.:\[\]=-]+)*$` $resolver) -}}
{{- fail (printf "web.nginx.resolver %q contains characters that are not valid in an nginx resolver directive" $resolver) -}}
{{- end -}}
{{- $ns := .Release.Namespace -}}
{{- $icons := include "uzi.webModules.icons" . | fromJsonArray -}}
{{- $seen := dict -}}
{{- $ipv4 := `^(25[0-5]|2[0-4][0-9]|1[0-9]{2}|[1-9]?[0-9])(\.(25[0-5]|2[0-4][0-9]|1[0-9]{2}|[1-9]?[0-9])){3}$` -}}
{{- $dns := `^[a-z0-9]([-a-z0-9]{0,61}[a-z0-9])?(\.[a-z0-9]([-a-z0-9]{0,61}[a-z0-9])?)*$` -}}
{{- $unsafe := `[\p{Cc}\x{200B}-\x{200F}\x{2028}\x{2029}\x{202A}-\x{202E}\x{2066}-\x{2069}\x{061C}\x{FEFF}\x{2060}-\x{2064}\x{00AD}\x{180E}]` -}}
{{- range $i, $m := $mods -}}
{{- $where := printf "web.modules[%d]" $i -}}
{{- if not (kindIs "map" $m) -}}
{{- fail (printf "%s must be a map" $where) -}}
{{- end -}}
{{- $id := index $m "id" -}}
{{- $idOK := false -}}
{{- if kindIs "string" $id -}}
{{- $idOK = regexMatch `^[a-z][a-z0-9-]{0,31}$` $id -}}
{{- end -}}
{{- if not $idOK -}}
{{- if kindIs "string" $id -}}
{{- fail (printf "%s: id %q is malformed: it must match ^[a-z][a-z0-9-]{0,31}$" $where $id) -}}
{{- end -}}
{{- fail (printf "%s: id must be a string matching ^[a-z][a-z0-9-]{0,31}$ (got %s)" $where (kindOf $id)) -}}
{{- end -}}
{{- if hasKey $seen $id -}}
{{- fail (printf "%s: duplicate id %q (web.modules[%d] already uses it)" $where $id (index $seen $id)) -}}
{{- end -}}
{{- $_ := set $seen $id $i -}}
{{- $where = printf "web.modules[%d] (%s)" $i $id -}}
{{- range $f := list "version" "page" "label" "icon" "navGroup" "upstream" -}}
{{- $v := index $m $f -}}
{{- $present := false -}}
{{- if kindIs "string" $v -}}
{{- $present = ne (trim $v) "" -}}
{{- end -}}
{{- if not $present -}}
{{- fail (printf "%s: missing required field %q (it must be a non-empty string)" $where $f) -}}
{{- end -}}
{{- end -}}
{{- range $f := list "label" "version" "page" -}}
{{- if regexMatch $unsafe (index $m $f) -}}
{{- fail (printf "%s: %s contains control or invisible formatting characters" $where $f) -}}
{{- end -}}
{{- end -}}
{{- if not (regexMatch `(?s)^.{1,40}$` $m.label) -}}
{{- fail (printf "%s: label must be 1 to 40 characters" $where) -}}
{{- end -}}
{{- if not (has $m.icon $icons) -}}
{{- fail (printf "%s: unknown icon %q (see MODULE_ICON_NAMES in web/src/modules/icons.tsx)" $where $m.icon) -}}
{{- end -}}
{{- if not (has $m.navGroup (list "work" "factory" "modules")) -}}
{{- fail (printf "%s: unknown navGroup %q (use work, factory or modules)" $where $m.navGroup) -}}
{{- end -}}
{{- $adminOnly := false -}}
{{- if hasKey $m "adminOnly" -}}
{{- if not (kindIs "bool" (index $m "adminOnly")) -}}
{{- fail (printf "%s: adminOnly must be a boolean" $where) -}}
{{- end -}}
{{- $adminOnly = index $m "adminOnly" -}}
{{- end -}}
{{- /* Upstream: the whole value is [http://]HOST:PORT and nothing else. */ -}}
{{- $up := $m.upstream -}}
{{- $bad := printf "%s: upstream %q is invalid: it must be [http://]HOST:PORT, with HOST a lowercase DNS-1123 name or a strict IPv4 address and PORT 1 to 65535 (no path, query, fragment, userinfo, other scheme or special characters)" $where $up -}}
{{- $rest := trimPrefix "http://" $up -}}
{{- $parts := splitList ":" $rest -}}
{{- if ne (len $parts) 2 -}}
{{- fail $bad -}}
{{- end -}}
{{- $host := index $parts 0 -}}
{{- $port := index $parts 1 -}}
{{- if not (regexMatch `^[1-9][0-9]{0,4}$` $port) -}}
{{- fail $bad -}}
{{- end -}}
{{- if gt (atoi $port) 65535 -}}
{{- fail $bad -}}
{{- end -}}
{{- if regexMatch `^[0-9.]+$` $host -}}
{{- if not (regexMatch $ipv4 $host) -}}
{{- fail $bad -}}
{{- end -}}
{{- else -}}
{{- if or (gt (len $host) 253) (not (regexMatch $dns $host)) -}}
{{- fail $bad -}}
{{- end -}}
{{- end -}}
{{- if not (contains "." $host) -}}
{{- $host = printf "%s.%s.svc.cluster.local" $host $ns -}}
{{- end -}}
{{- $out = append $out (dict "id" $id "version" $m.version "page" $m.page "label" $m.label "icon" $m.icon "navGroup" $m.navGroup "adminOnly" $adminOnly "upstream" (printf "%s:%s" $host $port)) -}}
{{- end -}}
{{- end -}}
{{- toJson $out -}}
{{- end -}}

{{- /*
The public projection served as /modules.json: exactly id, version, page, label, icon, navGroup
and adminOnly (always a boolean). The upstream and any unknown input field never appear here.
*/ -}}
{{- define "uzi.webModules.json" -}}
{{- $pub := list -}}
{{- range include "uzi.webModules.list" . | fromJsonArray -}}
{{- $pub = append $pub (dict "id" .id "version" .version "page" .page "label" .label "icon" .icon "navGroup" .navGroup "adminOnly" .adminOnly) -}}
{{- end -}}
{{- toPrettyJson $pub -}}
{{- end -}}

{{- /*
One module's location. Call with a dict: id, var (uzi_module_<id with - as _>), resolver, upstream.
Byte-identical to scripts/fixtures/web-modules/location.example.conf after substitution (that
file documents each line; scripts/assert-web-modules-render.sh diffs them). Do not reformat.
*/ -}}
{{- define "uzi.webModules.locationBlock" -}}
location ^~ /modules/{{ .id }}/ {
    resolver {{ .resolver }} valid=10s;
    resolver_timeout 5s;
    set ${{ .var }} {{ .upstream }};
    limit_except GET {
        deny all;
    }
    proxy_set_header Cookie "";
    proxy_set_header Authorization "";
    proxy_set_header Proxy-Authorization "";
    proxy_set_header X-CSRF-Token "";
    proxy_hide_header Cache-Control;
    proxy_hide_header Expires;
    proxy_hide_header Set-Cookie;
    proxy_hide_header Content-Security-Policy;
    proxy_hide_header X-Frame-Options;
    proxy_hide_header X-Content-Type-Options;
    proxy_hide_header Referrer-Policy;
    proxy_hide_header Service-Worker-Allowed;
    proxy_hide_header Clear-Site-Data;
    proxy_hide_header Strict-Transport-Security;
    proxy_hide_header Refresh;
    proxy_hide_header Permissions-Policy;
    proxy_hide_header Content-Security-Policy-Report-Only;
    proxy_hide_header Access-Control-Allow-Origin;
    proxy_hide_header Access-Control-Allow-Credentials;
    proxy_hide_header Access-Control-Allow-Methods;
    proxy_hide_header Access-Control-Allow-Headers;
    proxy_hide_header Access-Control-Expose-Headers;
    proxy_hide_header Report-To;
    proxy_hide_header NEL;
    proxy_hide_header X-Accel-Redirect;
    proxy_hide_header X-Accel-Expires;
    proxy_hide_header X-Accel-Limit-Rate;
    proxy_hide_header X-Accel-Buffering;
    proxy_hide_header X-Accel-Charset;
    js_header_filter uzi_modules.stripAccel;
    proxy_ignore_headers X-Accel-Redirect X-Accel-Expires X-Accel-Limit-Rate X-Accel-Buffering X-Accel-Charset Expires Cache-Control Set-Cookie;
    proxy_redirect off;
    proxy_pass http://${{ .var }}$request_uri;
    gzip on;
    gzip_comp_level 5;
    gzip_min_length 1024;
    gzip_vary on;
    gzip_proxied any;
    gzip_types application/javascript text/javascript text/css image/svg+xml application/json application/manifest+json;
    add_header Cache-Control $uzi_modcc always;
    add_header X-Content-Type-Options nosniff always;
    add_header X-Frame-Options DENY always;
    add_header Referrer-Policy no-referrer always;
    add_header Content-Security-Policy "default-src 'self'; base-uri 'self'; object-src 'none'; frame-ancestors 'none'; img-src 'self' data:; script-src 'self'; style-src 'self'; font-src 'self'; connect-src 'self'; form-action 'self'" always;
}
{{ end -}}

{{- /* All module locations (the images ship an empty one). Call with the root context. */ -}}
{{- define "uzi.webModules.locations" -}}
{{- $blocks := list -}}
{{- range include "uzi.webModules.list" . | fromJsonArray -}}
{{- $blocks = append $blocks (include "uzi.webModules.locationBlock" (dict "id" .id "var" (printf "uzi_module_%s" (replace "-" "_" .id)) "resolver" $.Values.web.nginx.resolver "upstream" .upstream)) -}}
{{- end -}}
{{- if $blocks -}}
# Runtime web modules (PRD #4686): generated by the chart from web.modules[]. Do not edit.
{{ join "\n" $blocks }}
{{- else -}}
# Runtime web modules (PRD #4686): no web.modules configured, so no module locations.
{{- end -}}
{{- end -}}
