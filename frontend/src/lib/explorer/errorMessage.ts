import { withErrorDetails } from "@/lib/utils/error-handling"

/**
 * The human-readable message in any API error body the Explorer receives.
 *
 * A gateway handler answers `{error: "<label>", details: "<reason>"}` — a label
 * such as "query execution failed" with the database's own answer in `details` —
 * so the reason is appended to the label.
 */
export function extractRawMessage(data: unknown): string {
  if (typeof data === "string") return data.trim()
  if (!data || typeof data !== "object") return ""
  const body = data as Record<string, unknown>
  const direct = body.error || body.message || (typeof body.detail === "string" ? body.detail : "")
  if (direct) return withErrorDetails(String(direct), body.details)
  if (Array.isArray(body.detail)) {
    return body.detail
      .map((d) => {
        if (typeof d === "string") return d
        const item = (d ?? {}) as { loc?: unknown; msg?: unknown; message?: unknown }
        const loc = Array.isArray(item.loc) ? item.loc.filter((x) => x !== "body").join(".") : ""
        const msg = String(item.msg || item.message || "")
        return loc ? `${loc}: ${msg}` : msg
      })
      .filter(Boolean)
      .join("; ")
  }
  return withErrorDetails("", body.details)
}
