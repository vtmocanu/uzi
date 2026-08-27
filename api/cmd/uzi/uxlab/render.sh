#!/usr/bin/env bash
# Turn the raw-ANSI TUI frames under frames/ into PNGs under png/, using
# charmbracelet/freeze. Run it inside the devbox env so `freeze` is on PATH:
#
#   cd api/cmd/uzi/uxlab && devbox run build     # generate frames + render (one command)
#   cd api/cmd/uzi/uxlab && devbox run render    # render only (frames already exist)
#
# Theme handling, and why it is not just a background swap: freeze's default text
# colour is light, which is correct for a dark terminal but washes out un-styled text
# (most of the board) on a light one. A real light terminal draws that text near-black.
# So the light frames use the `github` chroma theme purely to get a dark DEFAULT
# foreground; the ANSI colour escapes in the frame still drive every styled cell.
set -euo pipefail

here="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
frames="$here/frames"
out="$here/png"
mkdir -p "$out"

# Chrome shared by both themes. 14px keeps a 100-col frame readable; the window
# controls and rounded corners frame it as a terminal rather than a code block.
common=(-l ansi -p 24 -m 0 --font.size 14 --line-height 1.3 --window -r 8 --shadow.blur 18 --shadow.y 6)

DARK_BG="#12121c"
LIGHT_BG="#f6f6f8"

shopt -s nullglob
count=0
for f in "$frames"/*.ansi; do
	base="$(basename "$f" .ansi)"
	png="$out/$base.png"
	if [[ "$base" == *-light ]]; then
		freeze "${common[@]}" -t github -b "$LIGHT_BG" "$f" -o "$png" >/dev/null
	else
		freeze "${common[@]}" -b "$DARK_BG" "$f" -o "$png" >/dev/null
	fi
	count=$((count + 1))
done

echo "rendered $count PNG(s) into $out"
