import { readFileSync } from "node:fs";

import adapter from "@sveltejs/adapter-static";
import { sveltekit } from "@sveltejs/kit/vite";
import { vitePreprocess } from "@sveltejs/vite-plugin-svelte";
import tailwindcss from "@tailwindcss/vite";
import { defineConfig, type Plugin } from "vite";

const pkg = JSON.parse(readFileSync(new URL("./package.json", import.meta.url), "utf8")) as {
  version: string;
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
  plugins: [
    tailwindcss(),
    /**
     * The interface is a single-page application served by the Go binary from
     * an embedded filesystem. It is not prerendered and there is no Node
     * runtime: the fallback document is what every unknown path resolves to,
     * and the router takes over from there. That is why the Go side needs no
     * route table of its own: see internal/api/ui.go.
     */
    sveltekit({
      preprocess: vitePreprocess(),
      adapter: adapter({
        // Straight into the directory that is embedded. A copy step between
        // the two would be one more thing that can be stale.
        pages: "../internal/api/dist",
        assets: "../internal/api/dist",
        fallback: "index.html",
        precompress: false,
        strict: true,
      }),
      // The document carries its own policy, because only SvelteKit knows the
      // hashes of the inline scripts it emits. `frame-ancestors` is
      // deliberately absent: it is ignored in a meta element, so the Go side
      // sends X-Frame-Options instead (internal/api/ui.go).
      //
      // style-src allows inline because the interface sets style attributes
      // for values that are data (a latency bar's width, a swatch's colour)
      // and a hash cannot cover an attribute.
      csp: {
        mode: "hash",
        directives: {
          "default-src": ["self"],
          "script-src": ["self"],
          "style-src": ["self", "unsafe-inline"],
          "img-src": ["self", "data:"],
          "font-src": ["self"],
          "connect-src": ["self"],
          "form-action": ["none"],
          "base-uri": ["none"],
          "object-src": ["none"],
        },
      },
      // Pinned, so that two builds of the same sources produce the same
      // bytes. SvelteKit's default is a timestamp, which lands in the document
      // as the name of a global and would make the committed build differ on
      // every run, and `make web-check` could then never be green.
      version: {
        name: pkg.version,
      },
    }),
    fontLicences(),
  ],
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
