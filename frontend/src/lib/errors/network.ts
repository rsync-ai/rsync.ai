/**
 * fetch fails with a TypeError when the connection fails or drops, and each
 * browser words it its own way. Any other TypeError is a bug, and keeps its
 * words. No imports, so every error reader can use it.
 */
const WORDINGS = [
  "failed to fetch", // Chrome, Edge
  "networkerror", // Firefox: "NetworkError when attempting to fetch resource."
  "load failed", // Safari
]

export function isNetworkFailure(err: unknown): boolean {
  if (!(err instanceof TypeError)) return false
  const message = err.message.toLowerCase()
  return WORDINGS.some((wording) => message.includes(wording))
}
