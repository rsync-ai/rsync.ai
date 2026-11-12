/**
 * "Still being set up" connection-test results.
 *
 * A never-deployed connector's container is built/started on demand, so the first
 * Test Connection on a fresh install can arrive before it is ready. The backend
 * answers with a retryable result (status "connector_deploying", retryable: true)
 * instead of a failure. The UI must show it as "wait and try again", never as a
 * credential/connectivity error — and must never show a raw Python import error
 * ("No module named 'pymongo'"), which only ever means the connector's runtime was
 * not ready. See KI-FIRST-CONNECTION-TEST-FALLS-BACK-TO-AN-UNUSABLE-STDIO-INTERPRETER.
 *
 * A connector whose image FAILED to build is the opposite case: retrying does not
 * help, and its answer carries the end of the build log, whose last lines often
 * include an import error naming the missing module. That text is never "still
 * being set up" (generator-trust plan, phase 3b).
 *
 * Lockstep: CONNECTOR_DEPLOYING_MARKER matches backend-orchestrator
 * internal/mcp.ConnectorDeployingMarker and api-gateway connectorDeployingMarker;
 * CONNECTOR_BUILD_FAILED_MARKER matches mcp.ConnectorBuildFailedMarker, api-gateway
 * connectorBuildFailedMarker and tool-generator BUILD_FAILED_PREFIX.
 */

export const CONNECTOR_DEPLOYING_MARKER = "connector is still being set up"
export const CONNECTOR_BUILD_FAILED_MARKER = "connector image failed to build"

/** True when a test's error text is a failed connector image build (with its log). */
export function isConnectorBuildFailedText(text: string | undefined | null): boolean {
  return !!text && text.includes(CONNECTOR_BUILD_FAILED_MARKER)
}

export function connectorDeployingMessage(displayName?: string): string {
  const name = (displayName || "").trim() || "This"
  return `The ${name} ${CONNECTOR_DEPLOYING_MARKER} for first use — this can take a minute or two. Please try again shortly.`
}

const MISSING_MODULE_RE = /ModuleNotFoundError|No module named\s/i

export interface TestConnectionResponseLike {
  success?: boolean
  status?: string
  retryable?: boolean
  error?: string
  message?: string
}

/** True when a Test Connection response means "connector not ready yet, retry". */
export function isConnectorDeployingResponse(data: TestConnectionResponseLike | null | undefined): boolean {
  if (!data || data.success) return false
  const text = `${data.error ?? ""} ${data.message ?? ""}`
  if (isConnectorBuildFailedText(text)) return false
  if (data.status === "connector_deploying" || data.retryable === true) return true
  return text.includes(CONNECTOR_DEPLOYING_MARKER) || MISSING_MODULE_RE.test(text)
}

/**
 * The notice for a create/update save refused because the pre-save connectivity test
 * reached a connector that is still being set up (api-gateway answers 503 with
 * status "connector_deploying", retryable: true). Nothing was saved; the user should
 * wait and save again. Returns null for any other response.
 */
export function connectorDeployingSaveNotice(
  httpStatus: number,
  body: (TestConnectionResponseLike & { test_error?: string }) | null | undefined,
  displayName?: string,
): { title: string; description: string } | null {
  if (httpStatus !== 503 || !body) return null
  if (body.status !== "connector_deploying" && body.error !== "connector_deploying") return null
  return {
    title: "Connector is still being set up — changes not saved",
    description: connectorDeployingErrorMessage(body.test_error, displayName) ?? connectorDeployingMessage(displayName),
  }
}

/**
 * Maps a failed test's error text to the retryable "still being set up" message when
 * it is one (the backend's own message, or a raw import error from an older backend).
 * Returns null for any other error so callers keep their normal handling.
 */
export function connectorDeployingErrorMessage(raw: string | undefined | null, displayName?: string): string | null {
  if (!raw || isConnectorBuildFailedText(raw)) return null
  if (raw.includes(CONNECTOR_DEPLOYING_MARKER)) return raw
  if (MISSING_MODULE_RE.test(raw)) return connectorDeployingMessage(displayName)
  return null
}

/**
 * The notice for a create/update save refused because the connector's image failed
 * to build (api-gateway answers 422 with error "connector_build_failed" and the end
 * of the build log in test_error). Nothing was saved, and saving again fails the
 * same way until the connector is fixed. Returns null for any other response.
 */
export function connectorBuildFailedSaveNotice(
  httpStatus: number,
  body: { error?: string; status?: string; message?: string; test_error?: string } | null | undefined,
): { title: string; description: string } | null {
  if (httpStatus !== 422 || !body) return null
  if (body.error !== "connector_build_failed" && body.status !== "connector_build_failed") return null
  return {
    title: "Connector image failed to build — changes not saved",
    description: body.test_error || body.message || "The connector image failed to build.",
  }
}
