export default {
  plugins: {
    // Tailwind v4 ships its PostCSS plugin as a separate package and handles
    // @import inlining and vendor prefixing itself, so postcss-import and
    // autoprefixer are no longer needed.
    "@tailwindcss/postcss": {},
  },
};
