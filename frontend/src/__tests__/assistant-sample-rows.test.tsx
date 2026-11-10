import { afterEach, beforeAll, beforeEach, describe, expect, it, vi } from "vitest"
import type { Mock } from "vitest"
import { fireEvent, render, screen, waitFor, within } from "@testing-library/react"
import { toast } from "sonner"

import { lookedAtLabel } from "@/components/assistant/AssistantThread"
import { SampleRowsNotice } from "@/components/assistant/SampleRowsNotice"
import { WorkspaceLLMDataCard } from "@/components/workspace/WorkspaceLLMDataCard"
import { featureFlagsManager } from "@/config/features"
import { authFetch } from "@/lib/api/auth-fetch"
import { API_ENDPOINTS } from "@/lib/config/api"

// Sample rows: api-gateway and the egress guard own the rules (two switches,
// owner only, fail closed, audited). What only the page can get wrong:
//   - the owner's switch asks before it turns sample rows on, never before off;
//   - anyone else sees the setting and no switch;
//   - with the assistant off (404) the card is not there at all;
//   - an install without sample rows gets the explainer and no switch, and no
//     switch shows before /features has answered;
//   - "Looked at" says what a sample read, never a value;
//   - the connection form says so only while a model can get rows.

vi.mock("@/lib/api/auth-fetch", () => ({ authFetch: vi.fn() }))
vi.mock("sonner", () => ({ toast: { success: vi.fn(), error: vi.fn() } }))

const mockFetch = authFetch as unknown as Mock

function json(status: number, body: unknown) {
  return {
    ok: status >= 200 && status < 300,
    status,
    headers: { get: () => null },
    json: () => Promise.resolve(body),
  } as unknown as Response
}

type Call = { url: string; method: string; body?: unknown }

function serve(get: Response, put?: (body: { mode: string }) => Response) {
  const calls: Call[] = []
  mockFetch.mockImplementation((url: string, init?: RequestInit) => {
    const method = init?.method ?? "GET"
    const body = init?.body ? JSON.parse(String(init.body)) : undefined
    calls.push({ url, method, body })
    if (method === "PUT" && put) return Promise.resolve(put(body))
    return Promise.resolve(get)
  })
  return calls
}

beforeAll(() => {
  window.ResizeObserver ||= class {
    observe() {}
    unobserve() {}
    disconnect() {}
  }
})

beforeEach(() => {
  mockFetch.mockReset()
  ;(toast.success as Mock).mockReset()
  ;(toast.error as Mock).mockReset()
  featureFlagsManager.markResolved() // the cloud answer: sample rows allowed
})

afterEach(() => featureFlagsManager.resetFlags())

describe("workspace sample-rows card", () => {
  it("the owner's switch asks before turning sample rows on", async () => {
    const calls = serve(
      json(200, { mode: "names_only", approved_models: ["claude"], can_edit: true }),
      (body) => json(200, { mode: body.mode, approved_models: ["claude"], can_edit: true }),
    )
    render(<WorkspaceLLMDataCard />)
    const sw = await screen.findByRole("switch", { name: "Let Ask rsync.ai read sample rows" })
    expect(sw.getAttribute("aria-checked")).toBe("false")
    expect(screen.getByText(/allowed sample rows for: Claude/)).toBeTruthy()

    fireEvent.click(sw)
    const dialog = await screen.findByRole("alertdialog")
    expect(dialog.textContent).toMatch(/Claude, a third-party service/)
    expect(calls.some((c) => c.method === "PUT")).toBe(false)
    fireEvent.click(within(dialog).getByRole("button", { name: "Turn on" }))
    await waitFor(() => expect(sw.getAttribute("aria-checked")).toBe("true"))
    expect(calls.filter((c) => c.method === "PUT")).toEqual([
      { url: API_ENDPOINTS.ASSISTANT.WORKSPACE_LLM_DATA, method: "PUT", body: { mode: "sample_rows" } },
    ])

    fireEvent.click(sw)
    await waitFor(() => expect(sw.getAttribute("aria-checked")).toBe("false"))
    expect(calls.filter((c) => c.method === "PUT")[1].body).toEqual({ mode: "names_only" })
    expect(screen.queryByRole("alertdialog")).toBeNull()
  })

  it("a refused save says why, and shows what is saved", async () => {
    let gets = 0
    mockFetch.mockImplementation((_url: string, init?: RequestInit) => {
      if ((init?.method ?? "GET") === "PUT") {
        return Promise.resolve(json(403, { error: "insufficient workspace role" }))
      }
      gets++
      return Promise.resolve(json(200, { mode: "names_only", approved_models: ["chatgpt"], can_edit: true }))
    })
    render(<WorkspaceLLMDataCard />)
    fireEvent.click(await screen.findByRole("switch", { name: "Let Ask rsync.ai read sample rows" }))
    fireEvent.click(within(await screen.findByRole("alertdialog")).getByRole("button", { name: "Turn on" }))
    await waitFor(() =>
      expect(toast.error).toHaveBeenCalledWith("Could not save the setting: insufficient workspace role"),
    )
    await waitFor(() => expect(gets).toBe(2)) // re-read after the failure
    expect(screen.getByRole("switch").getAttribute("aria-checked")).toBe("false")
    expect(screen.queryByRole("alertdialog")).toBeNull()
  })

  it("anyone else sees the setting and no switch", async () => {
    serve(json(200, { mode: "sample_rows", approved_models: [], can_edit: false }))
    render(<WorkspaceLLMDataCard />)
    expect(await screen.findByText(/Sample rows are on\. Only the workspace owner can change this\./)).toBeTruthy()
    expect(screen.getByText(/No model is allowed sample rows yet/)).toBeTruthy()
    expect(screen.queryByRole("switch")).toBeNull()
  })

  it("an install without sample rows explains names only and offers no switch", async () => {
    featureFlagsManager.updateFlags({ llmSampleRows: false })
    serve(json(200, { mode: "names_only", approved_models: [], can_edit: true }))
    render(<WorkspaceLLMDataCard />)
    expect(await screen.findByText(/Sample rows are\s+turned off on this install\./)).toBeTruthy()
    await waitFor(() => expect(mockFetch).toHaveBeenCalled())
    expect(screen.queryByRole("switch")).toBeNull()
    expect(screen.queryByText(/With sample rows on/)).toBeNull()
  })

  it("shows the owner's switch only once /features has answered", async () => {
    featureFlagsManager.resetFlags() // the answer has not arrived
    serve(json(200, { mode: "names_only", approved_models: ["claude"], can_edit: true }))
    render(<WorkspaceLLMDataCard />)
    await waitFor(() => expect(mockFetch).toHaveBeenCalled())
    expect(await screen.findByText("Loading…")).toBeTruthy()
    expect(screen.queryByRole("switch")).toBeNull()
    featureFlagsManager.markResolved()
    expect(await screen.findByRole("switch", { name: "Let Ask rsync.ai read sample rows" })).toBeTruthy()
  })

  it("is not there while the assistant is off", async () => {
    serve(json(404, { error: "not found" }))
    const { container } = render(<WorkspaceLLMDataCard />)
    await waitFor(() => expect(mockFetch).toHaveBeenCalled())
    await waitFor(() => expect(container.innerHTML).toBe(""))
  })
})

describe("connection form notice", () => {
  it("names the models while sample rows are on", async () => {
    serve(json(200, { mode: "sample_rows", approved_models: ["claude", "gemini"], can_edit: false }))
    render(<SampleRowsNotice />)
    expect((await screen.findByRole("note")).textContent).toMatch(/may send Claude, Gemini up to 5 rows/)
  })

  it.each([
    ["names only", json(200, { mode: "names_only", approved_models: ["claude"], can_edit: false })],
    ["no approved model", json(200, { mode: "sample_rows", approved_models: [], can_edit: false })],
    ["assistant off", json(404, {})],
    ["unreadable", json(500, { error: "boom" })],
  ])("says nothing when %s", async (_, res) => {
    serve(res)
    render(<SampleRowsNotice />)
    await waitFor(() => expect(mockFetch).toHaveBeenCalled())
    await new Promise((r) => setTimeout(r, 0))
    expect(screen.queryByRole("note")).toBeNull()
  })
})

describe("looked at", () => {
  const base = { tool: "sample_rows", arguments: { connection_id: "c", table: "public.orders" }, ok: true }
  it("names the table and what was read, never a value", () => {
    expect(lookedAtLabel({ ...base, sample: { rows: 3, withheld_columns: 2 } })).toBe(
      "sample rows of public.orders (3 rows; 2 columns withheld)",
    )
    expect(lookedAtLabel({ ...base, sample: { rows: 1, withheld_columns: 0 } })).toBe("sample rows of public.orders (1 row)")
    expect(lookedAtLabel({ ...base, sample: { rows: 3, withheld_columns: 0, not_sent: true } })).toBe(
      "sample rows of public.orders (not sent to the model)",
    )
    expect(lookedAtLabel({ ...base, ok: false })).toBe("sample rows of public.orders (not read)")
    expect(lookedAtLabel({ tool: "list_pipelines", arguments: {}, ok: true })).toBe("your pipelines")
  })
})
