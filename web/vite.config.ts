import { sveltekit } from "@sveltejs/kit/vite";
import tailwindcss from "@tailwindcss/vite";
import { defineConfig } from "vite";

export default defineConfig({
  plugins: [tailwindcss(), sveltekit()],
  server: {
    // `npm run dev` talks to a weg running beside it. Same-origin, so the
    // session cookie of D5 behaves in development exactly as it does in the
    // binary, and no CORS configuration exists to get wrong.
    proxy: {
      "/api": "http://127.0.0.1:8053",
      "/healthz": "http://127.0.0.1:8053",
    },
  },
  build: {
    // The whole bundle is embedded in the binary and served from memory, so a
    // few larger chunks beat many small ones.
    chunkSizeWarningLimit: 900,
  },
});
