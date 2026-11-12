/**
 * Which api-gateway the axe suite signs in to -- one answer for
 * playwright.a11y.config.ts (which starts it) and auth.setup.ts (which logs in).
 *
 * E2E_API_URL points the suite at a real gateway. Without it the suite starts
 * a11y-api-stub.mjs, on A11Y_STUB_PORT (default 5199, clear of a dev stack's
 * gateway on 5001), so the authenticated pages are visited on every run rather
 * than whenever something happens to serve that port.
 */
export const A11Y_STUB_PORT = process.env.A11Y_STUB_PORT || '5199'
export const USES_STUB = !process.env.E2E_API_URL
export const A11Y_API_URL = process.env.E2E_API_URL || `http://localhost:${A11Y_STUB_PORT}`
