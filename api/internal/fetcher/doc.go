// Package fetcher is uzi-fetcher (PRD #1906 M2): the only path by which a profile-bound
// research run reads web content. It runs as its own Deployment from the api image
// (api/cmd/fetcher), never inside the api, because it parses untrusted internet content
// and the api holds every secret (Decision 2).
//
// # Worker -> fetcher
//
//	POST /v1/fetch
//	Authorization: Bearer <per-run fetch credential>
//	Content-Type: application/json
//	{"url": "https://docs.example.com/guide.pdf"}
//
// The body is apitypes.FetchRequest, decoded with encoding/json and unknown fields
// disallowed: a field whose name does not match "url" (encoding/json matches names
// case-insensitively, so "URL" is the url field, and a repeated key keeps its last
// value), a second JSON value, an empty url or a body over MaxRequestBody is a 400
// "bad_request", and a URL over MaxURLLen a 400 "url_too_long". Those shape errors
// happen before the api is asked and are not logged: there is no attempt to log. The
// fetcher itself performs a GET: the caller cannot choose a method, header or body.
// GET /healthz answers 200 without touching the api.
//
// On success: 200, the body is the fetched bytes, and the headers are
//
//	Content-Type:     the upstream media type, re-serialized (application/octet-stream if unparseable)
//	X-Uzi-Final-Url:  the URL the content came from, after redirects
//	X-Uzi-Sha256:     lowercase hex sha256 of the body
//	X-Uzi-Bytes:      the body length in bytes
//
// On refusal: a non-2xx status and an apitypes.FetchErrorDTO {error, reason}. Reason
// codes (stable; see the Reason* constants) and statuses:
//
//	400 bad_request, url_too_long             request shape, before Begin, not logged
//	                                          (url_too_long after Begin is a 403, below)
//	401 credential_invalid                    missing bearer, or the api says the run credential is invalid
//	429 admission_refused                     the api refused admission (admission_reason carries its code)
//	403 invalid_url, not_https, userinfo, ip_literal, port, off_list,
//	    redirect_off_list, private_address, too_many_redirects, redirect_invalid,
//	    content_encoding, url_too_long        policy refusals
//	502 dns_failed, connect_failed, tls, timeout, headers_too_large,
//	    upstream_status, upstream_error, too_large
//	503 control_unavailable, log_failed, busy, cancelled
//
// A redirect hop is checked exactly like the first URL; a hop whose host is off the list
// is "redirect_off_list", a hop that fails any other check carries that check's code. So
// "url_too_long" also refuses, after Begin and logged, a redirect Location over MaxURLLen
// and a URL whose escaped form (the URL actually requested) is over MaxURLLen.
// "upstream_status" means the site answered a non-2xx, non-redirect status: nothing of
// its body is returned and upstream_status carries the status. "content_encoding" means
// some Content-Encoding value (any header line, any comma-separated coding) is neither
// identity nor empty, although the fetcher asked for identity. The fetcher does not
// decode (a decoder is attack surface), so such a response is refused, and the bytes the
// per-file cap counts are always the bytes returned. "timeout" means the fetch's own time
// limit (or a phase timeout) ran out; "cancelled" means the worker went away mid-fetch,
// so the answer is recorded but nobody reads it.
//
// # Fetcher -> api (implemented by the api in PRD #1906 M3)
//
// Both routes are on the api's TLS listener, authenticated with the fetcher's own
// service credential (Authorization: Bearer <service token>, the token read from
// UZI_FETCHER_TOKEN_FILE; the api stores only its sha256, like the controller token).
// Bodies are JSON, decoded strictly by the api. A 401 on either route means the SERVICE
// credential was refused; the run credential's verdict is always in the body.
//
//	POST /api/fetcher/v1/begin      apitypes.FetcherBeginRequest {credential, url}
//	  200 apitypes.FetcherBeginResponse {reservation_id, entries, max_bytes}
//	      The api validated the run credential (introspection), atomically reserved
//	      max_bytes, one file slot and one concurrency slot against the run's totals,
//	      and returned the run's site-list snapshot taken at claim, as EFFECTIVE entries
//	      (egressprofile.EffectiveEntries already applied). reservation_id must be
//	      non-empty and max_bytes > 0, or the fetcher treats the answer as a control
//	      failure (and the api's sweep releases the reservation).
//	  403 apitypes.FetcherControlErrorDTO {reason: "credential_invalid"}
//	      Unknown, revoked or terminal-run credential. The worker gets 401.
//	  429 apitypes.FetcherControlErrorDTO {reason: <admission code>}
//	      A run total (bytes, files) or the concurrency limit is used up. The worker
//	      gets 429 "admission_refused" with admission_reason = the api's code.
//	  anything else: control failure, the worker gets 503 "control_unavailable".
//
//	POST /api/fetcher/v1/complete   apitypes.FetcherCompleteRequest
//	  {credential, reservation_id, url, final_url, verdict, reason, http_status,
//	   content_type, bytes, sha256, started_at, finished_at}
//	  2xx: logged. The api keys the run from credential (never from anything else the
//	      fetcher sends), writes one run_fetches row, and settles the reservation
//	      (releases the unused bytes and the concurrency slot).
//	  anything else: the fetcher returns 503 "log_failed" and NO content.
//
// Complete is sent exactly once for every attempt Begin admitted, allowed or refused,
// and the fetcher returns content only after the api acknowledged it (fail closed,
// Decision 8). verdict is "allowed" or "refused"; reason is "" when allowed and one of
// the reason codes above otherwise. final_url is printable ASCII of at most MaxURLLen
// bytes, for a refused hop too (see recordURL). The api's introspection may be cached
// for a short time; that only delays revocation, never widens a list, because the list
// is the claim-time snapshot.
//
// # What is checked
//
// Every hop: at most MaxURLLen bytes (as received and as escaped), https only, no
// userinfo, no port but 443, no IP-literal host; the host is normalized with
// egressprofile.NormalizeHost and checked with egressprofile.Match against the admitted
// entries; that normalized name, never the raw one, is what is resolved (as a fully
// qualified name, with a trailing dot), sent as SNI and Host, and verified on the
// certificate. The lookup happens inside the hop's own dialer, once; every address it
// returns must pass AddressPolicy (IPv4-mapped IPv6 unmapped first, any other IPv6
// admitted only inside 2000::/3; see blockedPrefixes), and the connection is dialed to a
// checked address itself, with the policy re-checked on the exact address being
// connected to. The transport ignores proxy environment, requests Accept-Encoding:
// identity with decompression off (and refuses any other Content-Encoding), caps the
// response header bytes and reads the body through a cap+1 limit. Redirects are followed by hand, at most
// MaxRedirects hops, each hop a fresh checked request, redirect bodies discarded under a
// small cap.
//
// Per-run totals, counts and concurrency are the api's (Begin's reservation), because
// per-replica counters do not add up across fetcher replicas. The fetcher's only own
// limit is a process-wide cap on concurrent fetches (DefaultMaxInflight), which bounds the
// fetch bodies it holds in memory at once.
package fetcher
