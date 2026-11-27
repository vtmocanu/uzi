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

{{- /*
Canonical value of workers.goMemLimit (GOMEMLIMIT for worker commands). Call with a
dict holding "root". Renders "<n>MiB" or "<n>GiB", or nothing when unset (null or
blank). Same rule as agent/src/go-concurrency.ts and controller/internal/config:
trim only ASCII space, tab, CR and LF; empty is unset; the value must match
^[0-9]+(MiB|GiB)$ with the number 1..2147483647 (leading zeros stripped). Only a
string is accepted: the unit is required, so any typed number or bool fails.
*/ -}}
{{- define "uzi.workerGoMemLimit" -}}
{{- $diag := "workers.goMemLimit must be a positive integer <= 2147483647 followed by MiB or GiB" -}}
{{- $ceiling := "2147483647" -}}
{{- $v := get (.root.Values.workers | default dict) "goMemLimit" -}}
{{- if kindIs "invalid" $v -}}
{{- else if not (kindIs "string" $v) -}}
{{- fail $diag -}}
{{- else -}}
{{- $raw := regexReplaceAll "^[ \t\r\n]+|[ \t\r\n]+$" $v "" -}}
{{- if $raw -}}
{{- if not (regexMatch "^[0-9]+(MiB|GiB)$" $raw) -}}
{{- fail $diag -}}
{{- end -}}
{{- $unit := regexFind "(MiB|GiB)$" $raw -}}
{{- $n := regexReplaceAll "^0+" (regexFind "^[0-9]+" $raw) "" | default "0" -}}
{{- if or (eq $n "0") (gt (len $n) (len $ceiling)) (and (eq (len $n) (len $ceiling)) (gt $n $ceiling)) -}}
{{- fail $diag -}}
{{- end -}}
{{- printf "%s%s" $n $unit -}}
{{- end -}}
{{- end -}}
{{- end -}}

{{- /* Validate all settings unconditionally (invoked from worker-invariants.yaml). */ -}}
{{- define "uzi.workerGoConcurrencyValidate" -}}
{{- $_ := include "uzi.workerGoSetting" (dict "name" "goBuildParallelism" "root" .) -}}
{{- $_ := include "uzi.workerGoSetting" (dict "name" "goMaxProcs" "root" .) -}}
{{- $_ := include "uzi.workerGoMemLimit" (dict "root" .) -}}
{{- end -}}
