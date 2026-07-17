// Reproducible generator for web/public/favicon-32.png — the 32x32 Safari-fallback
// favicon for PRD #70. Safari < 26 ignores SVG favicons, so we ship a raster PNG too.
//
// This sandbox (and CI) has NO SVG rasterizer (no rsvg-convert / ImageMagick /
// inkscape / sharp / canvas / PIL / cairosvg), so we rasterize by hand using only
// Node built-ins (node:zlib, node:fs). Run from the repo root:
//     node web/scripts/gen-favicon-png.mjs
//
// DELIBERATE DEVIATION FROM THE SVG: the SVG favicon draws the FactoryIcon as a
// STROKED outline. Here the same mark is drawn as a FILLED ember SILHOUETTE. A faithful
// mini stroke rasterizer is overkill for a 32px Safari-only fallback; a solid silhouette
// on the dark field reads cleanly and is trivial to produce with a scanline polygon fill.
// Path A's four `a2 2 0 0 0` rounded corners are flattened into short line segments; the
// tiny window ticks (Path B) are omitted (invisible at this size).

import { deflateSync } from "node:zlib";
import { writeFileSync } from "node:fs";
import { fileURLToPath } from "node:url";
import { dirname, join } from "node:path";

const SIZE = 32;

// Brand colors (see PRD #70 / web/src/components/icons.tsx).
const STEEL = [0x08, 0x0a, 0x0f]; // near-black field
const EMBER = [0xfb, 0x92, 0x3c]; // brand ember mark

// --- Build the factory silhouette polygon (24-unit coord space) ---------------
// Path A: M2 20 a2 2 0 0 0 2 2 h16 a2 2 0 0 0 2 -2 V8 l-7 5 V8 l-7 5 V4
//         a2 2 0 0 0 -2 -2 H4 a2 2 0 0 0 -2 2 z
// Each `a2 2 0 0 0` is an axis-aligned quarter circle (r=2, sweep=0). We flatten
// each into segments around its (hand-computed) center, sweeping -90 degrees.
const ARC_SEGMENTS = 4;

function arcPoints(cx, cy, startDeg, out) {
  // sweep -90 degrees from startDeg, emitting intermediate+end points (skip start).
  for (let i = 1; i <= ARC_SEGMENTS; i++) {
    const a = ((startDeg - (90 * i) / ARC_SEGMENTS) * Math.PI) / 180;
    out.push([cx + 2 * Math.cos(a), cy + 2 * Math.sin(a)]);
  }
}

function buildPolygon() {
  const p = [];
  p.push([2, 20]); // M
  arcPoints(4, 20, 180, p); // a2 2 .. -> (4,22)
  p.push([20, 22]); // h16
  arcPoints(20, 20, 90, p); // a2 2 .. -> (22,20)
  p.push([22, 8]); // V8
  p.push([15, 13]); // l-7 5
  p.push([15, 8]); // V8
  p.push([8, 13]); // l-7 5
  p.push([8, 4]); // V4
  arcPoints(6, 4, 0, p); // a2 2 .. -> (6,2)
  p.push([4, 2]); // H4
  arcPoints(4, 4, -90, p); // a2 2 .. -> (2,4)
  return p; // z closes back to (2,20)
}

// Map the 0..24 coord space into the 32px canvas with a small inset margin.
const MARGIN = 2.5;
const SCALE = (SIZE - 2 * MARGIN) / 24;
const toPx = ([x, y]) => [x * SCALE + MARGIN, y * SCALE + MARGIN];

const poly = buildPolygon().map(toPx);

// Point-in-polygon via scanline crossings for a given sample point.
function insidePolygon(px, py) {
  let inside = false;
  for (let i = 0, j = poly.length - 1; i < poly.length; j = i++) {
    const [xi, yi] = poly[i];
    const [xj, yj] = poly[j];
    const intersect =
      yi > py !== yj > py &&
      px < ((xj - xi) * (py - yi)) / (yj - yi) + xi;
    if (intersect) inside = !inside;
  }
  return inside;
}

// Rounded-square background mask: full canvas minus the four corner quarter-circles.
const RADIUS = 6;
function insideField(px, py) {
  let cx = null;
  let cy = null;
  if (px < RADIUS) cx = RADIUS;
  else if (px > SIZE - RADIUS) cx = SIZE - RADIUS;
  if (py < RADIUS) cy = RADIUS;
  else if (py > SIZE - RADIUS) cy = SIZE - RADIUS;
  if (cx !== null && cy !== null) {
    return Math.hypot(px - cx, py - cy) <= RADIUS;
  }
  return true; // edges / interior are inside
}

// --- Compose RGBA pixel buffer ------------------------------------------------
const raw = Buffer.alloc(SIZE * (1 + SIZE * 4)); // one filter byte per scanline
let o = 0;
for (let y = 0; y < SIZE; y++) {
  raw[o++] = 0; // filter: none
  for (let x = 0; x < SIZE; x++) {
    const px = x + 0.5;
    const py = y + 0.5;
    let r = 0;
    let g = 0;
    let b = 0;
    let a = 0;
    if (insideField(px, py)) {
      const c = insidePolygon(px, py) ? EMBER : STEEL;
      [r, g, b] = c;
      a = 255;
    }
    raw[o++] = r;
    raw[o++] = g;
    raw[o++] = b;
    raw[o++] = a;
  }
}

// --- PNG encoding (color type 6 / 8-bit RGBA) ---------------------------------
const CRC_TABLE = (() => {
  const t = new Uint32Array(256);
  for (let n = 0; n < 256; n++) {
    let c = n;
    for (let k = 0; k < 8; k++) c = c & 1 ? 0xedb88320 ^ (c >>> 1) : c >>> 1;
    t[n] = c >>> 0;
  }
  return t;
})();

function crc32(buf) {
  let c = 0xffffffff;
  for (let i = 0; i < buf.length; i++) c = CRC_TABLE[(c ^ buf[i]) & 0xff] ^ (c >>> 8);
  return (c ^ 0xffffffff) >>> 0;
}

function chunk(type, data) {
  const len = Buffer.alloc(4);
  len.writeUInt32BE(data.length, 0);
  const typeBuf = Buffer.from(type, "ascii");
  const crc = Buffer.alloc(4);
  crc.writeUInt32BE(crc32(Buffer.concat([typeBuf, data])), 0);
  return Buffer.concat([len, typeBuf, data, crc]);
}

const ihdr = Buffer.alloc(13);
ihdr.writeUInt32BE(SIZE, 0); // width
ihdr.writeUInt32BE(SIZE, 4); // height
ihdr[8] = 8; // bit depth
ihdr[9] = 6; // color type: RGBA
ihdr[10] = 0; // compression
ihdr[11] = 0; // filter
ihdr[12] = 0; // interlace

const png = Buffer.concat([
  Buffer.from([0x89, 0x50, 0x4e, 0x47, 0x0d, 0x0a, 0x1a, 0x0a]),
  chunk("IHDR", ihdr),
  chunk("IDAT", deflateSync(raw)),
  chunk("IEND", Buffer.alloc(0)),
]);

const outPath = join(dirname(fileURLToPath(import.meta.url)), "..", "public", "favicon-32.png");
writeFileSync(outPath, png);
console.log(`wrote ${outPath} (${png.length} bytes, ${SIZE}x${SIZE} RGBA)`);
