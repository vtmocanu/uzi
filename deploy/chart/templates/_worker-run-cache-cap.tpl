{{- /*
Verbatim value of one optional per-run cache cap setting (issue #4576). Call with a
dict holding "name" (the workers.<name> key) and "root" (the chart root context).
Renders the string unchanged (no trim), or nothing when the setting is unset (null or
empty string). The worker parses it leniently, so the chart does not interpret it.

Only a string is accepted: Helm decodes an unquoted number, boolean or off as a typed
value, which would be relayed in a form the operator did not write. Any other kind
fails the render.
*/ -}}
{{- define "uzi.workerRunCacheCapSetting" -}}
{{- $name := .name -}}
{{- $v := get (.root.Values.workers | default dict) $name -}}
{{- if kindIs "invalid" $v -}}
{{- else if kindIs "string" $v -}}
{{- $v -}}
{{- else -}}
{{- fail (printf "workers.%s must be a string: quote it in YAML or use --set-string" $name) -}}
{{- end -}}
{{- end -}}

{{- /* Validate both settings unconditionally (invoked from worker-invariants.yaml). */ -}}
{{- define "uzi.workerRunCacheCapValidate" -}}
{{- $_ := include "uzi.workerRunCacheCapSetting" (dict "name" "runCacheCapMaxBytes" "root" .) -}}
{{- $_ := include "uzi.workerRunCacheCapSetting" (dict "name" "runCacheCapFraction" "root" .) -}}
{{- end -}}
