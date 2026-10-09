# Match codex-pricing-validation.ts and codexprice.go on final decoded values.
# Scan raw string tokens before fromjson can replace unpaired surrogate escapes.
# Escaped quotes/backslashes are consumed as units, valid pairs together.
def unicode_strings:
  all(scan("\"(?:[^\"\\\\]|\\\\.)*\"");
    all(scan("\\\\u[dD][89aAbB][0-9a-fA-F]{2}\\\\u[dD][c-fC-F][0-9a-fA-F]{2}|\\\\(?:u[0-9a-fA-F]{4}|.)");
      length != 6 or (test("^\\\\u[dD][89a-fA-F]") | not)));
def require($ok; $why): if $ok then . else error($why) end;
def fields($required; $optional):
  require(type == "object"; "expected object")
  | keys as $keys
  | require(($required - $keys | length) == 0; "missing required field")
  | require(($keys - ($required + $optional) | length) == 0; "unknown field");
def finite:
  if type == "number" then (isinfinite | not) and (isnan | not) else false end;
def day:
  require(type == "string"; "expected date string")
  | require(length == 10 and test("^[0-9]{4}-[0-9]{2}-[0-9]{2}$"); "expected YYYY-MM-DD")
  | (split("-") | map(tonumber)) as $parts
  | require($parts[0] >= 1; "year must be 0001..9999")
  | (strptime("%Y-%m-%d") | mktime) as $seconds
  # gmtime compares numeric components: strftime may render year 0001 as 1.
  | ($seconds | gmtime | [.[0], .[1] + 1, .[2]]) as $actual
  | require($actual == $parts; "expected real Gregorian date")
  | $seconds / 86400;
def source:
  require(type == "string"; "expected source string")
  | require((test("[^\\x21-\\x7e]") | not); "expected ASCII HTTPS URL")
  | require(test("^https://([^/:?#]+)(?::([0-9]+))?([/?#][A-Za-z0-9._~!$&'()*+,;=:@/?#%+-]*)?$"); "expected HTTPS URL")
  | capture("^https://(?<host>[^/:?#]+)(?::(?<port>[0-9]+))?(?<suffix>[/?#][A-Za-z0-9._~!$&'()*+,;=:@/?#%+-]*)?$") as $url
  | require(($url.host | length) <= 253; "host too long")
  | require(($url.host | split(".") | all(.[]; length <= 63 and test("^[A-Za-z0-9](?:[A-Za-z0-9-]*[A-Za-z0-9])?$"))); "invalid host")
  | require((if $url.host | test("^[0-9]+(?:\\.[0-9]+){3}$") then
      $url.host | split(".") | all(.[]; (tonumber <= 255))
    else true end); "invalid IPv4")
  | require((if $url.port != null then ($url.port | tonumber) as $p | ($p | finite) and $p >= 1 and $p <= 65535 else true end); "invalid port")
  | require((($url.suffix // "") | gsub("%[0-9A-Fa-f]{2}"; "") | contains("%") | not); "invalid percent escape");
def rates:
  fields(["uncached_input", "cached_input", "cache_write", "output"]; [])
  | require(all(.[]; finite and . >= 0); "expected finite nonnegative rates");
def table:
  fields(["version", "input_tier_threshold_tokens", "models"]; [])
  | require((.version | type == "string" and length > 0); "expected nonempty version")
  | require((.input_tier_threshold_tokens | finite and . > 0 and floor == .); "expected positive finite integer threshold")
  | .models |= (
      require(type == "object" and length > 0; "expected nonempty models")
      | with_entries(
          require(.key != ""; "empty model identifier")
          | .value |= (
              fields(["verified_at", "sources", "low", "high"]; ["promo_review_date"])
              | (.verified_at | day) as $verified
              | (if has("promo_review_date") then .promo_review_date | day else null end) as $promo
              | require((.sources | type == "array" and length > 0); "expected nonempty sources")
              | .sources |= map(source)
              | .low |= rates | .high |= rates
            )
        )
    );
def finding($subject; $date; $reason; $sources):
  {subject: $subject, date: $date, reason: $reason, sources: $sources};

require(unicode_strings; "JSON strings must contain well-formed Unicode")
| fromjson | table as $table
| ($today | day) as $now
| ($anthropic_date | day) as $fetched
| ($anthropic_source | source) as $source
| [
    ($table.models | to_entries | sort_by(.key)[] |
      .key as $model | .value as $row |
      (if $now - ($row.verified_at | day) > 30 then
        finding($model; $row.verified_at; "verified more than 30 days ago"; $row.sources)
       else empty end),
      (if $row | has("promo_review_date") then
         (($row.promo_review_date | day) - $now) as $remaining |
         if $remaining <= 0 then
           finding($model; $row.promo_review_date; "promotional review date passed"; $row.sources)
         elif $remaining <= 14 then
           finding($model; $row.promo_review_date; "promotional review due within 14 days"; $row.sources)
         else empty end
       else empty end)),
    (if $now - $fetched > 30 then
      finding("Anthropic"; $anthropic_date; "fetched more than 30 days ago"; [$source])
     else empty end)
  ]
