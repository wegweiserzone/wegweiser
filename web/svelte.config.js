import { readFileSync } from "node:fs";

import adapter from "@sveltejs/adapter-static";
import { vitePreprocess } from "@sveltejs/vite-plugin-svelte";

const pkg = JSON.parse(readFileSync(new URL("./package.json", import.meta.url), "utf8"));

/**
 * The interface is a single-page application served by the Go binary from an
 * embedded filesystem. It is not prerendered and there is no Node runtime: the
 * fallback document is what every unknown path resolves to, and the router
 * takes over from there. That is why the Go side needs no route table of its
 * own: see internal/api/ui.go.
 *
 * @type {import("@sveltejs/kit").Config}
 */
export default {
  preprocess: vitePreprocess(),
  kit: {
    adapter: adapter({
      // Straight into the directory that is embedded. A copy step between the
      // two would be one more thing that can be stale.
      pages: "../internal/api/dist",
      assets: "../internal/api/dist",
      fallback: "index.html",
      precompress: false,
      strict: true,
    }),
    // The document carries its own policy, because only SvelteKit knows the
    // hashes of the inline scripts it emits. `frame-ancestors` is deliberately
    // absent: it is ignored in a meta element, so the Go side sends
    // X-Frame-Options instead (internal/api/ui.go).
    //
    // style-src allows inline because the interface sets style attributes for
    // values that are data (a latency bar's width, a swatch's colour) and a
    // hash cannot cover an attribute.
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

    // Pinned, so that two builds of the same sources produce the same bytes.
    // SvelteKit's default is a timestamp, which lands in the document as the
    // name of a global and would make the committed build differ on every
    // run, and `make web-check` could then never be green.
    version: {
      name: pkg.version,
    },

    typescript: {
      config: (config) => {
        // The generated config points at .svelte-kit; keep our own strictness.
        config.compilerOptions.verbatimModuleSyntax = true;
        return config;
      },
    },
  },
};
