#!/usr/bin/env python3
"""Generate the runtime web module files for the compose stack (PRD #4686).

usage: web-modules-gen.py <input.json> <outdir>
       web-modules-gen.py --help

<input.json> is a JSON array of modules with the chart's web.modules[] field names:
id, version, page, label, icon, navGroup, adminOnly (optional boolean) and upstream
([http://]HOST:PORT, the compose service name and port of the module's container). It applies
the SAME rules as the Helm chart (deploy/chart/templates/_web-modules.tpl) and shares its
fixtures (scripts/fixtures/web-modules/); the one difference is that a dotless host is NOT
expanded to a cluster-local name, because compose resolves a service name as it is.

Every module is validated before anything is written. An invalid list (including text that cannot be encoded as UTF-8) exits 1 with a message
naming modules[<index>] and the field, and writes nothing (the output directory is not even
created). On success <outdir> is created or chmod-ed 0755 and gets, in this order:

  locations.conf  one `location ^~ /modules/<id>/` block per module (a comment-only file for an
                  empty list), resolved by Docker's embedded DNS (127.0.0.11)
  modules.json    the public list the SPA fetches: exactly id, version, page, label, icon,
                  navGroup and adminOnly, in input order. The upstream and any unknown input
                  field never appear in it.

Each file is written to a temporary file in <outdir> (mode 0644) and renamed into place.

OPERATOR RULES. There is no lock, no recovery and no crash-safety claim; the rules are:
  1. Run one generator invocation at a time.
  2. Recreate the web container only after the generator has exited successfully:
       docker compose -f docker-compose.yml -f docker-compose.modules.yml \\
         up -d --force-recreate web example-module
  3. After an interruption (a killed run, a full disk), run the generator again first.

THE BIND-MOUNT WINDOW. <outdir> is bind-mounted into the web container, so a running web
container serves a new modules.json as soon as it is renamed, while its nginx keeps the OLD
locations until the container is recreated. A module that was just added therefore shows its
error card, and a removed one still proxies, until you recreate the container. nginx reads the
two files only at start, so a container that is never recreated never changes its routes.

Exit codes: 0 written, 1 invalid input (nothing written), 2 usage, unreadable input or an
output failure.
"""

import json
import os
import re
import sys
import tempfile

# Keep equal to MODULE_ICON_NAMES in web/src/modules/icons.tsx and to uzi.webModules.icons in
# deploy/chart/templates/_web-modules.tpl; web/src/modules/parity.test.ts pins all three.
ICONS = ("activity", "board", "book", "bot", "bug", "chat", "clock", "factory", "file-text", "gear", "git", "home", "key", "package", "scale", "search", "server", "shield", "skill", "terminal")
# Keep equal to NAV_GROUPS in web/src/modules/manifest.ts and the chart's navGroup list.
NAV_GROUPS = ("work", "factory", "modules")

# Control and invisible formatting characters refused in label, version and page. Keep equal to
# UNSAFE_TEXT_RE in web/src/modules/manifest.ts and $unsafe in the chart's _web-modules.tpl
# (the parity test compares the three code point sets): \p{Cc}, the zero-width and directional
# marks U+200B-200F, U+2028/2029, the embedding and isolate controls U+202A-202E and
# U+2066-2069, U+061C, U+FEFF, the invisible operators U+2060-2064, the soft hyphen U+00AD and
# the Mongolian vowel separator U+180E.
UNSAFE_TEXT_RE = re.compile("[\u0000-\u001f\u007f-\u009f\u200b-\u200f\u2028\u2029\u202a-\u202e\u2066-\u2069\u061c\ufeff\u2060-\u2064\u00ad\u180e]")

ID_RE = re.compile(r"[a-z][a-z0-9-]{0,31}")
IPV4_RE = re.compile(r"(25[0-5]|2[0-4][0-9]|1[0-9]{2}|[1-9]?[0-9])(\.(25[0-5]|2[0-4][0-9]|1[0-9]{2}|[1-9]?[0-9])){3}")
DNS_RE = re.compile(r"[a-z0-9]([-a-z0-9]{0,61}[a-z0-9])?(\.[a-z0-9]([-a-z0-9]{0,61}[a-z0-9])?)*")
PORT_RE = re.compile(r"[1-9][0-9]{0,4}")
DIGITS_DOTS_RE = re.compile(r"[0-9.]+")

RESOLVER = "127.0.0.11"  # Docker's embedded DNS: the only resolver a compose network has

# One module's location, byte-identical to scripts/fixtures/web-modules/location.example.conf
# from its first `location` line on (scripts/web-modules-gen.test.sh diffs them). @ID@, @VAR@,
# @RESOLVER@ and @UPSTREAM@ are the only substitutions. Do not reformat.
LOCATION_BLOCK = r"""location ^~ /modules/@ID@/ {
    resolver @RESOLVER@ valid=10s;
    resolver_timeout 5s;
    set $@VAR@ @UPSTREAM@;
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
    proxy_pass http://$@VAR@$request_uri;
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
"""

HEADER = "# Runtime web modules (PRD #4686): generated by scripts/web-modules-gen.py from the module list. Do not edit.\n"
EMPTY = "# Runtime web modules (PRD #4686): no web.modules configured, so no module locations.\n"


class Invalid(Exception):
    pass


def _bad_upstream(where, value):
    return Invalid(
        '%s: upstream %s is invalid: it must be [http://]HOST:PORT, with HOST a lowercase DNS-1123 name or a strict IPv4 address and PORT 1 to 65535 (no path, query, fragment, userinfo, other scheme or special characters)'
        % (where, json.dumps(value))
    )


def validate(modules):
    """Return the validated modules (dicts with upstream normalised to host:port) or raise Invalid."""
    if not isinstance(modules, list):
        raise Invalid("the input must be a JSON array of modules")
    out = []
    seen = {}
    for i, m in enumerate(modules):
        where = "modules[%d]" % i
        if not isinstance(m, dict):
            raise Invalid("%s must be a map" % where)
        mid = m.get("id")
        if not isinstance(mid, str):
            raise Invalid("%s: id must be a string matching ^[a-z][a-z0-9-]{0,31}$ (got %s)" % (where, _kind(mid)))
        if ID_RE.fullmatch(mid) is None:
            raise Invalid('%s: id %s is malformed: it must match ^[a-z][a-z0-9-]{0,31}$' % (where, json.dumps(mid)))
        if mid in seen:
            raise Invalid("%s: duplicate id %s (modules[%d] already uses it)" % (where, json.dumps(mid), seen[mid]))
        seen[mid] = i
        where = "modules[%d] (%s)" % (i, mid)
        for f in ("version", "page", "label", "icon", "navGroup", "upstream"):
            v = m.get(f)
            if not isinstance(v, str) or v.strip() == "":
                raise Invalid('%s: missing required field "%s" (it must be a non-empty string)' % (where, f))
        for f in ("label", "version", "page"):
            if UNSAFE_TEXT_RE.search(m[f]):
                raise Invalid("%s: %s contains control or invisible formatting characters" % (where, f))
        if not 1 <= len(m["label"]) <= 40:
            raise Invalid("%s: label must be 1 to 40 characters" % where)
        if m["icon"] not in ICONS:
            raise Invalid("%s: unknown icon %s (see MODULE_ICON_NAMES in web/src/modules/icons.tsx)" % (where, json.dumps(m["icon"])))
        if m["navGroup"] not in NAV_GROUPS:
            raise Invalid("%s: unknown navGroup %s (use work, factory or modules)" % (where, json.dumps(m["navGroup"])))
        admin_only = False
        if "adminOnly" in m:
            if not isinstance(m["adminOnly"], bool):
                raise Invalid("%s: adminOnly must be a boolean" % where)
            admin_only = m["adminOnly"]
        up = m["upstream"]
        rest = up[len("http://"):] if up.startswith("http://") else up
        parts = rest.split(":")
        if len(parts) != 2:
            raise _bad_upstream(where, up)
        host, port = parts
        if PORT_RE.fullmatch(port) is None or int(port) > 65535:
            raise _bad_upstream(where, up)
        if DIGITS_DOTS_RE.fullmatch(host):
            if IPV4_RE.fullmatch(host) is None:
                raise _bad_upstream(where, up)
        elif len(host) > 253 or DNS_RE.fullmatch(host) is None:
            raise _bad_upstream(where, up)
        out.append(
            {
                "id": mid,
                "version": m["version"],
                "page": m["page"],
                "label": m["label"],
                "icon": m["icon"],
                "navGroup": m["navGroup"],
                "adminOnly": admin_only,
                "upstream": "%s:%s" % (host, port),
            }
        )
    return out


def _kind(v):
    if v is None:
        return "null"
    if isinstance(v, bool):
        return "bool"
    if isinstance(v, (int, float)):
        return "number"
    if isinstance(v, list):
        return "slice"
    if isinstance(v, dict):
        return "map"
    return type(v).__name__


def render_locations(mods):
    if not mods:
        return EMPTY
    blocks = []
    for m in mods:
        blocks.append(
            LOCATION_BLOCK.replace("@ID@", m["id"])
            .replace("@VAR@", "uzi_module_" + m["id"].replace("-", "_"))
            .replace("@RESOLVER@", RESOLVER)
            .replace("@UPSTREAM@", m["upstream"])
        )
    return HEADER + "\n".join(blocks)


def render_json(mods):
    pub = [{k: m[k] for k in ("id", "version", "page", "label", "icon", "navGroup", "adminOnly")} for m in mods]
    return json.dumps(pub, indent=2, ensure_ascii=False) + "\n"


def write_atomic(outdir, name, data):
    fd, tmp = tempfile.mkstemp(prefix="." + name + ".", suffix=".tmp", dir=outdir)
    try:
        with os.fdopen(fd, "wb") as f:
            f.write(data)
        os.chmod(tmp, 0o644)
        os.replace(tmp, os.path.join(outdir, name))
    except BaseException:
        try:
            os.unlink(tmp)
        except OSError:
            pass
        raise


def main(argv):
    if len(argv) == 2 and argv[1] in ("-h", "--help"):
        print(__doc__)
        return 0
    if len(argv) != 3:
        sys.stderr.write("usage: web-modules-gen.py <input.json> <outdir>\n")
        return 2
    try:
        with open(argv[1], "r", encoding="utf-8") as f:
            data = json.load(f)
    except (OSError, ValueError) as e:
        sys.stderr.write("web-modules-gen: cannot read %s: %s\n" % (argv[1], e))
        return 2
    try:
        mods = validate(data)
    except Invalid as e:
        sys.stderr.write("web-modules-gen: %s\n" % e)
        return 1
    # Encode both outputs strictly BEFORE the output directory is touched: a lone surrogate in a
    # label passes validate() but cannot be encoded, and that is invalid input, not a crash.
    try:
        locations = render_locations(mods).encode("utf-8")
        public = render_json(mods).encode("utf-8")
    except UnicodeEncodeError as e:
        sys.stderr.write("web-modules-gen: a field is not valid UTF-8 text (a lone surrogate?): %s\n" % e)
        return 1
    outdir = argv[2]
    try:
        os.makedirs(outdir, exist_ok=True)
        os.chmod(outdir, 0o755)
        write_atomic(outdir, "locations.conf", locations)
        write_atomic(outdir, "modules.json", public)
    except OSError as e:
        sys.stderr.write("web-modules-gen: cannot write %s: %s\n" % (outdir, e))
        return 2
    print("web-modules-gen: wrote %d module(s) to %s" % (len(mods), outdir))
    return 0


if __name__ == "__main__":
    sys.exit(main(sys.argv))
