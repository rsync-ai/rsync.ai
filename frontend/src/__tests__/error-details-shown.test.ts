import { describe, expect, it } from "vitest"
import {
  classifyError,
  extractErrorMessage,
  readResponseErrorMessage,
  withErrorDetails,
} from "@/lib/utils/error-handling"
import { APIRequestError } from "@/lib/errors/api-errors"
import { serverReason } from "@/components/pipeline/PipelineSchedulePanel"
import { extractRawMessage } from "@/lib/explorer/errorMessage"

// Bug class: a frontend error reader keeps the API's `error` and drops its
// `details`. Gateway handlers answer {error: "<fixed label>", details:
// "<reason>"}, so the user read "Invalid request" or "Schema discovery failed"
// and nothing they could fix. #1334 fixed the transform save; these are the
// shared readers and the sites that built their own message.
//
// The bodies below are the handlers' own (api-gateway/internal/handlers).

const BIND_ERROR =
  "Key: 'CreateScheduleRequest.ScheduleType' Error:Field validation for 'ScheduleType' failed on the 'required' tag"

function jsonResponse(status: number, body: unknown): Response {
  return new Response(JSON.stringify(body), { status, headers: { "Content-Type": "application/json" } })
}

function gatewayError(status: number, body: Record<string, unknown>) {
  return new APIRequestError(`HTTP ${status}: ${JSON.stringify(body)}`)
}

describe("withErrorDetails", () => {
  it("appends the reason to the label", () => {
    expect(withErrorDetails("Invalid request", BIND_ERROR)).toBe(`Invalid request: ${BIND_ERROR}`)
    expect(withErrorDetails("Schema discovery failed.", "dial tcp: no such host")).toBe(
      "Schema discovery failed. dial tcp: no such host",
    )
  })

  it("joins a list of reasons and shows each once", () => {
    expect(withErrorDetails("Refused", ["transform[0]: a", "transform[1]: b", "transform[0]: a"])).toBe(
      "Refused: transform[0]: a; transform[1]: b",
    )
  })

  it("does not repeat a reason the label already says", () => {
    expect(withErrorDetails("pipeline not found", "Pipeline not found")).toBe("pipeline not found")
  })

  it("shows the reason alone when there is no label, and the label alone when there is no reason", () => {
    expect(withErrorDetails("", "no reachable servers")).toBe("no reachable servers")
    expect(withErrorDetails("Invalid request", undefined)).toBe("Invalid request")
    expect(withErrorDetails("Invalid request", ["", 42, null])).toBe("Invalid request")
  })

  it("cuts an upstream's raw body short", () => {
    const shown = withErrorDetails("Slack returned error", "x".repeat(5000))
    expect(shown.length).toBeLessThan(400)
    expect(shown.endsWith("…")).toBe(true)
  })
})

describe("the shared readers show `details`", () => {
  it("extractErrorMessage — the batch table selection save (pipeline_tables.go)", () => {
    // updatePipelineTables throws extractErrorMessage(response body).
    const body = { error: "Invalid request", details: "json: cannot unmarshal string into Go struct field" }
    expect(extractErrorMessage(body)).toBe(
      "Invalid request: json: cannot unmarshal string into Go struct field",
    )
  })

  it("classifyError — a refused connection save (connections.go CreateConnection)", () => {
    const e = classifyError(
      gatewayError(400, { error: "Invalid oauth_token_id", details: "oauth token not found" }),
      "connector.save",
    )
    expect(e.message).toBe("Invalid oauth_token_id: oauth token not found")
    // Callers show `hint ?? message`: the server gave a reason, so no guessed
    // "check the required fields" hint may cover it.
    expect(e.hint).toBeUndefined()
  })

  it("readResponseErrorMessage — the table preview sample (connections.go SampleConnectionData)", async () => {
    const msg = await readResponseErrorMessage(
      jsonResponse(424, {
        error: "data sampling failed; check your connection and try again",
        details: "Could not load connection details. The connection may have been deleted or modified.",
      }),
      "Preview",
    )
    expect(msg).toContain("data sampling failed")
    expect(msg).toContain("Could not load connection details.")
  })

  it("readResponseErrorMessage — a list of reasons, and a body with only `details`", async () => {
    expect(
      await readResponseErrorMessage(
        jsonResponse(400, { error: "One or more transforms cannot be executed.", details: ["transform[0]: a", "transform[1]: b"] }),
      ),
    ).toBe("One or more transforms cannot be executed. transform[0]: a; transform[1]: b")
    expect(await readResponseErrorMessage(jsonResponse(502, { details: "no reachable servers" }))).toBe(
      "no reachable servers",
    )
  })

  it("serverReason — the schedule panel's create and update (pipeline_schedules.go)", async () => {
    expect(await serverReason(jsonResponse(400, { error: "Invalid request", details: BIND_ERROR }))).toBe(
      `Invalid request: ${BIND_ERROR}`,
    )
  })
})

describe("the Explorer's error reader shows `details`", () => {
  it("extractRawMessage appends `details`", () => {
    const body = { error: "Connector returned error", status: 424, details: "table not found" }
    expect(extractRawMessage(body)).toBe("Connector returned error: table not found")
  })
})
