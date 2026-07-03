import { defineConfig } from "vite";
import react from "@vitejs/plugin-react";

// In production the SPA is served by nginx, which also proxies /api to the API
// service (same origin, no CORS). The dev proxy below mirrors that so `npm run
// dev` works against a locally running API on :8080.
export default defineConfig({
  plugins: [react()],
  server: {
    proxy: {
      "/api": "http://127.0.0.1:8080",
    },
  },
});
