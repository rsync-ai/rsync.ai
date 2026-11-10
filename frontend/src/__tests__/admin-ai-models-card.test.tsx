import { afterEach, beforeAll, beforeEach, describe, expect, it, vi } from "vitest"
import type { Mock } from "vitest"
import { fireEvent, render, screen, waitFor, within } from "@testing-library/react"
import { toast } from "sonner"

import AdminSettingsPage from "@/app/(dashboard)/admin/settings/page"
import { AIModelsCard } from "@/components/admin/AIModelsCard"
import { authFetch } from "@/lib/api/auth-fetch"
import { API_ENDPOINTS } from "@/lib/config/api"
import { featureFlagsManager } from "@/config/features"

// Admin → Settings → AI models. api-gateway owns the rules (platform admin
// only, the key encrypted and never returned, every set/remove audited, the
// panel key over the environment's); what is pinned here is what only the
// card can get wrong:
//   - a key typed into the card is sent once and is never on the page again;
//   - Change with the key left empty sends no api_key, so the saved key stays;
//   - each entry says where its key comes from (here, the environment, none);
//   - Remove asks first; Test shows the model that answered or why it failed;
//   - the card appears only while /api/v1/features says assistant.

vi.mock("@/lib/api/auth-fetch", () => ({ authFetch: vi.fn(), authPost: vi.fn(), redirectToLogout: vi.fn() }))
vi.mock("sonner", () => ({ toast: { success: vi.fn(), error: vi.fn() } }))
vi.mock("next/navigation", () => ({
  useRouter: () => ({ push: vi.fn(), refresh: vi.fn() }),
  usePathname: () => "/admin/settings",
}))
vi.mock("@/lib/api/admin", () => ({
  adminGetSettings: vi.fn(() => Promise.resolve({ ok: true, data: { settings: { registration_mode: "open" } } })),
  adminUpdateSettings: vi.fn(),
}))

const mockFetch = authFetch as unknown as Mock
const KEY = "unit-test-provider-key-canary"

const claudePanel = {
  entry: "claude",
  label: "Claude",
  panel_key_set: true,
  model: "",
  default_model: "claude-sonnet-5-5",
  updated_by: "admin@example.com",
  updated_at: "2026-10-01T08:00:00Z",
  env_key_set: false,
  source: "panel",
}
const chatgptEnv = {
  entry: "chatgpt",
  label: "ChatGPT",
  panel_key_set: false,
  model: "",
  default_model: "gpt-5",
  env_key_set: true,
  env_model: "gpt-5-mini",
  source: "env",
}
const geminiNone = {
  entry: "gemini",
  label: "Gemini",
  panel_key_set: false,
  model: "",
  default_model: "gemini-2.5-flash",
  env_key_set: false,
  source: "none",
}

function json(status: number, body: unknown) {
  return {
    ok: status >= 200 && status < 300,
    status,
    headers: { get: () => null },
    json: () => Promise.resolve(body),
  } as unknown as Response
}

type Call = { url: string; method: string; body?: Record<string, unknown> }

// Routes authFetch by method; `entries` is what the next list returns.
function route(
  opts: { entries?: unknown[]; envKnown?: boolean; sampleRowsKnown?: boolean; list?: Response; put?: Response; test?: Response } = {},
) {
  const calls: Call[] = []
  const entries = opts.entries ?? [claudePanel, chatgptEnv, geminiNone]
  const list = () => json(200, { entries, env_known: opts.envKnown ?? true, sample_rows_known: opts.sampleRowsKnown })
  mockFetch.mockImplementation((url: string, init?: RequestInit) => {
    const method = init?.method ?? "GET"
    calls.push({ url, method, body: init?.body ? JSON.parse(String(init.body)) : undefined })
    if (url === "/api/v1/admin/settings") return Promise.resolve(json(200, { settings: { registration_mode: "open" } }))
    if (method === "PUT") return Promise.resolve(opts.put ?? list())
    if (method === "DELETE") return Promise.resolve(list())
    if (method === "POST") return Promise.resolve(opts.test ?? json(200, { ok: true, entry: "claude", label: "Claude", model: "claude-sonnet-5-5", source: "panel" }))
    return Promise.resolve(opts.list ?? list())
  })
  return calls
}

beforeAll(() => {
  window.ResizeObserver ||= class {
    observe() {}
    unobserve() {}
    disconnect() {}
  }
  if (!Element.prototype.hasPointerCapture) Element.prototype.hasPointerCapture = () => false
  if (!Element.prototype.scrollIntoView) Element.prototype.scrollIntoView = () => {}
})

beforeEach(() => {
  mockFetch.mockReset()
  ;(toast.success as Mock).mockReset()
  ;(toast.error as Mock).mockReset()
  featureFlagsManager.markResolved() // the cloud answer: sample rows allowed
})

afterEach(() => featureFlagsManager.resetFlags())

async function row(label: string) {
  return within(await screen.findByRole("listitem", { name: label }))
}

describe("AI models card", () => {
  it("sample rows: allowing one asks first and sends only that entry's switch; withdrawing does not ask", async () => {
    const calls = route({ entries: [claudePanel, { ...chatgptEnv, sample_rows: true }, geminiNone], sampleRowsKnown: true })
    render(<AIModelsCard />)
    const claudeSwitch = (await row("Claude")).getByRole("switch", { name: "Allow sample rows to Claude" })
    expect(claudeSwitch.getAttribute("aria-checked")).toBe("false")
    expect((await row("ChatGPT")).getByRole("switch").getAttribute("aria-checked")).toBe("true")

    fireEvent.click(claudeSwitch)
    const dialog = await screen.findByRole("alertdialog")
    expect(dialog.textContent).toMatch(/up to 5 rows/)
    expect(calls.some((c) => c.method === "PUT")).toBe(false) // nothing until confirmed
    fireEvent.click(within(dialog).getByRole("button", { name: "Allow sample rows" }))
    await waitFor(() => expect(calls.some((c) => c.method === "PUT")).toBe(true))
    expect(calls.filter((c) => c.method === "PUT")).toEqual([
      { url: API_ENDPOINTS.ASSISTANT.ADMIN_MODEL_SAMPLE_ROWS("claude"), method: "PUT", body: { allowed: true } },
    ])

    fireEvent.click((await row("ChatGPT")).getByRole("switch"))
    await waitFor(() => expect(calls.filter((c) => c.method === "PUT")).toHaveLength(2))
    expect(calls.filter((c) => c.method === "PUT")[1]).toEqual({
      url: API_ENDPOINTS.ASSISTANT.ADMIN_MODEL_SAMPLE_ROWS("chatgpt"),
      method: "PUT",
      body: { allowed: false },
    })
  })

  it("shows no sample-rows switch on an install that does not allow sample rows", async () => {
    featureFlagsManager.updateFlags({ llmSampleRows: false })
    route({ entries: [claudePanel, { ...chatgptEnv, sample_rows: true }, geminiNone], sampleRowsKnown: true })
    render(<AIModelsCard />)
    await row("Claude")
    expect(screen.queryByRole("switch")).toBeNull()
    expect(screen.getByText(/sample rows are turned off on this install/)).toBeTruthy()
  })

  it("shows no sample-rows switch when the approvals could not be read", async () => {
    route()
    render(<AIModelsCard />)
    await row("Claude")
    expect(screen.queryByRole("switch")).toBeNull()
  })

  it("says where each entry's key comes from, never showing one", async () => {
    route()
    const { container } = render(<AIModelsCard />)
    const claude = await row("Claude")
    expect(claude.getByText("Key saved here")).toBeTruthy()
    expect(claude.getByText(/Set by admin@example.com/)).toBeTruthy()
    expect(claude.getByText("claude-sonnet-5-5")).toBeTruthy()
    const chatgpt = await row("ChatGPT")
    expect(chatgpt.getByText("Key from the environment file")).toBeTruthy()
    expect(chatgpt.getByText("gpt-5-mini")).toBeTruthy()
    expect(chatgpt.queryByRole("button", { name: "Remove ChatGPT key" })).toBeNull()
    const gemini = await row("Gemini")
    expect(gemini.getByText("No key")).toBeTruthy()
    expect(gemini.queryByRole("button", { name: "Test Gemini" })).toBeNull()
    expect(container.querySelector('input[type="password"]')).toBeNull()
  })

  it("sends a typed key once, then clears it from the page", async () => {
    const calls = route()
    const { container } = render(<AIModelsCard />)
    fireEvent.click((await row("Gemini")).getByRole("button", { name: "Set Gemini key" }))

    const keyInput = screen.getByLabelText("Gemini API key") as HTMLInputElement
    expect(keyInput.type).toBe("password")
    expect(keyInput.value).toBe("")
    const save = screen.getByRole("button", { name: "Save" })
    expect((save as HTMLButtonElement).disabled).toBe(true) // no saved key, so one must be typed

    fireEvent.change(keyInput, { target: { value: `  ${KEY}  ` } })
    fireEvent.change(screen.getByLabelText("Model id (optional)"), { target: { value: "gemini-2.5-pro" } })
    fireEvent.click(save)

    await waitFor(() => expect(calls.some((c) => c.method === "PUT")).toBe(true))
    const put = calls.find((c) => c.method === "PUT")!
    expect(put.url).toBe(API_ENDPOINTS.ASSISTANT.ADMIN_MODEL("gemini"))
    expect(put.body).toEqual({ api_key: KEY, model: "gemini-2.5-pro" })
    await waitFor(() => expect(screen.queryByLabelText("Gemini API key")).toBeNull())
    expect(container.innerHTML).not.toContain(KEY)
    expect(toast.success).toHaveBeenCalledWith("Saved the Gemini key")
  })

  it("Change with the key left empty sends only the model id, so the saved key stays", async () => {
    const calls = route()
    render(<AIModelsCard />)
    fireEvent.click((await row("Claude")).getByRole("button", { name: "Set Claude key" }))
    const keyInput = screen.getByLabelText("Claude API key") as HTMLInputElement
    expect(keyInput.value).toBe("") // never prefilled
    fireEvent.change(screen.getByLabelText("Model id (optional)"), { target: { value: "claude-opus-5-5" } })
    fireEvent.click(screen.getByRole("button", { name: "Save" }))

    await waitFor(() => expect(calls.some((c) => c.method === "PUT")).toBe(true))
    const put = calls.find((c) => c.method === "PUT")!
    expect(put.body).toEqual({ model: "claude-opus-5-5" })
    expect("api_key" in put.body!).toBe(false)
  })

  it("shows the server's reason when a save is refused, and keeps the form open", async () => {
    route({ put: json(400, { error: "bad_request", message: "the key must be 8-512 printable characters" }) })
    render(<AIModelsCard />)
    fireEvent.click((await row("Gemini")).getByRole("button", { name: "Set Gemini key" }))
    fireEvent.change(screen.getByLabelText("Gemini API key"), { target: { value: "short key" } })
    fireEvent.click(screen.getByRole("button", { name: "Save" }))
    await waitFor(() => expect(toast.error).toHaveBeenCalledWith("the key must be 8-512 printable characters"))
    expect(screen.getByLabelText("Gemini API key")).toBeTruthy()
  })

  it("Test shows the model that answered, or why it failed", async () => {
    route()
    render(<AIModelsCard />)
    fireEvent.click((await row("Claude")).getByRole("button", { name: "Test Claude" }))
    expect(await screen.findByText("Claude answered with claude-sonnet-5-5.")).toBeTruthy()

    route({ test: json(200, { ok: false, entry: "chatgpt", error: "provider_error", message: "invalid api key" }) })
    fireEvent.click((await row("ChatGPT")).getByRole("button", { name: "Test ChatGPT" }))
    expect(await screen.findByText("The test failed: invalid api key")).toBeTruthy()
  })

  it("Remove asks first, sends DELETE for that entry, and Cancel sends nothing", async () => {
    const calls = route()
    render(<AIModelsCard />)
    fireEvent.click((await row("Claude")).getByRole("button", { name: "Remove Claude key" }))
    const dialog = await screen.findByRole("alertdialog")
    expect(dialog.textContent).toMatch(/leaves the assistant's picker/)
    fireEvent.click(within(dialog).getByRole("button", { name: /cancel/i }))
    await waitFor(() => expect(screen.queryByRole("alertdialog")).toBeNull())
    expect(calls.some((c) => c.method === "DELETE")).toBe(false)

    fireEvent.click((await row("Claude")).getByRole("button", { name: "Remove Claude key" }))
    fireEvent.click(within(await screen.findByRole("alertdialog")).getByRole("button", { name: "Remove key" }))
    await waitFor(() => expect(calls.some((c) => c.method === "DELETE")).toBe(true))
    expect(calls.find((c) => c.method === "DELETE")!.url).toBe(API_ENDPOINTS.ASSISTANT.ADMIN_MODEL("claude"))
    await waitFor(() => expect(screen.queryByRole("alertdialog")).toBeNull())
  })

  it("a failed read offers Retry, never an empty list", async () => {
    route({ list: json(500, { error: "internal" }) })
    render(<AIModelsCard />)
    expect(await screen.findByText("Could not load the AI models.")).toBeTruthy()
    expect(screen.queryByRole("listitem")).toBeNull()

    route()
    fireEvent.click(screen.getByRole("button", { name: "Retry" }))
    expect(await screen.findByRole("listitem", { name: "Claude" })).toBeTruthy()
  })

  it("says when the environment's keys are unknown", async () => {
    route({ envKnown: false })
    render(<AIModelsCard />)
    expect(await screen.findByText(/keys from the environment file are not shown/)).toBeTruthy()
  })
})

describe("Admin Settings page", () => {
  afterEach(() => featureFlagsManager.resetFlags())

  it("shows the AI models card only while the assistant is on", async () => {
    route()
    const { unmount } = render(<AdminSettingsPage />)
    await screen.findByText("Registration")
    expect(screen.queryByText("AI models")).toBeNull()
    expect(mockFetch.mock.calls.some(([u]) => u === API_ENDPOINTS.ASSISTANT.ADMIN_MODELS)).toBe(false)
    unmount()

    featureFlagsManager.updateFlags({ assistant: true })
    render(<AdminSettingsPage />)
    expect(await screen.findByText("AI models")).toBeTruthy()
    expect(await screen.findByRole("listitem", { name: "Claude" })).toBeTruthy()
  })
})
