/**
 * The Checks tab on a model's page (ModelChecksTab.tsx): the checks with their last
 * results, the add/edit form, Test now, the admin approval of a custom SQL check, and a
 * draft handed over by Ask rsync.ai.
 */

import { afterEach, beforeEach, describe, expect, it, vi } from "vitest"
import type { Mock } from "vitest"
import { act, cleanup, render, screen, waitFor, within } from "@testing-library/react"
import userEvent from "@testing-library/user-event"
import "@testing-library/jest-dom"
import { toast } from "sonner"

import { authFetch } from "@/lib/api/auth-fetch"
import { ModelChecksTab, columnChoice } from "@/components/explorer/ModelChecksTab"
import { emptyDraft, storeAssistantDraft, takeAssistantDraft, type ModelCheck, type ModelChecksResponse } from "@/components/explorer/modelChecks"

vi.mock("@/lib/api/auth-fetch", () => ({ authFetch: vi.fn() }))
vi.mock("sonner", () => ({ toast: { success: vi.fn(), error: vi.fn(), info: vi.fn() } }))

const mockFetch = authFetch as unknown as Mock
const ID = "6f1c0a52-3c52-4a0e-9d7c-2d2f0f6a1b11"
const CONN = "0b0c3d1e-4f5a-4b6c-8d7e-9f0a1b2c3d4e"
const CHECKS = `/api/v1/explorer/saved/${ID}/checks`

function res(body: unknown, status = 200) {
  return { ok: status < 400, status, json: async () => body } as unknown as Response
}

const notNull: ModelCheck = {
  check_id: "c1",
  name: "order id present",
  type: "not_null",
  columns: ["order_id"],
  params: {},
  severity: "block",
  enabled: true,
  history: [
    { run_id: "r2", finished_at: "2026-10-03T10:00:00Z", status: "fail", severity: "block", observed: 3, threshold: "no NULLs" },
    { run_id: "r1", finished_at: "2026-10-02T10:00:00Z", status: "pass", severity: "block", observed: 0, threshold: "no NULLs" },
  ],
}

function listing(over: Partial<ModelChecksResponse> = {}): ModelChecksResponse {
  return { checks: [notNull], pending: [], max_checks: 20, can_block: true, ...over }
}

/** The gateway: GET lists, the schema index names the table's columns, writes answer `writes`. */
function gateway(list: ModelChecksResponse, writes: Record<string, { status: number; body: unknown }> = {}) {
  mockFetch.mockImplementation(async (url: string, init?: RequestInit) => {
    const method = init?.method ?? "GET"
    const w = writes[`${method} ${url}`]
    if (w) return res(w.body, w.status)
    if (method === "GET" && url === CHECKS) return res(list)
    if (url === `/api/v1/explorer/connections/${CONN}/schema-index`) {
      return res({ tables: [{ name: "orders", schema: "analytics", columns: [{ name: "order_id" }, { name: "region" }] }] })
    }
    return res({ error: "unexpected" }, 500)
  })
}

function calls(method: string, url: string) {
  return mockFetch.mock.calls
    .filter(([u, init]) => u === url && ((init as RequestInit | undefined)?.method ?? "GET") === method)
    .map(([, init]) => {
      const body = (init as RequestInit | undefined)?.body
      return body ? JSON.parse(String(body)) : undefined
    })
}

function renderTab(props: Partial<Parameters<typeof ModelChecksTab>[0]> = {}) {
  return render(
    <ModelChecksTab
      modelId={ID}
      connectionId={CONN}
      targetTable={'"analytics"."orders"'}
      scheduled
      canEdit
      isAdmin={false}
      userId="u1"
      {...props}
    />,
  )
}

beforeEach(() => {
  mockFetch.mockReset()
  vi.mocked(toast.success).mockReset()
  vi.mocked(toast.info).mockReset()
  sessionStorage.clear()
})
afterEach(() => cleanup())

describe("the list", () => {
  it("shows each check, what it holds and what the last run found", async () => {
    gateway(listing())
    renderTab()
    const row = await screen.findByTestId("model-check")
    expect(row).toHaveTextContent("order id present")
    expect(row).toHaveTextContent("Not null")
    expect(row).toHaveTextContent("Blocks")
    expect(row).toHaveTextContent("order_id has no NULLs")
    expect(row).toHaveTextContent("Failed")
    expect(row).toHaveTextContent("found 3 NULL rows")

    await userEvent.setup().click(within(row).getByRole("button", { name: "History" }))
    const history = within(row).getByRole("list", { name: "order id present history" })
    expect(within(history).getAllByRole("listitem")).toHaveLength(2)
  })

  it("says a check only warns where the engine cannot block", async () => {
    gateway(listing({ can_block: false, cannot_block_reason: "this engine can't hold a table back yet — add it as a warning" }))
    renderTab()
    const row = await screen.findByTestId("model-check")
    expect(row).toHaveTextContent("Warns")
    expect(screen.getByTestId("model-checks-card")).toHaveTextContent("Checks on this model can only warn")
  })

  it("names the engine that cannot block, and promises no blocking where it cannot", async () => {
    gateway(
      listing({
        checks: [],
        can_block: false,
        cannot_block_reason: "this engine can't hold a table back yet — add it as a warning",
        connector_type: "bigquery",
      }),
    )
    const user = userEvent.setup()
    renderTab()
    await screen.findByText(/On BigQuery, checks can only warn/)
    const card = screen.getByTestId("model-checks-card")
    expect(card).not.toHaveTextContent(/stop a bad build|leaves the live table as it was/)
    expect(card).toHaveTextContent("Add one to be alerted when a build looks wrong.")

    await user.click(screen.getByRole("button", { name: "Add check" }))
    const dialog = await screen.findByRole("dialog")
    expect(within(dialog).getByRole("combobox", { name: "When it fails" })).toBeDisabled()
    expect(dialog).toHaveTextContent("On BigQuery, checks can only warn")
  })

  it("offers no change to someone who cannot edit the model", async () => {
    gateway(listing())
    renderTab({ canEdit: false })
    await screen.findByTestId("model-check")
    expect(screen.queryByRole("button", { name: "Add check" })).toBeNull()
    expect(screen.queryByRole("button", { name: "Edit order id present" })).toBeNull()
    // Test now records nothing, so a viewer may run it.
    expect(screen.getByRole("button", { name: "Test now" })).toBeEnabled()
  })

  it("says why a model cannot have checks", async () => {
    gateway(listing({ checks: [], can_block: false, unsupported_reason: "a statement model builds no table" }))
    renderTab()
    expect(await screen.findByText(/a statement model builds no table/)).toBeInTheDocument()
    expect(screen.queryByRole("button", { name: "Add check" })).toBeNull()
  })

  it("stops adding at the cap", async () => {
    gateway(listing({ max_checks: 1 }))
    renderTab()
    expect(await screen.findByRole("button", { name: "Add check" })).toBeDisabled()
  })
})

describe("Test now", () => {
  it("lists what each check found, recording nothing", async () => {
    gateway(listing(), {
      [`POST ${CHECKS}/test`]: {
        status: 200,
        body: {
          results: [{ check_id: "c1", name: "order id present", type: "not_null", severity: "block", status: "pass", observed: 0, threshold: "no NULLs" }],
          tested_at: "2026-10-03T11:00:00Z",
        },
      },
    })
    const user = userEvent.setup()
    renderTab()
    await user.click(await screen.findByRole("button", { name: "Test now" }))
    const box = await screen.findByTestId("model-checks-test")
    expect(within(box).getByRole("list", { name: "Check results" })).toHaveTextContent("Found 0 NULL rows · allowed: no NULLs")
    expect(box).toHaveTextContent("it records nothing")
  })

  it("opens a check's card from its result, and the card says what the test found", async () => {
    const scrolled = vi.spyOn(Element.prototype, "scrollIntoView").mockImplementation(() => {})
    gateway(listing(), {
      [`POST ${CHECKS}/test`]: {
        status: 200,
        body: {
          results: [{ check_id: "c1", name: "order id present", type: "not_null", severity: "block", status: "pass", observed: 0, threshold: "no NULLs" }],
        },
      },
    })
    const user = userEvent.setup()
    renderTab()
    const row = await screen.findByTestId("model-check")
    expect(within(row).queryByTestId("model-check-tested")).toBeNull()
    await user.click(screen.getByRole("button", { name: "Test now" }))
    const box = await screen.findByTestId("model-checks-test")

    // A test records nothing, so the run line still says what the last run found; the
    // test's own result is said beside it.
    expect(row).toHaveTextContent("found 3 NULL rows")
    expect(within(row).getByTestId("model-check-tested")).toHaveTextContent("Test now:Passedfound 0 NULL rows· allowed: no NULLs")

    await user.click(within(box).getByRole("button", { name: /order id present/ }))
    expect(within(row).getByRole("list", { name: "order id present history" })).toBeInTheDocument()
    expect(scrolled.mock.contexts).toContain(row)

    await user.click(within(box).getByRole("button", { name: "Close" }))
    expect(within(row).queryByTestId("model-check-tested")).toBeNull()
    scrolled.mockRestore()
  })

  it("shows the gateway's refusal", async () => {
    gateway(listing(), { [`POST ${CHECKS}/test`]: { status: 400, body: { error: "the table does not exist yet" } } })
    const user = userEvent.setup()
    renderTab()
    await user.click(await screen.findByRole("button", { name: "Test now" }))
    expect(await screen.findByText("the table does not exist yet")).toBeInTheDocument()
  })
})

describe("the form", () => {
  it("adds a check on a column picked from the table's schema", async () => {
    gateway(listing({ checks: [] }), { [`POST ${CHECKS}`]: { status: 201, body: { check_id: "c9" } } })
    const user = userEvent.setup()
    renderTab()
    await user.click(await screen.findByRole("button", { name: "Add check" }))
    const dialog = await screen.findByRole("dialog")
    await user.type(within(dialog).getByRole("textbox", { name: "Name" }), "region present")
    await user.selectOptions(within(dialog).getByRole("combobox", { name: "Check type" }), "not_null")
    // The quoted target is found under its bare name, so its columns are offered.
    await user.selectOptions(await within(dialog).findByRole("combobox", { name: "Column" }), "region")
    await user.click(within(dialog).getByRole("button", { name: "Add check" }))

    await waitFor(() => expect(screen.queryByRole("dialog")).toBeNull())
    expect(calls("POST", CHECKS)).toEqual([
      { name: "region present", type: "not_null", columns: ["region"], params: {}, severity: "block", enabled: true },
    ])
    expect(toast.success).toHaveBeenCalledWith("Added region present")
  })

  it("tests the check as it would save, before it is saved", async () => {
    gateway(listing({ checks: [] }), {
      [`POST ${CHECKS}/test`]: {
        status: 200,
        body: { results: [{ name: "region present", type: "not_null", severity: "block", status: "fail", observed: 2, threshold: "no NULLs" }] },
      },
    })
    const user = userEvent.setup()
    renderTab()
    await user.click(await screen.findByRole("button", { name: "Add check" }))
    const dialog = await screen.findByRole("dialog")
    await user.type(within(dialog).getByRole("textbox", { name: "Name" }), "region present")
    await user.selectOptions(within(dialog).getByRole("combobox", { name: "Check type" }), "not_null")
    await user.selectOptions(await within(dialog).findByRole("combobox", { name: "Column" }), "region")
    await user.click(within(dialog).getByRole("button", { name: "Test" }))

    const result = await within(dialog).findByRole("region", { name: "Test result" })
    expect(result).toHaveTextContent("Failed")
    expect(result).toHaveTextContent("Found 2 NULL rows · allowed: no NULLs")
    expect(result).toHaveTextContent("Nothing was saved")
    expect(calls("POST", `${CHECKS}/test`)).toEqual([
      { check: { name: "region present", type: "not_null", columns: ["region"], params: {}, severity: "block", enabled: true } },
    ])
    expect(calls("POST", CHECKS)).toEqual([])
    expect(screen.getByRole("dialog")).toBeInTheDocument()

    // A change makes the result stale: it goes until the next Test.
    await user.type(within(dialog).getByRole("textbox", { name: "Name" }), "!")
    expect(within(dialog).queryByRole("region", { name: "Test result" })).toBeNull()
  })

  // N-6 (prod 2026-10-10): Test with no name answered "a check needs a name". Saving needs
  // one; trying the check does not.
  it("tests a draft that has no name yet, under its type's name", async () => {
    gateway(listing({ checks: [] }), { [`POST ${CHECKS}/test`]: { status: 200, body: { results: [] } } })
    const user = userEvent.setup()
    renderTab()
    await user.click(await screen.findByRole("button", { name: "Add check" }))
    const dialog = await screen.findByRole("dialog")
    await user.click(within(dialog).getByRole("button", { name: "Test" }))
    await within(dialog).findByRole("region", { name: "Test result" })
    expect(within(dialog).queryByRole("alert")).toBeNull()
    expect(calls("POST", `${CHECKS}/test`)).toEqual([{ check: expect.objectContaining({ name: "Row count", type: "row_count" }) }])
    // The field stays empty: the type's name is only what the test reports under.
    expect(within(dialog).getByRole("textbox", { name: "Name" })).toHaveValue("")
  })

  it("says what is wrong with a draft before testing it, as a sentence", async () => {
    gateway(listing({ checks: [] }))
    const user = userEvent.setup()
    renderTab()
    await user.click(await screen.findByRole("button", { name: "Add check" }))
    const dialog = await screen.findByRole("dialog")
    await user.selectOptions(within(dialog).getByRole("combobox", { name: "Check type" }), "not_null")
    await user.click(within(dialog).getByRole("button", { name: "Test" }))
    expect(within(dialog).getByRole("alert")).toHaveTextContent(/^A not null check takes exactly one column$/)
    expect(calls("POST", `${CHECKS}/test`)).toEqual([])
  })

  it("says what is wrong before asking the gateway", async () => {
    gateway(listing({ checks: [] }))
    const user = userEvent.setup()
    renderTab()
    await user.click(await screen.findByRole("button", { name: "Add check" }))
    const dialog = await screen.findByRole("dialog")
    await user.click(within(dialog).getByRole("button", { name: "Add check" }))
    expect(within(dialog).getByRole("alert")).toHaveTextContent("needs a name")
    expect(calls("POST", CHECKS)).toEqual([])
  })

  it("shows the gateway's refusal in the form", async () => {
    gateway(listing({ checks: [] }), {
      [`POST ${CHECKS}`]: { status: 409, body: { error: "this model already has a check named x" } },
    })
    const user = userEvent.setup()
    renderTab()
    await user.click(await screen.findByRole("button", { name: "Add check" }))
    const dialog = await screen.findByRole("dialog")
    await user.type(within(dialog).getByRole("textbox", { name: "Name" }), "x")
    await user.click(within(dialog).getByRole("button", { name: "Add check" }))
    expect(await within(dialog).findByRole("alert")).toHaveTextContent("already has a check named x")
  })

  it("says a custom SQL check on a scheduled model waits for an admin", async () => {
    const reason = "This model is on a schedule, and a custom SQL check runs as the schedule's user."
    gateway(listing({ checks: [] }), {
      [`POST ${CHECKS}`]: { status: 200, body: { pending_approval: { id: "p1", check_id: "c9", reason } } },
    })
    const user = userEvent.setup()
    renderTab()
    await user.click(await screen.findByRole("button", { name: "Add check" }))
    const dialog = await screen.findByRole("dialog")
    await user.type(within(dialog).getByRole("textbox", { name: "Name" }), "no negatives")
    await user.selectOptions(within(dialog).getByRole("combobox", { name: "Check type" }), "custom_sql")
    await user.type(within(dialog).getByRole("textbox", { name: "Check query" }), "amount < 0")
    await user.type(within(dialog).getByRole("textbox", { name: "Note for the approver (optional)" }), "finance asked")
    await user.click(within(dialog).getByRole("button", { name: "Add check" }))

    await waitFor(() => expect(toast.info).toHaveBeenCalledWith(reason))
    expect(calls("POST", CHECKS)[0]).toMatchObject({
      type: "custom_sql",
      sql_text: "SELECT *\nFROM {{this}}\nWHERE amount < 0",
      note: "finance asked",
    })
  })

  it("turns a check off with the whole check", async () => {
    gateway(listing(), { [`PUT ${CHECKS}/c1`]: { status: 200, body: { check_id: "c1" } } })
    const user = userEvent.setup()
    renderTab()
    await user.click(await screen.findByRole("button", { name: "Turn off" }))
    await waitFor(() =>
      expect(calls("PUT", `${CHECKS}/c1`)).toEqual([
        { name: "order id present", type: "not_null", columns: ["order_id"], params: {}, severity: "block", enabled: false },
      ]),
    )
  })
})

describe("a custom SQL check waiting for approval", () => {
  const pending = listing({
    pending: [
      {
        id: "p1",
        check_id: "c1",
        sql_text: "SELECT * FROM {{this}} WHERE amount < 0",
        base_sql_text: "",
        check: { name: "order id present", type: "custom_sql", columns: [], params: {}, severity: "block", enabled: true },
        note: "finance asked",
        proposed_by: "u2",
        proposed_at: "2026-10-03T09:00:00Z",
      },
    ],
  })

  it("lets an admin approve it, with a note", async () => {
    gateway(pending, { [`POST ${CHECKS}/pending/p1/approve`]: { status: 200, body: { status: "approved" } } })
    const user = userEvent.setup()
    renderTab({ isAdmin: true })
    const card = await screen.findByTestId("model-check-proposal")
    expect(card).toHaveTextContent("SELECT * FROM {{this}} WHERE amount < 0")
    expect(card).toHaveTextContent("Note: finance asked")
    // A check with a change waiting cannot be edited past it.
    expect(screen.getByRole("button", { name: "Edit order id present" })).toBeDisabled()

    await user.type(within(card).getByRole("textbox", { name: "Review note" }), "ok")
    await user.click(within(card).getByRole("button", { name: "Approve" }))
    await waitFor(() => expect(calls("POST", `${CHECKS}/pending/p1/approve`)).toEqual([{ note: "ok" }]))
  })

  it("shows anyone else that it waits", async () => {
    gateway(pending)
    renderTab({ isAdmin: false })
    const card = await screen.findByTestId("model-check-proposal")
    expect(within(card).queryByRole("button", { name: "Approve" })).toBeNull()
    expect(card).toHaveTextContent("A workspace admin approves or rejects it.")
  })
})

describe("a draft from Ask rsync.ai", () => {
  it("is listed for review, and fills the form", async () => {
    const draft = { ...emptyDraft(true, "unique"), name: "one row per order", columns: ["order_id"] }
    storeAssistantDraft({ modelId: ID, drafts: [draft] })
    gateway(listing({ checks: [] }), { [`POST ${CHECKS}`]: { status: 201, body: { check_id: "c9" } } })
    const user = userEvent.setup()
    renderTab()
    const card = await screen.findByTestId("model-checks-ask-drafts")
    expect(card).toHaveTextContent("one row per order · Unique · order_id · blocks")
    await user.click(within(card).getByRole("button", { name: "Review and add" }))
    const dialog = await screen.findByRole("dialog")
    expect(within(dialog).getByRole("textbox", { name: "Name" })).toHaveValue("one row per order")
    await user.click(within(dialog).getByRole("button", { name: "Add check" }))

    await waitFor(() => expect(screen.queryByTestId("model-checks-ask-drafts")).toBeNull())
    expect(calls("POST", CHECKS)[0]).toMatchObject({ name: "one row per order", type: "unique", columns: ["order_id"] })
  })
})

describe("a draft from Ask rsync.ai, with the tab already open", () => {
  it("is taken as the card stores it", async () => {
    gateway(listing({ checks: [] }))
    renderTab()
    await screen.findByText("No checks yet. Add one to stop a bad build from replacing the table.")
    expect(screen.queryByTestId("model-checks-ask-drafts")).toBeNull()

    // Another model's draft is left for that model's page.
    act(() => storeAssistantDraft({ modelId: "other", drafts: [{ ...emptyDraft(true, "row_count"), name: "theirs" }] }))
    expect(screen.queryByTestId("model-checks-ask-drafts")).toBeNull()

    act(() => storeAssistantDraft({ modelId: ID, drafts: [{ ...emptyDraft(true, "row_count"), name: "has rows" }] }))
    expect(await screen.findByTestId("model-checks-ask-drafts")).toHaveTextContent("has rows")
    expect(takeAssistantDraft(ID)).toBeNull()
    expect(takeAssistantDraft("other")?.[0].name).toBe("theirs")
  })
})

describe("columnChoice", () => {
  const ready = { status: "ready" as const, tables: [{ name: "orders", schema: "analytics", columns: [{ name: "id" }] }] }
  it("finds a quoted or bracketed table under its bare name", () => {
    expect(columnChoice('"analytics"."orders"', ready)).toEqual({ status: "columns", columns: ["id"] })
    expect(columnChoice("[analytics].[orders]", ready)).toEqual({ status: "columns", columns: ["id"] })
  })
  it("asks for typed names where the schema cannot say", () => {
    expect(columnChoice("analytics.missing", ready)).toMatchObject({ status: "typed" })
    expect(columnChoice(undefined, ready)).toMatchObject({ status: "typed" })
    expect(columnChoice("orders", { status: "error", message: "boom" })).toEqual({ status: "typed", note: "boom" })
    expect(columnChoice("orders", undefined)).toEqual({ status: "loading" })
  })
})
