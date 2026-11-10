import { describe, expect, it, vi } from "vitest"
import type { Mock } from "vitest"

import { askAssistant } from "@/lib/api/assistant"
import { authFetch } from "@/lib/api/auth-fetch"
import { assistantErrorText } from "@/lib/assistant/ask"
import { parseAPIError } from "@/lib/errors/api-errors"
import { classifyError, parseApiError } from "@/lib/utils/error-handling"

vi.mock("@/lib/api/auth-fetch", () => ({ authFetch: vi.fn() }))

// Bug class: a dropped connection shown in the browser's own words. fetch
// fails with a TypeError, worded differently by each browser, and Ask rsync.ai
// showed the words as they came ("Failed to fetch"); the shared readers knew
// Chrome's and Firefox's and not Safari's. Every reader is run on all three.
const WORDINGS = [
  ["Chrome", "Failed to fetch"],
  ["Firefox", "NetworkError when attempting to fetch resource."],
  ["Safari", "Load failed"],
] as const

describe.each(WORDINGS)("a dropped connection in %s", (_browser, wording) => {
  it("Ask rsync.ai says the connection dropped", async () => {
    ;(authFetch as unknown as Mock).mockRejectedValue(new TypeError(wording))
    const err = await askAssistant({ question: "which pipelines failed?" }).catch((e: unknown) => e)
    expect(assistantErrorText(err)).toBe("The connection to the server dropped before the answer came.")
  })

  it("the shared readers call it a network error", () => {
    expect(parseApiError(new TypeError(wording)).code).toBe("NETWORK_ERROR")
    expect(parseAPIError(new TypeError(wording)).code).toBe("NETWORK_ERROR")
    expect(classifyError(new TypeError(wording)).title).toBe("Connection failed")
  })
})

it("a TypeError from a bug is not a dropped connection", async () => {
  const bug = new TypeError("Cannot read properties of undefined (reading 'id')")
  ;(authFetch as unknown as Mock).mockRejectedValue(bug)
  const err = await askAssistant({ question: "which pipelines failed?" }).catch((e: unknown) => e)
  expect(assistantErrorText(err)).toBe(bug.message)
  expect(parseApiError(bug).code).not.toBe("NETWORK_ERROR")
})
