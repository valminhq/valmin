// SPA mode (ADR-002): no Node process in production, so nothing is rendered or
// prerendered at build time.
export const ssr = false;
export const prerender = false;
