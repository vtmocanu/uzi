import { defineConfig } from "vite";
import react from "@vitejs/plugin-react";

// In production the SPA is served by nginx, which also proxies /api to the API
// service (same origin, no CORS). The dev proxy below mirrors that so `npm run
// dev` works against a locally running API on :8080. ws: true upgrades the
// /api/ws WebSocket too (nginx does this via its dedicated /api/ws location), so
// the live run view streams under `npm run dev` and not just behind nginx.
export default defineConfig({
  plugins: [react()],
  server: {
    proxy: {
      "/api": {
        target: "http://127.0.0.1:8080",
        ws: true,
      },
    },
  },
});
