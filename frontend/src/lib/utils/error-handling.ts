/**
 * Centralized, connector-agnostic error handling utilities.
 *
 * Goals:
 * - Extract a useful human message from ANY backend error format (Go/Gin, FastAPI, tool-generator v1/v2, plain text)
 * - Work with our existing APIRequestError + tracedJsonFetch patterns
 * - Avoid leaking secrets (do not stringify configs)
 */

import { isNetworkFailure } from "@/lib/errors/network"

// ─── Structured UI error ────────────────────────────────────────────────────
// Use AppError anywhere you need to render a structured error in the UI
// (title as heading, message in mono, hint as actionable 💡 tip).
// Build one with classifyError() instead of constructing it by hand.

export type AppError = {
  /** Short label shown as the error heading. */
  title: string
  /** Raw server message or human fallback — shown in monospace. */
  message: string
  /** Actionable suggestion shown below the message (optional). */
  hint?: string
}

type ErrorContext =
  | "connections.load"
  | "connections.test"
  | "connections.create"
  | "connections.update"
  | "connections.delete"
  | "connectors.load"
  | "connector.save"
  | "connector.generate"
  | "connector.validate"
  | "pipeline.load"
  | "pipeline.run"
  | "pipeline.pause"
  | "pipeline.resume"
  | "pipeline.stop"
  | "pipeline.delete"
  | "pipeline.export"
  | "general"

/**
 * Convert any thrown value into a structured AppError for display.
 * Uses parseApiError() internally so every known backend error format is handled.
 */
export function classifyError(err: unknown, context: ErrorContext = "general"): AppError {
  const parsed = parseApiError(err)
  const status = parsed.statusCode
  const raw = withErrorDetails(parsed.message || "An unexpected error occurred", parsed.details)

  // Callers show `hint ?? message`, so a hint guessed from the status alone
  // replaces whatever the server said. The guesses below for a 403, 400/422 and
  // 409 apply only when the server gave no reason of its own
  // (KI-TOAST-HINT-HIDES-SERVER-REASON: a refused Stop read "Check that all
  // required fields are filled in correctly"). A 401's and a 503's hints stay:
  // those statuses mean the same thing whatever the body says.
  const reasonGiven = _serverGaveAReason(parsed)

  // ── Auth ────────────────────────────────────────────────────────────────
  if (status === 401 || status === 403) {
    return {
      title: "Authentication error",
      message: raw,
      hint: status === 401 || !reasonGiven ? "Your session may have expired. Refresh the page to sign in again." : undefined,
    }
  }

  // ── Service unavailable ─────────────────────────────────────────────────
  if (status === 503) {
    return {
      title: "Service unavailable",
      message: raw,
      hint: _serviceHint(context),
    }
  }

  // ── Validation / bad request ─────────────────────────────────────────────
  if (status === 422 || status === 400) {
    return {
      // A refused action is named for the action: there is no form behind Stop.
      title: context === "general" ? "Invalid request" : _titleForContext(context),
      message: raw,
      hint: parsed.suggestion || (reasonGiven ? undefined : "Check that all required fields are filled in correctly."),
    }
  }

  // ── Conflict ─────────────────────────────────────────────────────────────
  if (status === 409) {
    return {
      title: "Conflict",
      message: raw,
      hint: parsed.suggestion || (reasonGiven ? undefined : "This resource is still in use by other components. Remove those dependencies first."),
    }
  }

  // ── Not found ────────────────────────────────────────────────────────────
  if (status === 404) {
    return { title: "Not found", message: raw }
  }

  // ── Network / ECONNREFUSED ───────────────────────────────────────────────
  // The label alone: a server's `details` can quote "failed to fetch …" from
  // its own upstream call, and that is not the browser losing the network.
  const label = (parsed.message || "").toLowerCase()
  if (parsed.code === "NETWORK_ERROR" || label.includes("failed to fetch") || label.includes("networkerror")) {
    return {
      title: "Connection failed",
      message: "Could not reach the server.",
      hint: _networkHint(context),
    }
  }

  // ── Context-specific titles ──────────────────────────────────────────────
  const title = _titleForContext(context)
  const hint = parsed.suggestion || _hintForContext(context, raw)
  return { title, message: raw, hint }
}

// One reason longer than this is an upstream's raw body (Slack's, a dev
// build's err.Error()), not a sentence; a toast shows its start.
const MAX_REASON_CHARS = 300

/**
 * Append the reason the server gave in `details` to the label in `error`.
 *
 * Gateway handlers answer `{error: "<fixed label>", details: "<reason>"}` (a
 * list for the transform save): "Invalid request" plus the field that failed
 * binding, "Schema discovery failed" plus the connector's DNS error. A reader
 * that keeps only `error` leaves the user nothing to fix. A reason the label
 * already says, or one repeated, is shown once.
 */
export function withErrorDetails(label: string, details: unknown): string {
  const base = (label || "").trim()
  const seen = new Set<string>()
  const reasons = (Array.isArray(details) ? details : [details])
    .filter((d): d is string => typeof d === "string" && d.trim() !== "")
    .map((d) => {
      const reason = d.trim()
      return reason.length > MAX_REASON_CHARS ? `${reason.slice(0, MAX_REASON_CHARS)}…` : reason
    })
    .filter((reason) => {
      const key = reason.toLowerCase()
      if (seen.has(key) || base.toLowerCase().includes(key)) return false
      seen.add(key)
      return true
    })
  if (reasons.length === 0) return base
  if (!base) return reasons.join("; ")
  return `${base}${/[.!?]$/.test(base) ? " " : ": "}${reasons.join("; ")}`
}

// ── Private helpers ──────────────────────────────────────────────────────────

// A message that is only a status line — "Bad Request", "HTTP 400",
// "Save failed: Conflict (HTTP 409)" (readResponseErrorMessage's last resort) —
// names no reason, so a guessed hint is the only help there is.
const STATUS_LINE_ONLY =
  /^(?:[\w .-]*\bfailed:?\s*)?(?:HTTP\s*\d{3}|bad request|unprocessable (?:entity|content)|forbidden|conflict|unauthorized)?\s*(?:\(HTTP \d{3}\))?$/i

function _serverGaveAReason(parsed: ParsedApiError): boolean {
  if (parsed.details) return true
  const msg = (parsed.message || "").trim()
  return msg !== "" && msg !== "An unexpected error occurred" && !STATUS_LINE_ONLY.test(msg)
}

function _titleForContext(ctx: ErrorContext): string {
  const map: Record<ErrorContext, string> = {
    "connections.load": "Could not load connections",
    "connections.test": "Connection test failed",
    "connections.create": "Failed to create connection",
    "connections.update": "Failed to update connection",
    "connections.delete": "Failed to delete connection",
    "connectors.load": "Could not load connectors",
    "connector.save": "Failed to save connector",
    "connector.generate": "Connector generation failed",
    "connector.validate": "Validation failed",
    "pipeline.load": "Could not load pipelines",
    "pipeline.run": "Could not start pipeline",
    "pipeline.pause": "Could not pause pipeline",
    "pipeline.resume": "Could not resume pipeline",
    "pipeline.stop": "Could not stop pipeline",
    "pipeline.delete": "Could not delete pipeline",
    "pipeline.export": "Export failed",
    "general": "Something went wrong",
  }
  return map[ctx] ?? "Something went wrong"
}

function _hintForContext(ctx: ErrorContext, raw: string): string | undefined {
  const lower = raw.toLowerCase()
  if (ctx === "connections.test") {
    return lower.includes("timeout") || lower.includes("timed out")
      ? "The database host may be unreachable or blocked by a firewall."
      : lower.includes("password") || lower.includes("authentication") || lower.includes("auth")
      ? "Check your username and password."
      : lower.includes("ssl") || lower.includes("certificate")
      ? "Try toggling SSL mode in your connection config."
      : "Check your credentials and host settings, then try again."
  }
  if (ctx === "connector.generate") {
    return lower.includes("timeout") || lower.includes("timed out")
      ? "Generation timed out. Try again — large APIs can take 2-3 minutes."
      : lower.includes("rate limit") || lower.includes("429")
      ? "API rate limit hit. Wait a moment then retry."
      : "Check your API docs URL and try again."
  }
  if (ctx === "pipeline.run") {
    return lower.includes("connection") ? "Verify your source and destination connections are still active." : undefined
  }
  return undefined
}

// Cloud and self-hosted run this same app, and most readers are on Cloud, where
// nobody runs Docker: a hint leads with what any user can do, and the server
// step is one sentence for whoever hosts rsync.ai.
const SELF_HOSTED = "If you host rsync.ai yourself, check that its services are running."

function _serviceHint(ctx: ErrorContext): string {
  if (ctx === "connector.generate" || ctx === "connector.validate") {
    return "The connector generation service may be temporarily unavailable. Try again in a moment."
  }
  if (ctx.startsWith("pipeline.")) {
    return "The pipeline runner service may be temporarily unavailable. Try again shortly."
  }
  return `A backend service is temporarily unavailable. Try again in a moment. ${SELF_HOSTED}`
}

function _networkHint(ctx: ErrorContext): string {
  if (ctx.startsWith("connections.") || ctx.startsWith("connectors.") || ctx.startsWith("pipeline.")) {
    return `Check your network connection and try again. ${SELF_HOSTED}`
  }
  return "Check your network connection and try again."
}

export type ParsedApiError = {
  message: string
  code?: string
  details?: string
  field?: string
  suggestion?: string
  /** Why a connectivity test failed, when the server says (`test_error`). */
  cause?: string
  statusCode?: number
  raw?: unknown
}

/**
 * Convert a failed fetch Response into an Error that `parseApiError()` can understand.
 * We encode the HTTP status + raw body as: "HTTP <code>: <body>"
 */
export async function httpErrorFromResponse(response: Response, fallbackMessage: string): Promise<Error> {
  let body = ""
  try {
    body = await response.text()
  } catch {
    body = ""
  }
  const payload = body && body.trim() ? body : JSON.stringify({ error: fallbackMessage })
  return new Error(`HTTP ${response.status}: ${payload}`)
}

/**
 * Turn a failed `Response` into the one line a toast should show.
 *
 * KI-CDC-CONTROL-502-BODY-NOT-IN-TOAST — why this exists, and why the body is
 * read as TEXT exactly once:
 *
 * The CDC control buttons each carried their own copy of a helper that did
 * `await res.json().catch(() => null)` first and then fell back to
 * `await res.text().catch(() => "")`. That fallback is DEAD CODE. Per the Fetch
 * spec `Response.json()` marks the body disturbed *before* it parses, so once
 * `json()` has run — success or failure — `text()` always rejects with
 * "Body has already been read". On a 502 whose body is non-JSON or empty the
 * chain therefore evaluated to `"" || res.statusText || "Request failed"`, and
 * `statusText` is empty on HTTP/2, so the operator saw the bare literal
 * "Request failed" with the orchestrator's real reason nowhere in sight.
 *
 * So: read text ONCE, parse it ourselves, and make the last resort carry the
 * HTTP status — a message that names nothing is never acceptable.
 *
 * @param res    the failed response (already known to be !ok)
 * @param action optional verb for the last-resort line, e.g. "Pause" ->
 *               "Pause failed (HTTP 502)". Omit it when the toast title already
 *               names the action, otherwise the user reads "Stop failed" twice.
 */
const HTML_ERROR_PAGE = /^<(?:!doctype|html|head|body|title|center|h1)\b/i

export async function readResponseErrorMessage(res: Response, action?: string): Promise<string> {
  let body = ""
  try {
    body = await res.text()
  } catch {
    body = ""
  }
  const trimmed = body.trim()

  if (trimmed) {
    const json = safeJsonParse(trimmed)
    if (json && typeof json === "object") {
      const rec = json as Record<string, unknown>
      const msg = withErrorDetails(String(rec["message"] || rec["error"] || rec["detail"] || ""), rec["details"])
      if (msg) return msg
      // Structured, but with nothing human-readable in it. Fall through to the
      // status line rather than dumping a JSON blob into a toast.
    } else if (!HTML_ERROR_PAGE.test(trimmed)) {
      // Not JSON at all (a proxy's plain text, e.g. Envoy's "upstream connect
      // error"). This is the case the old json()-first ordering could never reach.
      return trimmed.length > 300 ? `${trimmed.slice(0, 300)}…` : trimmed
    }
    // An HTML error page (nginx's or a load balancer's "502 Bad Gateway") is
    // markup, not a reason: it falls through to the status line below rather
    // than printing tags into a toast or a chat message.
  }

  const what = action ? `${action} failed` : "Request failed"
  const statusText = res.statusText.trim()
  return statusText ? `${what}: ${statusText} (HTTP ${res.status})` : `${what} (HTTP ${res.status})`
}

/**
 * Parse an unknown thrown error into a normalized structure.
 * This is safe to call from anywhere (UI, API wrappers, hooks).
 */
export function parseApiError(err: unknown): ParsedApiError {
  // If it's our structured APIRequestError, preserve its fields (duck-typed to avoid cycles).
  const apiRequest = asApiRequestError(err)
  if (apiRequest) return apiRequest

  // Handle common patterns like: "HTTP 500: { ...json... }"
  if (err instanceof Error) {
    const parsed = parseFromHttpPrefixedError(err.message)
    if (parsed) return parsed
  }
  if (typeof err === "string") {
    const parsed = parseFromHttpPrefixedError(err)
    if (parsed) return parsed

    // Raw JSON string bodies (common when we use response.text()).
    const json = safeJsonParse(err)
    if (json && typeof json === "object") {
      return { ...normalizeErrorObject(json as Record<string, unknown>), raw: json }
    }
  }

  // Network errors
  if (isNetworkFailure(err)) {
    return {
      message: "Unable to connect to the server",
      code: "NETWORK_ERROR",
      suggestion: "Please check your connection and try again.",
      raw: err,
    }
  }

  // JSON-like objects from fetch clients
  if (err && typeof err === "object") {
    const obj = err as Record<string, unknown>
    // Sometimes errors include a raw string body
    const rawBody = asString(obj.body) || asString(obj.response) || asString(obj.data)
    if (rawBody) {
      const parsedHttp = parseFromHttpPrefixedError(rawBody)
      if (parsedHttp) return parsedHttp
      const json = safeJsonParse(rawBody)
      if (json && typeof json === "object") {
        return { ...normalizeErrorObject(json as Record<string, unknown>), raw: json }
      }
    }

    // Direct structured shape. A caller that threw `{ statusCode, ... }` keeps
    // its status, so a 409 or 500 is still one after this point.
    if (hasAnyKey(obj, ["error_message", "error", "message", "detail", "details", "code"])) {
      const statusCode = typeof obj.statusCode === "number" ? obj.statusCode : undefined
      return { ...normalizeErrorObject(obj), ...(statusCode ? { statusCode } : {}), raw: obj }
    }
  }

  // Plain Error fallback
  if (err instanceof Error) {
    return { message: err.message || "An unexpected error occurred", raw: err }
  }

  return { message: "An unexpected error occurred", raw: err }
}

/**
 * Best-effort message extraction for toasts, inline errors, etc.
 */
export function extractErrorMessage(err: unknown): string {
  const parsed = parseApiError(err)
  return withErrorDetails(parsed.message || "Unknown error", parsed.details)
}

// -----------------------------
// Internal helpers
// -----------------------------

function parseFromHttpPrefixedError(input: string): ParsedApiError | null {
  const str = String(input || "")
  const m = str.match(/^HTTP\s+(\d+)\s*:\s*([\s\S]+)$/)
  if (!m) return null

  const statusCode = Number(m[1])
  const body = m[2] || ""

  // Try to parse JSON response body
  const json = safeJsonParse(body)
  if (json && typeof json === "object") {
    return {
      statusCode,
      ...normalizeErrorObject(json as Record<string, unknown>),
      raw: json,
    }
  }

  return {
    statusCode,
    message: body.trim() || `Request failed (HTTP ${statusCode})`,
    raw: body,
  }
}

// A machine code such as "connection_test_failed" or "DUPLICATE_NAME": one
// token, no spaces. The gateway sends `{error: <code>, message: <sentence>}`.
const MACHINE_CODE = /^[A-Za-z0-9_.:-]+$/

function normalizeErrorObject(obj: Record<string, unknown>): Omit<ParsedApiError, "statusCode"> {
  // Tool-generator v2 / API Gateway patterns. `error` is a sentence from some
  // handlers and a code from others; when it is a code and a sentence sits in
  // `message`, the sentence is what the user needs to read.
  const error = asString(obj.error)
  const errorIsCode = !!error && MACHINE_CODE.test(error)
  const msg =
    asString(obj.error_message) ||
    (errorIsCode ? asString(obj.message) : undefined) ||
    error ||
    asString(obj.message) ||
    asString(obj.detail) ||
    // Some handlers return { errors: [...] }
    normalizeFastApiDetail(obj.detail) ||
    "An unexpected error occurred"

  const code = asString(obj.code) || asString(obj.error_code) || (errorIsCode ? error : undefined)
  const details = detailsText(obj.details) || asString(obj.hint)
  const field = asString(obj.field)
  const suggestion = asString(obj.suggestion)
  const cause = asString(obj.test_error)

  return { message: msg, code, details, field, suggestion, ...(cause ? { cause } : {}) }
}

function normalizeFastApiDetail(detail: unknown): string | undefined {
  // FastAPI/Pydantic: { detail: [{loc,msg,type}, ...] }
  if (!Array.isArray(detail)) return undefined
  const first = detail[0] as Record<string, unknown>
  if (!first) return undefined
  const msg = asString(first["msg"]) || asString(first["message"])
  const loc = Array.isArray(first["loc"]) ? (first["loc"] as unknown[]).join(".") : undefined
  if (msg && loc) return `${loc}: ${msg}`
  return msg
}

// `details` is one string from most handlers and a list from the transform save.
function detailsText(v: unknown): string | undefined {
  if (!Array.isArray(v)) return asString(v)
  const parts = v.map(asString).filter((p): p is string => !!p)
  return parts.length ? parts.join("; ") : undefined
}

function asString(v: unknown): string | undefined {
  if (typeof v === "string" && v.trim()) return v.trim()
  return undefined
}

function safeJsonParse(text: string): unknown | null {
  try {
    return JSON.parse(text)
  } catch {
    return null
  }
}

function hasAnyKey(obj: Record<string, unknown>, keys: string[]): boolean {
  return keys.some((k) => Object.prototype.hasOwnProperty.call(obj, k))
}

function asApiRequestError(err: unknown): ParsedApiError | null {
  if (!err || typeof err !== "object") return null
  const errObj = err as Record<string, unknown>
  if (errObj["name"] !== "APIRequestError") return null
  const apiErr = errObj["apiError"]
  if (!apiErr || typeof apiErr !== "object") {
    return { message: String(errObj["message"] || "An unexpected error occurred"), raw: err }
  }
  const api = apiErr as Record<string, unknown>
  return {
    message: String(api["message"] || errObj["message"] || "An unexpected error occurred"),
    code: api["code"] ? String(api["code"]) : undefined,
    details: api["details"] ? String(api["details"]) : undefined,
    field: api["field"] ? String(api["field"]) : undefined,
    suggestion: api["suggestion"] ? String(api["suggestion"]) : undefined,
    cause: api["cause"] ? String(api["cause"]) : undefined,
    raw: apiErr,
  }
}
