import { describe, it, expect } from "vitest"
import {
  CONNECTOR_BUILD_FAILED_MARKER,
  CONNECTOR_DEPLOYING_MARKER,
  connectorBuildFailedSaveNotice,
  connectorDeployingErrorMessage,
  connectorDeployingMessage,
  connectorDeployingSaveNotice,
  isConnectorBuildFailedText,
  isConnectorDeployingResponse,
} from "../connector-deploying"

// Verbatim backend text (backend-orchestrator mcp.ConnectorDeployingMessage("MongoDB")).
const BACKEND_MSG =
  "The MongoDB connector is still being set up for first use — this can take a minute or two. Please try again shortly."

describe("connector-deploying", () => {
  it("builds the same message as the backend", () => {
    expect(connectorDeployingMessage("MongoDB")).toBe(BACKEND_MSG)
    expect(connectorDeployingMessage("")).toContain(CONNECTOR_DEPLOYING_MARKER)
  })

  it("recognizes the gateway's retryable response", () => {
    expect(
      isConnectorDeployingResponse({ success: false, status: "connector_deploying", retryable: true, error: BACKEND_MSG }),
    ).toBe(true)
    // Marker alone (e.g. a gateway that did not add status/retryable).
    expect(isConnectorDeployingResponse({ success: false, status: "failed", error: BACKEND_MSG })).toBe(true)
    // A raw import error from an older backend is the same condition.
    expect(isConnectorDeployingResponse({ success: false, error: "No module named 'pymongo'" })).toBe(true)
  })

  it("does not classify real failures or successes as deploying", () => {
    expect(isConnectorDeployingResponse({ success: false, status: "failed", error: "Authentication failed." })).toBe(false)
    expect(isConnectorDeployingResponse({ success: true, status: "success", message: "Connection test successful" })).toBe(false)
    expect(isConnectorDeployingResponse(null)).toBe(false)
  })

  it("maps raw import errors to the friendly message and leaves other errors alone", () => {
    const raw =
      "No module named 'pymongo' [mcp stdio fallback: connector mongodb@v1.0.0 ran as a subprocess inside the orchestrator because no Docker container was reachable]"
    const mapped = connectorDeployingErrorMessage(raw, "MongoDB")
    expect(mapped).toBe(BACKEND_MSG)
    expect(mapped).not.toContain("No module named")

    expect(connectorDeployingErrorMessage(BACKEND_MSG, "Other")).toBe(BACKEND_MSG)
    expect(connectorDeployingErrorMessage("Connection refused", "MongoDB")).toBeNull()
    expect(connectorDeployingErrorMessage("", "MongoDB")).toBeNull()
  })

  it("maps the gateway's retryable save refusal (edit page Save) to a wait-and-retry notice", () => {
    const body = {
      error: "connector_deploying",
      status: "connector_deploying",
      retryable: true,
      message: "The connector is still being set up, so nothing was saved. Try again in a minute.",
      test_error: BACKEND_MSG,
    }
    expect(connectorDeployingSaveNotice(503, body)).toEqual({
      title: "Connector is still being set up — changes not saved",
      description: BACKEND_MSG,
    })
    // No test_error: fall back to the generic deploying message.
    expect(connectorDeployingSaveNotice(503, { error: "connector_deploying" })?.description).toContain(
      CONNECTOR_DEPLOYING_MARKER,
    )
  })

  it("leaves other save refusals to their normal handling", () => {
    expect(
      connectorDeployingSaveNotice(422, { error: "connection_test_failed", test_error: "Authentication failed." }),
    ).toBeNull()
    expect(connectorDeployingSaveNotice(503, { error: "connection_test_interrupted" })).toBeNull()
    expect(connectorDeployingSaveNotice(503, null)).toBeNull()
  })
})

// A failed image build as the gateway answers it (orchestrator
// mcp.FriendlyTestConnectionError over tool-generator BUILD_FAILED_PREFIX). Its log
// ends in an import error, which must not read as "still being set up".
const BUILD_FAILED_MSG =
  "The Acme connector image failed to build. End of the build log:\n" +
  "Step 5/9 : RUN pip install -r requirements.txt\n" +
  "ERROR: No matching distribution found for acme-sdk==9.9\n" +
  "ModuleNotFoundError: No module named 'acme'"

describe("connector image failed to build", () => {
  it("is a failure, never a wait", () => {
    expect(isConnectorBuildFailedText(BUILD_FAILED_MSG)).toBe(true)
    expect(BUILD_FAILED_MSG).toContain(CONNECTOR_BUILD_FAILED_MARKER)
    expect(isConnectorDeployingResponse({ success: false, status: "failed", error: BUILD_FAILED_MSG })).toBe(false)
    expect(connectorDeployingErrorMessage(BUILD_FAILED_MSG, "Acme")).toBeNull()
    // A bare import error from an older backend is still the deploying case.
    expect(isConnectorDeployingResponse({ success: false, error: "No module named 'pymongo'" })).toBe(true)
  })

  it("maps the gateway's save refusal to the build log", () => {
    const body = {
      error: "connector_build_failed",
      status: "connector_build_failed",
      message: "The connector image failed to build, so nothing was saved.",
      test_error: BUILD_FAILED_MSG,
    }
    expect(connectorBuildFailedSaveNotice(422, body)).toEqual({
      title: "Connector image failed to build — changes not saved",
      description: BUILD_FAILED_MSG,
    })
    expect(connectorBuildFailedSaveNotice(422, { error: "connection_test_failed", test_error: "x" })).toBeNull()
    expect(connectorBuildFailedSaveNotice(503, body)).toBeNull()
    expect(connectorDeployingSaveNotice(422, body)).toBeNull()
  })
})
