import { readFileSync } from "node:fs";

import { sveltekit } from "@sveltejs/kit/vite";
import tailwindcss from "@tailwindcss/vite";
import { defineConfig, type Plugin } from "vite";

const pkg = JSON.parse(readFileSync(new URL("./package.json", import.meta.url), "utf8")) as {
  dependencies: Record<string, string>;
};

/**
 * fontLicences puts the licence of every font the interface ships beside the
 * interface, as `licenses/<family>.txt`. The SIL Open Font License asks that
 * its text travel with the fonts, and the binary is how they travel, as far
 * as an image that holds nothing else. The text is read from each package
 * at build time rather than kept in this repository, so the two cannot drift,
 * and a font added to the dependencies brings its licence with it.
 */
function fontLicences(): Plugin {
  const fonts = Object.keys(pkg.dependencies).filter((name) => name.startsWith("@fontsource"));
  return {
    name: "wegweiser:font-licences",
    apply: "build",
    generateBundle() {
      // Once, for the files the browser is sent; the server build is discarded.
      if (this.environment.config.consumer !== "client") return;
      for (const name of fonts) {
        this.emitFile({
          type: "asset",
          fileName: `licenses/${name.replace(/^@fontsource(-variable)?\//, "")}.txt`,
          source: readFileSync(new URL(`./node_modules/${name}/LICENSE`, import.meta.url), "utf8"),
        });
      }
    },
  };
}

export default defineConfig({
  plugins: [tailwindcss(), sveltekit(), fontLicences()],
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
