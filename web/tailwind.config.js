/** @type {import('tailwindcss').Config} */
// Semantic token mapping (adapted from multica's packages/ui/styles/tokens.css,
// which routes every color through CSS variables). Channels live in
// src/index.css as space-separated RGB so Tailwind opacity modifiers
// (e.g. bg-danger/10) keep working.
const token = (name) => `rgb(var(--${name}) / <alpha-value>)`;

export default {
  content: ["./index.html", "./src/**/*.{ts,tsx}"],
  theme: {
    extend: {
      colors: {
        ink: token("bg"), // page background (kept name: body class uses it)
        surface: token("surface"), // cards, sidebar
        raised: token("raised"), // inputs, hover fills, nested boxes
        edge: token("edge"), // default border
        "edge-strong": token("edge-strong"),
        fg: token("fg"),
        muted: token("muted"),
        faint: token("faint"),
        brand: token("brand"),
        "brand-hover": token("brand-hover"),
        "on-brand": token("on-brand"),
        ok: token("ok"),
        warn: token("warn"),
        danger: token("danger"),
        info: token("info"),
        ring: token("ring"), // keyboard focus ring (see :focus-visible in index.css)
      },
      // Radii derive from the single --radius token (multica's calc scheme in
      // tokens.css), so a theme can densify or soften the whole UI at once.
      borderRadius: {
        sm: "calc(var(--radius) * 0.4)",
        DEFAULT: "calc(var(--radius) * 0.6)",
        md: "calc(var(--radius) * 0.8)",
        lg: "var(--radius)",
        xl: "calc(var(--radius) * 1.4)",
        "2xl": "calc(var(--radius) * 1.8)",
      },
      fontFamily: {
        sans: ["var(--font-sans)"],
        mono: ["var(--font-mono)"],
      },
    },
  },
  plugins: [],
};
