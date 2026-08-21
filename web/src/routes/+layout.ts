// The interface is a single-page application served from an embedded
// filesystem: there is no Node process to render on and nothing to prerender.
// Every path resolves to the fallback document and the router takes it from
// there (see internal/api/ui.go).
export const ssr = false;
export const prerender = false;
export const trailingSlash = "never";
