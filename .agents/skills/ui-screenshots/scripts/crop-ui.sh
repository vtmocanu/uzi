#!/usr/bin/env bash
# crop-ui.sh — crop a full browser-window screenshot down to the uzi web UI,
# removing browser chrome (sidebar, toolbar, rounded window border) with no
# hardcoded coordinates. Works at any resolution / window size.
#
# Assumes a DARK-themed UI (uzi's default): the UI is one large dark rectangle
# surrounded by lighter browser chrome. It samples many rows and columns and,
# scanning outward from the centre of each, treats a sustained run of light
# pixels as the chrome edge (short light runs — card borders, text, big numbers
# — are ignored). It keeps the OUTERMOST edge across all samples: on-screen
# content can only push an edge inward, never past the real chrome, so the
# extreme is the true window-content boundary. It then insets just past the
# rounded corners until all four crop corners are dark.
#
# Usage:
#   crop-ui.sh <input.png> <output.png> [--width N] [--darkmax G] [--run N] [--inset N]
#
#   --width N    downscale the crop to N px wide (blog default: 1800). Omit to keep native.
#   --darkmax G  gray value (0-255) at/below which a pixel counts as UI-dark (default 80).
#   --run N      consecutive light pixels that mark a chrome edge (default 6).
#   --inset N    force a fixed inset instead of auto-growing past the corners.
#
# Exit: 0 ok; 2 usage; 3 no dark UI found (not a dark UI, or wrong window); 4 tool missing.
set -euo pipefail

die() { echo "crop-ui: $2" >&2; exit "$1"; }
command -v magick >/dev/null 2>&1 || die 4 "ImageMagick 'magick' not found on PATH"

IN=""; OUT=""; WIDTH=""; DARKMAX=80; RUN=6; FORCE_INSET=""
while [ $# -gt 0 ]; do
  case "$1" in
    --width)   WIDTH="${2:-}"; shift 2 ;;
    --darkmax) DARKMAX="${2:-}"; shift 2 ;;
    --run)     RUN="${2:-}"; shift 2 ;;
    --inset)   FORCE_INSET="${2:-}"; shift 2 ;;
    -*) die 2 "unknown flag: $1" ;;
    *) if [ -z "$IN" ]; then IN="$1"; elif [ -z "$OUT" ]; then OUT="$1"; else die 2 "too many args"; fi; shift ;;
  esac
done
[ -n "$IN" ] && [ -n "$OUT" ] || die 2 "usage: crop-ui.sh <input.png> <output.png> [--width N]"
[ -f "$IN" ] || die 2 "input not found: $IN"

read -r W H < <(magick identify -format '%w %h\n' "$IN")

# gray value (0-255) of one pixel
gray_at() { magick "$IN" -crop "1x1+$1+$2" +repage -colorspace Gray -depth 8 -format '%[fx:int(255*p{0,0})]' info: ; }

# For one 1px line (a row at y=$1, or a column at x=$1 when axis=col), scan
# outward from the line's centre and print "LO HI" — the innermost sustained
# chrome edges bounding the centre. Prints nothing if the centre pixel is light
# (that sample crosses content at the centre and is unusable).
line_edges() {
  local axis="$1" idx="$2" len center crop
  if [ "$axis" = row ]; then len="$W"; center=$((W/2)); crop="${W}x1+0+${idx}"; else len="$H"; center=$((H/2)); crop="1x${H}+${idx}+0"; fi
  magick "$IN" -crop "$crop" +repage -colorspace Gray -depth 8 txt:- \
  | awk -F'[(,]' -v c="$center" -v n="$len" -v dm="$DARKMAX" -v run="$RUN" '
      /^[0-9]/ { g[$1+0]=$3+0 }
      END {
        if (g[c] > dm) exit 0;      # centre on content — skip this sample
        lo=0; hi=n-1; lr=0;
        for (i=c; i>=0; i--) { if (g[i]>dm) { if (++lr>=run) { lo=i+lr; break } } else lr=0 }
        lr=0;
        for (i=c; i<n; i++) { if (g[i]>dm) { if (++lr>=run) { hi=i-lr; break } } else lr=0 }
        print lo, hi;
      }'
}

# Sample lines across each axis; keep the OUTERMOST edges.
LEFT=$W; RIGHT=-1; TOP=$H; BOTTOM=-1
for pct in 8 15 22 29 36 43 50 57 64 71 78 85 92; do
  y=$(( H * pct / 100 ))
  if read -r l r < <(line_edges row "$y"); then
    [ "$l" -lt "$LEFT" ] && LEFT="$l"; [ "$r" -gt "$RIGHT" ] && RIGHT="$r"
  fi
  x=$(( W * pct / 100 ))
  if read -r t b < <(line_edges col "$x"); then
    [ "$t" -lt "$TOP" ] && TOP="$t"; [ "$b" -gt "$BOTTOM" ] && BOTTOM="$b"
  fi
done
[ "$RIGHT" -gt "$LEFT" ] && [ "$BOTTOM" -gt "$TOP" ] || die 3 "no dark UI region found — not a dark UI, wrong window, or raise --darkmax"

# Inset past the rounded corners: grow until all 4 corners are dark (or forced).
inset_ok() {
  local l="$1" t="$2" r="$3" b="$4" px py
  for xy in "$l $t" "$r $t" "$l $b" "$r $b"; do
    read -r px py <<<"$xy"
    [ "$(gray_at "$px" "$py")" -le "$DARKMAX" ] || return 1
  done
  return 0
}

if [ -n "$FORCE_INSET" ]; then
  L=$((LEFT+FORCE_INSET)); T=$((TOP+FORCE_INSET)); R=$((RIGHT-FORCE_INSET)); B=$((BOTTOM-FORCE_INSET))
else
  ins=2
  while :; do
    L=$((LEFT+ins)); T=$((TOP+ins)); R=$((RIGHT-ins)); B=$((BOTTOM-ins))
    [ "$R" -gt "$L" ] && [ "$B" -gt "$T" ] || die 3 "could not bound a dark UI region (inset ran away)"
    if inset_ok "$L" "$T" "$R" "$B"; then break; fi
    ins=$((ins+3)); [ "$ins" -le 80 ] || die 3 "corners still light at inset=$ins — chrome may not be lighter than the UI"
  done
fi

CW=$((R-L+1)); CH=$((B-T+1))
[ "$CW" -gt 0 ] && [ "$CH" -gt 0 ] || die 3 "empty crop ($CW x $CH)"

if [ -n "$WIDTH" ]; then
  magick "$IN" -crop "${CW}x${CH}+${L}+${T}" +repage -resize "${WIDTH}x" "$OUT"
else
  magick "$IN" -crop "${CW}x${CH}+${L}+${T}" +repage "$OUT"
fi

read -r OW OH < <(magick identify -format '%w %h\n' "$OUT")
echo "crop-ui: ${IN##*/} ${W}x${H} -> ${OUT##*/} ${OW}x${OH} (box ${CW}x${CH}+${L}+${T})"
