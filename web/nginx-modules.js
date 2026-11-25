// njs helper for the runtime web modules locations (PRD #4686).
//
// A module upstream is untrusted: nginx honours any X-Accel-* response header it sends
// (X-Accel-Redirect would make nginx serve an internal location, X-Accel-Limit-Rate and
// X-Accel-Buffering change delivery). proxy_ignore_headers lists only the five names nginx
// knows today, and proxy_hide_header the same five, so this filter drops EVERY header whose
// name starts with x-accel-, including names a later nginx may start to honour.
//
// ES5 only: njs 1.0.0 (the pinned image) rejects for...of.
function stripAccel(r) {
  var ks = Object.keys(r.headersOut);
  for (var i = 0; i < ks.length; i++) {
    if (/^x-accel-/i.test(ks[i])) {
      delete r.headersOut[ks[i]];
    }
  }
}

export default { stripAccel: stripAccel };
