{{- /*
Normalize the same boolean aliases as agent/src/config.ts. Disabled configuration
ignores numeric fields. Enabled configuration requires all six decimal values.
Limit-relative threshold ordering is validated by the worker's cgroup reader.
*/ -}}
{{- define "uzi.workerMemoryGuardEnabled" -}}
{{- $guard := .Values.workers.memoryGuard | default dict -}}
{{- $flag := get $guard "enabled" | toString | trim | lower -}}
{{- if has $flag (list "" "0" "false" "no" "off") -}}
false
{{- else if has $flag (list "1" "true" "yes" "on") -}}
{{- range $setting := list
  (dict "name" "reserveBytes" "ceiling" "9007199254740991")
  (dict "name" "sampleMs" "ceiling" "2147483647")
  (dict "name" "hysteresisBytes" "ceiling" "9007199254740991")
  (dict "name" "rearmMs" "ceiling" "2147483647")
  (dict "name" "responseBudgetMs" "ceiling" "2147483647")
  (dict "name" "maxInterventions" "ceiling" "9999")
-}}
{{- $raw := get $guard $setting.name | toString | trim -}}
{{- if not (regexMatch "^[0-9]+$" $raw) -}}
{{- fail (printf "workers.memoryGuard.%s must be an explicit positive decimal integer <= %s" $setting.name $setting.ceiling) -}}
{{- end -}}
{{- $n := regexReplaceAll "^0+" $raw "" | default "0" -}}
{{- if or (eq $n "0") (gt (len $n) (len $setting.ceiling)) (and (eq (len $n) (len $setting.ceiling)) (gt $n $setting.ceiling)) -}}
{{- fail (printf "workers.memoryGuard.%s must be an explicit positive decimal integer <= %s" $setting.name $setting.ceiling) -}}
{{- end -}}
{{- end -}}
true
{{- else -}}
{{- fail "workers.memoryGuard.enabled must be a boolean" -}}
{{- end -}}
{{- end -}}
