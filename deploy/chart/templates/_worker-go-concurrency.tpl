{{- /*
Canonical value of one optional Go concurrency setting. Call with a dict holding
"name" (the workers.<name> key) and "root" (the chart root context). Renders the
canonical decimal string, or nothing when the setting is unset (null or blank).

Same rule as agent/src/config.ts and controller/internal/config/config.go: trim;
empty is unset; decimal digits only (leading zeros accepted and stripped); the
value must be > 0 and <= 2147483647. Helm decodes an unquoted number in a values
file as float64 (toString may print exponent form) and --set as int64, so numbers
are formatted explicitly: an integral float via %.0f, a fractional one is rejected.
*/ -}}
{{- define "uzi.workerGoSetting" -}}
{{- $name := .name -}}
{{- $ceiling := "2147483647" -}}
{{- $v := get (.root.Values.workers | default dict) $name -}}
{{- $raw := "" -}}
{{- if kindIs "invalid" $v -}}
{{- else if kindIs "float64" $v -}}
{{- if eq (floor $v) $v -}}
{{- $raw = printf "%.0f" $v -}}
{{- else -}}
{{- fail (printf "workers.%s must be a positive decimal integer <= %s" $name $ceiling) -}}
{{- end -}}
{{- else if or (kindIs "string" $v) (kindIs "int" $v) (kindIs "int64" $v) -}}
{{- $raw = toString $v | trim -}}
{{- else -}}
{{- fail (printf "workers.%s must be a positive decimal integer <= %s" $name $ceiling) -}}
{{- end -}}
{{- if $raw -}}
{{- if not (regexMatch "^[0-9]+$" $raw) -}}
{{- fail (printf "workers.%s must be a positive decimal integer <= %s" $name $ceiling) -}}
{{- end -}}
{{- $n := regexReplaceAll "^0+" $raw "" | default "0" -}}
{{- if or (eq $n "0") (gt (len $n) (len $ceiling)) (and (eq (len $n) (len $ceiling)) (gt $n $ceiling)) -}}
{{- fail (printf "workers.%s must be a positive decimal integer <= %s" $name $ceiling) -}}
{{- end -}}
{{- $n -}}
{{- end -}}
{{- end -}}

{{- /* Validate both settings unconditionally (invoked from worker-invariants.yaml). */ -}}
{{- define "uzi.workerGoConcurrencyValidate" -}}
{{- $_ := include "uzi.workerGoSetting" (dict "name" "goBuildParallelism" "root" .) -}}
{{- $_ := include "uzi.workerGoSetting" (dict "name" "goMaxProcs" "root" .) -}}
{{- end -}}
