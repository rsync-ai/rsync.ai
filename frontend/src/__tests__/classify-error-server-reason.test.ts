/**
 * KI-TOAST-HINT-HIDES-SERVER-REASON — found on live prod 2026-09-30: Stop on a
 * run that had just finished answered 400 {"error": "Pipeline is not running"},
 * and the toast read "Invalid request — Check that all required fields are
 * filled in correctly." There is no form behind a Stop button.
 *
 * classifyError gave every 400/422, 403 and 409 a hint guessed from the status
 * alone, and the toasts show `e.hint ?? e.message` (45 call sites), so the
 * guess replaced the server's own reason. A guessed hint now applies only when
 * the server gave no reason of its own; a 401's and a 503's hints stay, since
 * those statuses mean the same thing whatever the body says.
 */

import { describe, it, expect } from "vitest"
import { classifyError, readResponseErrorMessage, type AppError } from "@/lib/utils/error-handling"

/** What every toast call site shows as the description. */
const shown = (e: AppError) => e.hint ?? e.message

/** The error PipelineActions.handleStop builds from a failed response. */
const stopError = (statusCode: number, message: string) => Object.assign(new Error(), { statusCode, message })

describe("classifyError — a guessed hint never hides the server's reason", () => {
  it("a refused Stop shows why it was refused, under the Stop title", () => {
    const e = classifyError(stopError(400, "Pipeline is not running"), "pipeline.stop")
    expect(shown(e)).toBe("Pipeline is not running")
    expect(e.title).toBe("Could not stop pipeline")
  })

  it("a 422 body read through the HTTP-prefixed path keeps its reason", () => {
    const e = classifyError(new Error('HTTP 422: {"error": "name too long"}'), "connections.create")
    expect(shown(e)).toBe("name too long")
  })

  it("a 403 role refusal is not told its session expired", () => {
    const e = classifyError(stopError(403, "insufficient workspace role"), "pipeline.stop")
    expect(shown(e)).toBe("insufficient workspace role")
  })

  it("a 409 outside a delete is not told the resource is in use", () => {
    const e = classifyError(stopError(409, "Pipeline is already running"), "pipeline.run")
    expect(shown(e)).toBe("Pipeline is already running")
  })

  it("a 409 on a delete shows the server's reason too", () => {
    const e = classifyError(stopError(409, "connection is used by 2 pipelines"), "connections.delete")
    expect(shown(e)).toBe("connection is used by 2 pipelines")
  })
})

describe("classifyError — the guessed hints stay where they are the only help", () => {
  it("a 400 with only a status text keeps the field hint", () => {
    const e = classifyError(stopError(400, "Bad Request"), "connections.create")
    expect(e.hint).toBe("Check that all required fields are filled in correctly.")
  })

  it("a 400 whose body gave nothing keeps the field hint (status-line fallback)", async () => {
    const res = new Response("", { status: 400, statusText: "Bad Request" })
    const e = classifyError(stopError(400, await readResponseErrorMessage(res, "Save")), "connector.save")
    expect(e.hint).toBe("Check that all required fields are filled in correctly.")
  })

  it("a 409 with no reason keeps the in-use hint", () => {
    const e = classifyError(stopError(409, "Conflict"), "connections.delete")
    expect(e.hint).toBe("This resource is still in use by other components. Remove those dependencies first.")
  })

  it("a 403 with no reason keeps the session hint", () => {
    const e = classifyError(stopError(403, ""), "general")
    expect(e.hint).toBe("Your session may have expired. Refresh the page to sign in again.")
  })

  it("a 401 keeps the session hint whatever the body says", () => {
    const e = classifyError(stopError(401, "token expired"), "pipeline.stop")
    expect(e.hint).toBe("Your session may have expired. Refresh the page to sign in again.")
    expect(e.title).toBe("Authentication error")
  })

  it("a server suggestion is still the hint", () => {
    const e = classifyError(new Error('HTTP 400: {"error": "bad cron", "suggestion": "Use five fields."}'), "pipeline.run")
    expect(e.hint).toBe("Use five fields.")
    expect(e.message).toBe("bad cron")
  })

  it("a 400 outside any action keeps the generic title", () => {
    const e = classifyError(stopError(400, "name cannot be empty"), "general")
    expect(e.title).toBe("Invalid request")
    expect(shown(e)).toBe("name cannot be empty")
  })
})
