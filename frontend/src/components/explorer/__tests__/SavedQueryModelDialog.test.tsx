import { describe, it, expect, vi, beforeEach } from "vitest"
import { render, screen, waitFor, within } from "@testing-library/react"
import userEvent from "@testing-library/user-event"

vi.mock("@/lib/api/auth-fetch", () => ({ authFetch: vi.fn() }))
vi.mock("sonner", () => ({ toast: { success: vi.fn(), error: vi.fn(), info: vi.fn() } }))

// The dialog gates every mutation on the workspace role. Without a provider the real
// hook fails closed (role ""), which would disable the controls under test and make
// each assertion pass for the wrong reason — so the role is an explicit knob here.
const mockRole = { value: "admin" }
vi.mock("@/contexts/WorkspaceContext", () => ({
  useWorkspaceRole: () => ({
    role: mockRole.value,
    isLoading: false,
    error: false,
    activeWorkspace: null,
    can: () => mockRole.value === "admin" || mockRole.value === "owner",
    meets: (min: string) => {
      const rank: Record<string, number> = { viewer: 1, member: 2, admin: 3, owner: 4 }
      return (rank[mockRole.value] ?? 0) >= (rank[min] ?? 99)
    },
  }),
}))

// The workspace's layer list is its own request (useWorkspaceLayers, tested through
// the Workspace settings card): here it is a knob, so a test sets the list it needs
// and the fetch mocks below see only the dialog's own requests.
const LAYERS = [
  { id: "l-s", name: "Silver", description: "Cleaned from one pipeline table.", position: 0, model_count: 2 },
  { id: "l-g", name: "Gold", description: "Joined or aggregated from Silver models.", position: 1, model_count: 1 },
]
const mockLayers: {
  value: { layers: typeof LAYERS | null; canManage: boolean; error: string | null; reload: () => Promise<void> }
} = { value: { layers: LAYERS, canManage: true, error: null, reload: async () => {} } }
vi.mock("@/lib/hooks/useWorkspaceLayers", () => ({ useWorkspaceLayers: () => mockLayers.value }))

// "Where it sits" reads the asset graph, its own request (tested in ModelPlacement.test.tsx).
// A knob for the same reason as the layers: the fetch mocks below see only the dialog's own
// requests, so "something was submitted" keeps meaning what it says.
const mockSurroundings: { value: ModelSurroundings | null } = { value: null }
vi.mock("@/components/explorer/ModelPlacement", async (orig) => ({
  ...(await orig<typeof import("@/components/explorer/ModelPlacement")>()),
  useModelSurroundings: () => mockSurroundings.value,
}))

import { authFetch } from "@/lib/api/auth-fetch"
import { toast } from "sonner"
import { SavedQueryModelDialog, type ModelSchedule } from "@/components/explorer/SavedQueryModelDialog"
import type { ModelSurroundings } from "@/components/explorer/assetLineage"

// A model rebuilds a real table unattended. The three properties pinned here are the
// ones whose failure is silent:
//
//  1. the UI never offers a schedule before there is a target to write to — the
//     backend refuses that combination, so offering it produces a schedule that can
//     only fail;
//  2. an auto-pause is visible AND its refusal survives a Resume click, because an
//     auto-pause is a security stop, not a hiccup;
//  3. a rebuild the engine rejected reports what the engine said, not "something
//     went wrong" — the whole value of a failed run is its message.

const QUERY_ID = "7f1b9c62-2f4a-4b3e-9c11-2d5e6f7a8b90"

function res(status: number, body: unknown) {
  return {
    ok: status >= 200 && status < 300,
    status,
    json: async () => body,
  } as unknown as Response
}

const mockFetch = authFetch as unknown as ReturnType<typeof vi.fn>

function schedule(overrides: Partial<ModelSchedule> = {}): ModelSchedule {
  return {
    schedule_id: "b2c3d4e5-1111-2222-3333-444455556666",
    saved_query_id: QUERY_ID,
    schedule_type: "cron",
    schedule_spec: { cron: "0 2 * * *", timezone: "UTC" },
    status: "paused",
    run_as_user_id: "11111111-1111-1111-1111-111111111111",
    created_at: "2026-08-13T00:00:00Z",
    updated_at: "2026-08-13T00:00:00Z",
    ...overrides,
  }
}

function renderDialog(props: Partial<React.ComponentProps<typeof SavedQueryModelDialog>> = {}) {
  return render(
    <SavedQueryModelDialog
      savedQueryId={QUERY_ID}
      savedQueryName="Daily MRR"
      materialization="table"
      targetTable="analytics.daily_mrr"
      open
      onOpenChange={vi.fn()}
      onChanged={vi.fn()}
      {...props}
    />
  )
}

beforeEach(() => {
  vi.clearAllMocks()
  mockRole.value = "admin"
  mockLayers.value = { layers: LAYERS, canManage: true, error: null, reload: async () => {} }
  mockSurroundings.value = null
})

describe("SavedQueryModelDialog", () => {
  it("shows the schedule editor with no target yet, but will not submit one", async () => {
    // No schedule for this query.
    mockFetch.mockResolvedValue(res(404, { error: "no schedule for this saved query" }))

    const user = userEvent.setup({ delay: null })
    renderDialog({ materialization: "none", targetTable: "" })

    // A run that does nothing has nothing to schedule, and the page says so in place of
    // the choices — a reason on the page, not in a title a keyboard user never sees.
    expect(await screen.findByText(/a schedule that writes nowhere can only fail/i)).toBeInTheDocument()
    expect(screen.queryByRole("button", { name: /save and schedule/i })).not.toBeInTheDocument()
    // Run now is equally meaningless without a destination.
    expect(screen.getByRole("button", { name: /run now/i })).toBeDisabled()

    // The editor is VISIBLE once a run does something — hiding it behind a saved target
    // is what made this read as a dialog with two stages. But the backend refuses a
    // schedule with no target, so the button must not submit one, and says why.
    await user.click(screen.getByRole("radio", { name: /rebuild a table/i }))
    expect(screen.getByRole("button", { name: /save and schedule/i })).toBeDisabled()
    expect(screen.getByText(/name the table it builds/i)).toBeInTheDocument()
  })

  // The point of collapsing the gate: one click does the PUT then the POST. If this
  // regresses to a single call, the server answers 400 and the user is back to
  // guessing that a different button had to be pressed first.
  it("saves the target and creates the schedule from one click, in that order", async () => {
    const calls: Array<{ url: string; method: string }> = []
    mockFetch.mockImplementation(async (url: string, init?: RequestInit) => {
      const method = init?.method ?? "GET"
      if (url.endsWith("/schedule") && method === "GET") {
        return res(404, { error: "no schedule for this saved query" })
      }
      if (url.includes("/pipelines")) return res(200, { pipelines: [] })
      if (url.endsWith("/explorer/saved")) return res(200, { saved_queries: [] })
      calls.push({ url, method })
      if (url.endsWith("/materialization")) {
        // The server normalises the name; the dialog must adopt THIS value.
        return res(200, { materialization: "table", target_table: "analytics.daily_mrr" })
      }
      return res(200, { schedule_id: "b2c3d4e5-1111-2222-3333-444455556666" })
    })

    const user = userEvent.setup({ delay: null })
    renderDialog({ materialization: "none", targetTable: "" })

    // Loaded: the schedule lookup has answered "none yet".
    await screen.findByText(/a schedule that writes nowhere can only fail/i)
    await user.click(screen.getByRole("radio", { name: /rebuild a table/i }))
    // paste, not type: the field is controlled, so type() re-renders the whole
    // dialog once per character. 19 characters cost ~1.5s here, which is what
    // pushed this file past the 5s budget on a loaded CI runner. Nothing in
    // these tests depends on per-keystroke behaviour, only the final value.
    await user.click(screen.getByLabelText("Table it builds"))
    await user.paste("analytics.daily_mrr")
    await user.click(screen.getByRole("button", { name: /save and schedule/i }))

    await waitFor(() => {
      expect(calls.length).toBe(2)
    })
    expect(calls[0].method).toBe("PUT")
    expect(calls[0].url).toContain("/materialization")
    expect(calls[1].method).toBe("POST")
    expect(calls[1].url).toContain("/schedule")
  })

  // A failed target save must stop the sequence. Posting the schedule anyway earns a
  // 400 and a second error toast for one click, which reads as two separate faults.
  it("does not create the schedule when saving the target fails", async () => {
    const calls: Array<{ url: string; method: string }> = []
    mockFetch.mockImplementation(async (url: string, init?: RequestInit) => {
      const method = init?.method ?? "GET"
      if (url.endsWith("/schedule") && method === "GET") {
        return res(404, { error: "no schedule for this saved query" })
      }
      calls.push({ url, method })
      return res(400, { error: "target_table must be schema.table" })
    })

    // Pasted rather than typed, for the reason given on the test above.
    const user = userEvent.setup({ delay: null })
    renderDialog({ materialization: "none", targetTable: "" })

    // Loaded: the schedule lookup has answered "none yet".
    await screen.findByText(/a schedule that writes nowhere can only fail/i)
    await user.click(screen.getByRole("radio", { name: /rebuild a table/i }))
    await user.click(screen.getByLabelText("Table it builds"))
    await user.paste("not a table name")
    await user.click(screen.getByRole("button", { name: /save and schedule/i }))

    await waitFor(() => {
      expect(toast.error).toHaveBeenCalledWith("target_table must be schema.table")
    })
    expect(calls.filter((c) => c.method === "POST")).toHaveLength(0)
  })

  // Rendering a control that can only 403 teaches people the product is broken. The
  // server is still the authority — this is about not offering the click.
  it("disables every mutating control for a non-admin, and says why", async () => {
    mockRole.value = "member"
    mockFetch.mockResolvedValue(res(200, schedule({ status: "active" })))

    renderDialog()

    await waitFor(() => {
      expect(screen.getByText(/needs the\s+Admin or Owner role/i)).toBeInTheDocument()
    })
    expect(screen.getByRole("button", { name: /run now/i })).toBeDisabled()
    expect(screen.getByRole("button", { name: "Change what a run does" })).toBeDisabled()
    expect(screen.getByRole("button", { name: "Change when it runs" })).toBeDisabled()
    expect(screen.getByRole("button", { name: /pause/i })).toBeDisabled()
    expect(screen.getByRole("button", { name: /remove schedule/i })).toBeDisabled()
    // Nothing to save: with every Change closed there is no edit in progress.
    expect(screen.queryByRole("button", { name: /save changes/i })).not.toBeInTheDocument()
    // Still READABLE: a viewer may see what is scheduled, which is why the settings
    // render at all rather than being hidden behind the role.
    expect(screen.getByText("Every day at 02:00 (UTC)")).toBeInTheDocument()
  })

  it("leaves the controls usable for an admin", async () => {
    mockFetch.mockResolvedValue(res(200, schedule({ status: "active" })))

    renderDialog()

    await waitFor(() => {
      expect(screen.getByRole("button", { name: "Change what a run does" })).toBeEnabled()
    })
    expect(screen.getByRole("button", { name: "Change when it runs" })).toBeEnabled()
    expect(screen.queryByText(/needs the\s+Admin or Owner role/i)).not.toBeInTheDocument()
  })

  // Edit schedule opened on "What a run of this query does", with a Save there and an
  // Update schedule under the schedule: two saves, each leaving the other's edits behind.
  // Now a scheduled model reads as its settings, each with a Change, and one Save changes
  // commits whatever is open.
  it("gives a scheduled model one save, for what a run does and the schedule", async () => {
    const calls: Array<{ url: string; method: string }> = []
    mockFetch.mockImplementation(async (url: string, init?: RequestInit) => {
      const method = init?.method ?? "GET"
      if (method === "GET") return url.endsWith("/schedule") ? res(200, schedule({ status: "active" })) : res(404, {})
      calls.push({ url, method })
      return res(
        200,
        url.endsWith("/schedule")
          ? schedule({ status: "active" })
          : { materialization: "table", target_table: "analytics.mrr_daily" },
      )
    })
    const user = userEvent.setup({ delay: null })
    renderDialog()

    // Opens on what is scheduled, read-only: nothing to type into until a Change.
    expect(await screen.findByRole("button", { name: "Done" })).toBeInTheDocument()
    expect(screen.getByRole("dialog", { name: "Schedule “Daily MRR”" })).toBeInTheDocument()
    expect(screen.queryByRole("button", { name: /^save$/i })).not.toBeInTheDocument()
    expect(screen.queryByLabelText("Table it builds")).not.toBeInTheDocument()

    await user.click(screen.getByRole("button", { name: "Change what a run does" }))
    expect(screen.queryByRole("button", { name: "Done" })).not.toBeInTheDocument()
    await user.clear(screen.getByLabelText("Table it builds"))
    await user.paste("analytics.mrr_daily")
    await user.click(screen.getByRole("button", { name: /save changes/i }))
    await waitFor(() => expect(calls.map((c) => c.method)).toEqual(["PUT", "PUT"]))
    expect(calls[0].url).toContain("/materialization")
    expect(calls[1].url).toMatch(/\/schedule$/)
    // Saved, the sections close again.
    expect(await screen.findByRole("button", { name: "Done" })).toBeInTheDocument()
  })

  it("shows why a schedule auto-paused, and keeps the server's refusal when Resume is clicked", async () => {
    mockFetch.mockImplementation(async (url: string, init?: RequestInit) => {
      if (!init || init.method === undefined || init.method === "GET") {
        return res(
          200,
          schedule({
            status: "paused",
            auto_paused_at: "2026-08-13T03:00:00Z",
            auto_paused_reason: "the run-as user is no longer a member of this workspace",
          })
        )
      }
      // The resume attempt: the condition still holds, so the server refuses.
      return res(409, {
        error:
          "this schedule cannot resume yet: the run-as user is no longer a member of this workspace",
      })
    })

    renderDialog()

    await waitFor(() => {
      expect(
        screen.getByText(/Paused automatically: the run-as user is no longer a member/i)
      ).toBeInTheDocument()
    })

    // A paused schedule offers Resume, never Pause.
    expect(screen.queryByRole("button", { name: /^pause$/i })).not.toBeInTheDocument()
    await userEvent.click(screen.getByRole("button", { name: /resume/i }))

    await waitFor(() => {
      expect(toast.error).toHaveBeenCalledWith(
        "this schedule cannot resume yet: the run-as user is no longer a member of this workspace"
      )
    })
    // The refusal must not be reported as success.
    expect(toast.success).not.toHaveBeenCalled()
  })

  // A failed lookup and "there is no schedule" are different facts. Collapsing them
  // told a user with a live daily schedule that they had none, and offered a Create
  // button whose 409 was the only thing standing between them and a duplicate.
  it("does not report a failed schedule lookup as an absent schedule", async () => {
    mockFetch.mockResolvedValue(res(500, { error: "database is unavailable" }))

    renderDialog()

    await waitFor(() => {
      expect(screen.getByText(/Could not load this query's schedule/i)).toBeInTheDocument()
    })
    // The critical half of the message: it must not imply the schedule stopped.
    expect(
      screen.getByText(/Any existing schedule is unaffected and may still be running/i)
    ).toBeInTheDocument()
    // And none of the mutating controls may be offered on an unknown.
    expect(screen.queryByRole("button", { name: /save and schedule/i })).not.toBeInTheDocument()
    expect(screen.queryByRole("button", { name: /^delete$/i })).not.toBeInTheDocument()
    expect(screen.getByRole("button", { name: /retry/i })).toBeInTheDocument()
  })

  // 404 is the one response that actually asserts there is no schedule.
  it("treats only a 404 as 'no schedule yet'", async () => {
    mockFetch.mockResolvedValue(res(404, { error: "no schedule for this saved query" }))

    renderDialog()

    await waitFor(() => {
      expect(screen.getByRole("button", { name: /save and schedule/i })).toBeInTheDocument()
    })
    expect(screen.queryByText(/Could not load this query's schedule/i)).not.toBeInTheDocument()
  })

  // Chrome's (and Node's) Intl.supportedValuesOf("timeZone") lists no "UTC". A picker
  // built from it alone has no item for a schedule stored in UTC, and a Select whose
  // value has no item renders blank: the field read empty, and UTC could not be picked.
  it("opens a schedule stored in UTC on UTC, and offers UTC", async () => {
    mockFetch.mockResolvedValue(res(200, schedule()))

    const user = userEvent.setup()
    renderDialog()

    await user.click(await screen.findByRole("button", { name: "Change when it runs" }))
    const zone = await screen.findByRole("combobox", { name: /timezone/i })
    await waitFor(() => expect(zone).toHaveTextContent(/^UTC$/))
    await user.click(zone)
    expect(await screen.findByRole("option", { name: "UTC" })).toBeInTheDocument()
  })

  // Removing deregisters the Temporal schedule and cannot be undone, and it sits one
  // button away from the reversible Pause.
  it("does not remove a schedule on a single click", async () => {
    mockFetch.mockResolvedValue(res(200, schedule({ status: "active" })))

    renderDialog()

    await waitFor(() => {
      expect(screen.getByRole("button", { name: /^remove schedule$/i })).toBeInTheDocument()
    })
    mockFetch.mockClear()

    await userEvent.click(screen.getByRole("button", { name: /^remove schedule$/i }))

    // The confirmation is shown and nothing has been sent.
    const confirm = await screen.findByRole("alertdialog", { name: "Remove this schedule?" })
    expect(mockFetch).not.toHaveBeenCalled()

    // Confirming is what sends the DELETE.
    await userEvent.click(within(confirm).getByRole("button", { name: "Remove schedule" }))
    await waitFor(() => {
      expect(mockFetch).toHaveBeenCalledWith(
        expect.stringContaining(`/explorer/saved/${QUERY_ID}/schedule`),
        expect.objectContaining({ method: "DELETE" })
      )
    })
  })

  // The pill next to the title is the dialog's one-word answer to "is this running?".
  // "Not running" is the engine refusing it, which a status of "active" would hide.
  it.each([
    ["no schedule", res(404, { error: "no schedule for this saved query" }), "Not scheduled"],
    ["an active schedule", res(200, schedule({ status: "active" })), "Active"],
    ["a paused schedule", res(200, schedule({ status: "paused" })), "Paused"],
    ["a schedule the engine blocks", res(200, schedule({ status: "active", blocked: true })), "Not running"],
  ])("labels %s in the title", async (_case, answer, label) => {
    mockFetch.mockResolvedValue(answer)
    renderDialog()
    expect(await screen.findByTestId("schedule-status")).toHaveTextContent(label)
  })

  it("closes on Done when nothing is being changed", async () => {
    mockFetch.mockResolvedValue(res(200, schedule({ status: "active" })))
    const onOpenChange = vi.fn()
    const user = userEvent.setup({ delay: null })
    renderDialog({ onOpenChange })

    await user.click(await screen.findByRole("button", { name: "Done" }))
    expect(onOpenChange).toHaveBeenCalledWith(false)
    expect(mockFetch.mock.calls.filter(([, init]) => init?.method && init.method !== "GET")).toEqual([])
  })

  // Cancel on an open Change is "never mind", not "close": the edit goes, the dialog stays.
  it("puts an edit back on Cancel and keeps the dialog open", async () => {
    mockFetch.mockResolvedValue(res(200, schedule({ status: "active" })))
    const onOpenChange = vi.fn()
    const user = userEvent.setup({ delay: null })
    renderDialog({ onOpenChange })

    await user.click(await screen.findByRole("button", { name: "Change what a run does" }))
    await user.clear(screen.getByLabelText("Table it builds"))
    await user.paste("analytics.something_else")
    await user.click(screen.getByRole("button", { name: "Cancel" }))

    expect(onOpenChange).not.toHaveBeenCalled()
    expect(await screen.findByRole("button", { name: "Done" })).toBeInTheDocument()
    await user.click(screen.getByRole("button", { name: "Change what a run does" }))
    expect(screen.getByLabelText("Table it builds")).toHaveValue("analytics.daily_mrr")
  })

  // "Only by hand" on a live schedule means removing it, which cannot be undone — so it
  // goes through the same confirmation as the Remove schedule button, not straight to a DELETE.
  it("asks before removing a schedule when it is changed to only by hand", async () => {
    mockFetch.mockResolvedValue(res(200, schedule({ status: "active" })))
    const user = userEvent.setup({ delay: null })
    renderDialog()

    await user.click(await screen.findByRole("button", { name: "Change when it runs" }))
    await user.click(await screen.findByRole("radio", { name: /only by hand/i }))
    mockFetch.mockClear()
    await user.click(screen.getByRole("button", { name: /save changes/i }))

    const confirm = await screen.findByRole("alertdialog", { name: "Remove this schedule?" })
    expect(mockFetch.mock.calls.filter(([, init]) => init?.method === "DELETE")).toEqual([])

    await user.click(within(confirm).getByRole("button", { name: "Remove schedule" }))
    await waitFor(() => {
      expect(mockFetch).toHaveBeenCalledWith(
        expect.stringContaining(`/explorer/saved/${QUERY_ID}/schedule`),
        expect.objectContaining({ method: "DELETE" })
      )
    })
  })

  // The reported defect: the dialog demanded a target table for every model, so a
  // MERGE — which already names its own destination — could only be scheduled by
  // inventing a second table it would never write.
  it("asks for no target table when the SQL runs as written, and schedules without one", async () => {
    const calls: Array<{ url: string; method: string; body: unknown }> = []
    mockFetch.mockImplementation(async (url: string, init?: RequestInit) => {
      const method = init?.method ?? "GET"
      if (url.endsWith("/schedule") && method === "GET") {
        return res(404, { error: "no schedule for this saved query" })
      }
      if (url.includes("/pipelines")) return res(200, { pipelines: [] })
      if (url.endsWith("/explorer/saved")) return res(200, { saved_queries: [] })
      calls.push({ url, method, body: init?.body ? JSON.parse(String(init.body)) : null })
      if (url.endsWith("/materialization")) {
        return res(200, { materialization: "statement", target_table: "" })
      }
      return res(200, { schedule_id: "b2c3d4e5-1111-2222-3333-444455556666" })
    })

    const user = userEvent.setup({ delay: null })
    renderDialog({ materialization: "none", targetTable: "", statementClass: "dml_write" })

    await screen.findByText(/a schedule that writes nowhere can only fail/i)
    await user.click(screen.getByRole("radio", { name: /run the sql as written/i }))

    // There is nothing to ask for: a target input here would be a field whose only
    // correct value is one the statement ignores.
    expect(screen.queryByLabelText("Table it builds")).not.toBeInTheDocument()
    expect(screen.getByRole("button", { name: /save and schedule/i })).toBeEnabled()

    await user.click(screen.getByRole("button", { name: /save and schedule/i }))

    await waitFor(() => {
      expect(calls.length).toBe(2)
    })
    expect(calls[0].method).toBe("PUT")
    // The target is CLEARED rather than carried: a row that names a table it no
    // longer writes describes two destinations, one of them stale.
    expect(calls[0].body).toEqual({ materialization: "statement", target_table: "", layer: "" })
    expect(calls[1].method).toBe("POST")
  })

  // The stored class is advisory — the server re-classifies on every run — but saying
  // it here turns a refusal after the click into a correction before it.
  it("warns when the chosen mode cannot work for the SQL that is stored", async () => {
    mockFetch.mockResolvedValue(res(404, { error: "no schedule for this saved query" }))

    const user = userEvent.setup({ delay: null })
    renderDialog({ materialization: "table", targetTable: "analytics.daily_mrr", statementClass: "dml_write" })

    // Table mode wraps the SQL in CREATE TABLE … AS, which only works for a SELECT.
    await waitFor(() => {
      expect(screen.getByText(/stored as dml_write, not a read/i)).toBeInTheDocument()
    })

    // ...and the converse: a SELECT run as written delivers its rows nowhere.
    await user.click(screen.getByRole("radio", { name: /run the sql as written/i }))
    expect(screen.queryByText(/stored as dml_write, not a read/i)).not.toBeInTheDocument()

    mockFetch.mockClear()
    renderDialog({ materialization: "statement", targetTable: "", statementClass: "read" })
    await waitFor(() => {
      expect(screen.getAllByText(/delivers its rows nowhere/i).length).toBeGreaterThan(0)
    })
  })

  // "Rebuilt analytics.daily_mrr" is a lie about a MERGE, which has no target table
  // to name. What it has is a row count, and only for a DML write.
  it("reports rows affected, not a rebuilt table, for a statement model", async () => {
    mockFetch.mockImplementation(async (url: string) => {
      if (String(url).endsWith("/run")) {
        return res(200, { status: "success", rows_affected: 3 })
      }
      return res(404, { error: "no schedule for this saved query" })
    })

    renderDialog({ materialization: "statement", targetTable: "" })

    await userEvent.click(screen.getByRole("button", { name: /run now/i }))

    await waitFor(() => {
      expect(toast.success).toHaveBeenCalledWith("Statement ran — 3 rows affected")
    })
  })

  it("reports what the engine said when a rebuild is rejected", async () => {
    mockFetch.mockImplementation(async (url: string) => {
      if (String(url).endsWith("/run")) {
        // 422: rsync.ai did its job, the SQL did not.
        return res(422, { status: "failed", error: `relation "orders" does not exist` })
      }
      return res(404, { error: "no schedule for this saved query" })
    })

    renderDialog()

    await userEvent.click(screen.getByRole("button", { name: /run now/i }))

    await waitFor(() => {
      expect(toast.error).toHaveBeenCalledWith(`relation "orders" does not exist`)
    })
  })

  // An engine that queries but cannot materialize is the one case where the Explorer
  // works perfectly right up to the point where it doesn't. Every engine the Explorer
  // queries today can back a model, so Shopify stands in for the next one added before
  // its model path. SetSavedQueryMaterialization refuses it; without this, the only
  // signal a user gets is a 400 after filling in the form.
  describe("on an engine that cannot back a model", () => {
    const blocked = { supportsMaterialization: false, connectorType: "shopify" }

    it("disables the two write modes, naming the engine, but leaves the query saveable", async () => {
      mockFetch.mockResolvedValue(res(404, { error: "no schedule for this saved query" }))

      renderDialog({ ...blocked, materialization: "none", targetTable: "" })

      await waitFor(() => {
        expect(screen.getByRole("radio", { name: /rebuild a table/i })).toBeDisabled()
      })
      expect(screen.getByRole("radio", { name: /run the sql as written/i })).toBeDisabled()

      // "Nothing on its own" must stay live. The server returns before its dialect check
      // for mode "none", so turning a model off is allowed on every engine — disabling
      // this would strand a model configured before the gate existed.
      expect(screen.getByRole("radio", { name: /nothing on its own/i })).toBeEnabled()

      // In the page, not only in a title attribute: a keyboard user never hovers.
      // The engine's display name, not its raw connector_type.
      expect(screen.getByText(/Shopify Admin \(GraphQL\) connections cannot back a materialized model yet/))
        .toBeInTheDocument()

      // A run that does nothing has nothing to schedule.
      expect(await screen.findByText(/a schedule that writes nowhere can only fail/i)).toBeInTheDocument()
      expect(screen.queryByRole("button", { name: /save and schedule/i })).not.toBeInTheDocument()
      expect(screen.getByRole("button", { name: /run now/i })).toBeDisabled()
    })

    it("refuses to schedule a model stored before the gate existed", async () => {
      mockFetch.mockResolvedValue(res(404, { error: "no schedule for this saved query" }))

      // materialization="table" with a target satisfies canCreateSchedule, which is why
      // engineBlocked is checked on top of it rather than folded into it. Creating the
      // schedule saves the mode first, and that PUT is the one the server would 400.
      renderDialog({ ...blocked, materialization: "table", targetTable: "analytics.daily_mrr" })

      // Loaded first: the button is disabled while the lookup is out, for another reason.
      await screen.findByRole("region", { name: "What will happen" })
      expect(screen.getByRole("button", { name: /save and schedule/i })).toBeDisabled()
      expect(screen.getByRole("button", { name: /run now/i })).toBeDisabled()
    })

    // The contract that keeps this from becoming an outage: `supports_materialization`
    // is read as an explicit boolean, exactly like `supports_explorer` in the Explorer's
    // connection list. A payload from a server that predates the field leaves it
    // undefined, and reading that as "blocked" would disable materialization for every
    // engine — including the Postgres and MySQL connections it works on.
    it("does not gate anything when the server did not send the field", async () => {
      mockFetch.mockResolvedValue(res(404, { error: "no schedule for this saved query" }))

      renderDialog({ materialization: "table", targetTable: "analytics.daily_mrr" })

      await waitFor(() => {
        expect(screen.getByRole("radio", { name: /rebuild a table/i })).toBeEnabled()
      })
      expect(screen.getByRole("radio", { name: /run the sql as written/i })).toBeEnabled()
      expect(screen.getByRole("button", { name: /run now/i })).toBeEnabled()
      expect(screen.queryByText(/cannot back a materialized model yet/i)).not.toBeInTheDocument()
    })
  })

  // A MongoDB model is a saved pipeline whose result the connector writes with $out. It
  // has one write mode: a pipeline has no effect but its result, so "run as written"
  // does not exist for it, and the server refuses it (authorizeDocumentModel). Offering
  // the radio would be a control whose only outcome is a 400.
  describe("on a MongoDB pipeline", () => {
    const pipeline = { supportsMaterialization: true, connectorType: "mongodb", queryLanguage: "document" }

    it("offers a collection target and no statement mode", async () => {
      mockFetch.mockResolvedValue(res(404, { error: "no schedule for this saved query" }))

      renderDialog({ ...pipeline, materialization: "none", targetTable: "" })

      await waitFor(() => {
        expect(screen.getByRole("radio", { name: /rebuild a collection/i })).toBeEnabled()
      })
      expect(screen.getByRole("radio", { name: /nothing on its own/i })).toBeEnabled()
      expect(screen.queryByRole("radio", { name: /run the sql as written/i })).not.toBeInTheDocument()
      expect(screen.queryByRole("radio", { name: /rebuild a table/i })).not.toBeInTheDocument()
      expect(screen.queryByText(/cannot back a materialized model yet/i)).not.toBeInTheDocument()
    })

    it("saves the collection as the model's target", async () => {
      const calls: Array<{ url: string; method: string; body: unknown }> = []
      mockFetch.mockImplementation(async (url: string, init?: RequestInit) => {
        const method = init?.method ?? "GET"
        if (url.endsWith("/schedule") && method === "GET") {
          return res(404, { error: "no schedule for this saved query" })
        }
        if (url.includes("/pipelines")) return res(200, { pipelines: [] })
        if (url.endsWith("/explorer/saved")) return res(200, { saved_queries: [] })
        calls.push({ url, method, body: init?.body ? JSON.parse(String(init.body)) : null })
        return res(200, { materialization: "table", target_table: "analytics.daily_orders" })
      })

      const user = userEvent.setup({ delay: null })
      renderDialog({ ...pipeline, materialization: "none", targetTable: "" })

      await waitFor(() => {
        expect(screen.getByRole("radio", { name: /rebuild a collection/i })).toBeEnabled()
      })
      await user.click(screen.getByRole("radio", { name: /rebuild a collection/i }))
      await user.click(screen.getByLabelText("Collection it builds"))
      await user.paste("analytics.daily_orders")
      // By hand: saving what a run does without putting it on a schedule.
      await user.click(screen.getByRole("radio", { name: /only by hand/i }))
      await user.click(screen.getByRole("button", { name: /^save$/i }))

      await waitFor(() => {
        expect(calls.length).toBe(1)
      })
      expect(calls[0].method).toBe("PUT")
      expect(calls[0].url).toContain("/materialization")
      expect(calls[0].body).toEqual({ materialization: "table", target_table: "analytics.daily_orders", layer: "" })
      expect(screen.getByRole("button", { name: /run now/i })).toHaveAttribute("title", "Rebuild the collection now")
    })
  })

  // A layer is a label from the workspace's own list (migration 135) that the lineage
  // graph and the lists filter on. It rides on the materialization PUT, so what must
  // hold is: the picker offers the workspace's layers and no others, the PUT carries
  // the chosen one by name, a stored layer the list lacks is kept rather than dropped,
  // and a query that stops writing a table drops its layer.
  describe("layer", () => {
    function mockSave(calls: Array<{ url: string; method: string; body: unknown }>, answer: object) {
      mockFetch.mockImplementation(async (url: string, init?: RequestInit) => {
        const method = init?.method ?? "GET"
        if (url.endsWith("/schedule") && method === "GET") {
          return res(404, { error: "no schedule for this saved query" })
        }
        if (url.includes("/pipelines")) return res(200, { pipelines: [] })
        if (url.endsWith("/explorer/saved")) return res(200, { saved_queries: [] })
        calls.push({ url, method, body: init?.body ? JSON.parse(String(init.body)) : null })
        return res(200, answer)
      })
    }

    it("sends the chosen layer with the target", async () => {
      const calls: Array<{ url: string; method: string; body: unknown }> = []
      mockSave(calls, { materialization: "table", target_table: "analytics.daily_mrr", layer: "Gold" })
      const user = userEvent.setup({ delay: null })
      renderDialog({ materialization: "table", targetTable: "analytics.daily_mrr" })

      await user.click(await screen.findByRole("combobox", { name: /layer/i }))
      const options = (await screen.findAllByRole("option")).map((o) => o.textContent)
      expect(options).toEqual(["No layer", "Silver", "Gold"])
      await user.click(screen.getByRole("option", { name: "Gold" }))
      // The workspace's own words for the layer, under the picker.
      expect(screen.getByText("Joined or aggregated from Silver models.")).toBeInTheDocument()
      expect(screen.getByRole("link", { name: "Manage layers" })).toHaveAttribute("href", "/workspace/settings?tab=layers")
      await user.click(screen.getByRole("radio", { name: /only by hand/i }))
      await user.click(screen.getByRole("button", { name: /^save$/i }))

      await waitFor(() => {
        expect(calls.length).toBe(1)
      })
      expect(calls[0].method).toBe("PUT")
      expect(calls[0].body).toEqual({ materialization: "table", target_table: "analytics.daily_mrr", layer: "Gold" })
    })

    it("keeps a stored layer the workspace list no longer has, and sends it unchanged", async () => {
      const calls: Array<{ url: string; method: string; body: unknown }> = []
      mockSave(calls, { materialization: "table", target_table: "analytics.daily_mrr", layer: "Archive" })
      const user = userEvent.setup({ delay: null })
      renderDialog({ materialization: "table", targetTable: "analytics.daily_mrr", layer: "Archive" })

      const picker = await screen.findByRole("combobox", { name: /layer/i })
      expect(picker).toHaveTextContent("Archive")
      await user.click(picker)
      const options = (await screen.findAllByRole("option")).map((o) => o.textContent)
      expect(options).toEqual(["No layer", "Silver", "Gold", "Archive"])
    })

    it("names a layer called none as itself, not as no layer", async () => {
      mockLayers.value = {
        ...mockLayers.value,
        layers: [{ id: "l-n", name: "none", description: "", position: 0, model_count: 0 }],
      }
      const calls: Array<{ url: string; method: string; body: unknown }> = []
      mockSave(calls, { materialization: "table", target_table: "analytics.daily_mrr", layer: "none" })
      const user = userEvent.setup({ delay: null })
      renderDialog({ materialization: "table", targetTable: "analytics.daily_mrr" })

      await user.click(await screen.findByRole("combobox", { name: /layer/i }))
      await user.click(await screen.findByRole("option", { name: "none" }))
      await user.click(screen.getByRole("radio", { name: /only by hand/i }))
      await user.click(screen.getByRole("button", { name: /^save$/i }))
      await waitFor(() => {
        expect(calls.length).toBe(1)
      })
      expect(calls[0].body).toEqual({ materialization: "table", target_table: "analytics.daily_mrr", layer: "none" })
    })

    it("says how to add layers when the workspace has none, in place of an empty picker", async () => {
      mockLayers.value = { ...mockLayers.value, layers: [] }
      renderDialog({ materialization: "table", targetTable: "analytics.daily_mrr" })

      expect(await screen.findByText(/this workspace has no layers yet/i)).toBeInTheDocument()
      expect(screen.queryByRole("combobox", { name: /layer/i })).not.toBeInTheDocument()
      expect(screen.getByRole("link", { name: "Add layers" })).toHaveAttribute("href", "/workspace/settings?tab=layers")
    })

    it("sends a member to an owner or admin for layers, with no settings link", async () => {
      mockLayers.value = { ...mockLayers.value, layers: [], canManage: false }
      renderDialog({ materialization: "table", targetTable: "analytics.daily_mrr" })

      expect(await screen.findByText(/an owner or admin adds them in workspace settings/i)).toBeInTheDocument()
      expect(screen.queryByRole("link", { name: "Add layers" })).not.toBeInTheDocument()
    })

    it("still offers No layer and the stored layer when the layer list failed to load", async () => {
      mockLayers.value = { ...mockLayers.value, layers: null, error: "Could not load the model layers" }
      const user = userEvent.setup({ delay: null })
      renderDialog({ materialization: "table", targetTable: "analytics.daily_mrr", layer: "Gold" })

      const picker = await screen.findByRole("combobox", { name: /layer/i })
      expect(picker).toHaveTextContent("Gold")
      await user.click(picker)
      const options = (await screen.findAllByRole("option")).map((o) => o.textContent)
      expect(options).toEqual(["No layer", "Gold"])
    })

    it("opens on the stored layer, and a layer change alone is something to save", async () => {
      const calls: Array<{ url: string; method: string; body: unknown }> = []
      mockSave(calls, { materialization: "table", target_table: "analytics.daily_mrr", layer: "" })
      const user = userEvent.setup({ delay: null })
      renderDialog({ materialization: "table", targetTable: "analytics.daily_mrr", layer: "Silver" })

      const picker = await screen.findByRole("combobox", { name: /layer/i })
      expect(picker).toHaveTextContent("Silver")
      await user.click(picker)
      await user.click(await screen.findByRole("option", { name: "No layer" }))
      await user.click(screen.getByRole("radio", { name: /only by hand/i }))
      await user.click(screen.getByRole("button", { name: /^save$/i }))

      await waitFor(() => {
        expect(calls.length).toBe(1)
      })
      expect(calls[0].body).toEqual({ materialization: "table", target_table: "analytics.daily_mrr", layer: "" })
    })

    it("clears the layer when the query stops writing a table, and offers none to pick", async () => {
      const calls: Array<{ url: string; method: string; body: unknown }> = []
      mockSave(calls, { materialization: "none", target_table: "" })
      const user = userEvent.setup({ delay: null })
      renderDialog({ materialization: "table", targetTable: "analytics.daily_mrr", layer: "Gold" })

      await screen.findByRole("combobox", { name: /layer/i })
      await user.click(screen.getByRole("radio", { name: /nothing on its own/i }))
      expect(screen.queryByRole("combobox", { name: /layer/i })).not.toBeInTheDocument()
      await user.click(screen.getByRole("button", { name: /^save$/i }))

      await waitFor(() => {
        expect(calls.length).toBe(1)
      })
      expect(calls[0].body).toEqual({ materialization: "none", target_table: "", layer: "" })
    })
  })

  // "After an upstream runs" is the trigger that makes this an ELT tool rather than a
  // cron runner: the model rebuilds from the data a load just delivered, instead of
  // from whatever happened to be in the table when a clock struck. An upstream is a
  // pipeline OR another model, and a model may wait on several of either.
  //
  // What the tests below protect is the shape of the request. An event trigger carries
  // upstreams and no cadence; a cron carries a cadence and no upstreams. Sending a
  // half-and-half — a leftover cron beside a type that never reads it, or a type with
  // no upstream at all — is either refused by the server or stored as a trigger that
  // never fires, and the second failure is silent.
  describe("after an upstream runs", () => {
    const PIPELINE_ID = "3c9a1e77-8b44-4f21-a0d6-5e2b7c1d9f30"
    const MODEL_ID = "9a2e4c81-7d63-4b90-8f12-3c6d5e0a1b74"

    /** No schedule yet, with one pipeline and one model available to point at. */
    function mockNoScheduleWithProducers(
      onMutate?: (call: { url: string; method: string; body: unknown }) => void
    ) {
      mockFetch.mockImplementation(async (url: string, init?: RequestInit) => {
        const method = init?.method ?? "GET"
        if (url.includes("/pipelines")) {
          return res(200, { pipelines: [{ id: PIPELINE_ID, name: "Daily orders load" }] })
        }
        // endsWith, not includes: this query's own schedule lives at
        // /explorer/saved/<id>/schedule, so a looser match here would answer the
        // schedule GET with a list of saved queries and the dialog would decide the
        // query already has a schedule.
        if (url.endsWith("/explorer/saved")) {
          return res(200, {
            saved_queries: [{ id: MODEL_ID, name: "Customer dim", materialization: "table" }],
          })
        }
        if (url.endsWith("/schedule") && method === "GET") {
          return res(404, { error: "no schedule for this saved query" })
        }
        // A read, like the three above, and answered with nothing to suggest. It is
        // matched here rather than falling through so `onMutate` keeps meaning
        // "something was submitted" — the assertion in several tests below.
        if (url.includes("/upstreams")) {
          return res(200, { references: [], unresolved: [], candidates: [], ambiguous: false })
        }
        onMutate?.({ url, method, body: init?.body ? JSON.parse(String(init.body)) : null })
        if (url.endsWith("/materialization")) {
          return res(200, { materialization: "table", target_table: "analytics.daily_mrr" })
        }
        return res(200, { schedule_id: "b2c3d4e5-1111-2222-3333-444455556666" })
      })
    }

    async function chooseAfterUpstream(user: ReturnType<typeof userEvent.setup>) {
      await user.click(await screen.findByRole("radio", { name: /when its inputs refresh/i }))
    }

    /** The picker starts closed on a trigger that already has upstreams. */
    async function openPicker(user: ReturnType<typeof userEvent.setup>) {
      await user.click(await screen.findByRole("button", { name: /add a pipeline or model/i }))
    }

    it("sends every chosen upstream, of either kind, and no cadence", async () => {
      const calls: Array<{ url: string; method: string; body: unknown }> = []
      mockNoScheduleWithProducers((c) => calls.push(c))

      const user = userEvent.setup()
      renderDialog({ materialization: "table", targetTable: "analytics.daily_mrr" })

      await waitFor(() => {
        expect(screen.getByRole("button", { name: /save and schedule/i })).toBeInTheDocument()
      })

      await chooseAfterUpstream(user)
      await user.click(await screen.findByRole("checkbox", { name: "Daily orders load" }))
      await user.click(screen.getByRole("checkbox", { name: "Customer dim" }))
      await user.click(screen.getByRole("button", { name: /save and schedule/i }))

      await waitFor(() => {
        expect(calls.filter((c) => c.method === "POST")).toHaveLength(1)
      })
      const posted = calls.find((c) => c.method === "POST")!.body as Record<string, unknown>
      expect(posted.schedule_type).toBe("after_upstream")
      // The kind travels with the id. Pipelines and models are separate tables with
      // independent id spaces, so an id on its own does not say what to wait for.
      // toEqual rather than a containment check: the display name must NOT be sent —
      // it is the producer's to change, and a copy stored here goes stale silently.
      expect(posted.upstreams).toEqual([
        { kind: "pipeline", id: PIPELINE_ID },
        { kind: "model", id: MODEL_ID },
      ])
      // Not `toBeUndefined`: the field is sent, and what matters is that it carries no
      // cadence. A cron left over from the default would be stored beside a type that
      // never reads it, and would resurface the moment someone switched back to a clock.
      expect(posted.schedule_spec).toEqual({})
    })

    // The words upstream and downstream show up on the model page; this is where a link
    // between two models is made, so this is where they are explained.
    it("says what choosing an upstream means, and the picker points at that line", async () => {
      mockNoScheduleWithProducers()

      const user = userEvent.setup()
      renderDialog({ materialization: "table", targetTable: "analytics.daily_mrr" })

      await waitFor(() => {
        expect(screen.getByRole("button", { name: /save and schedule/i })).toBeInTheDocument()
      })
      expect(screen.queryByText(/becomes this model's upstream/)).not.toBeInTheDocument()

      await chooseAfterUpstream(user)
      const help = await screen.findByText(
        "What you pick here becomes this model's upstream: this model runs after it. This model becomes its downstream.",
      )
      expect(help).toHaveAttribute("id", "model-upstreams-help")
      await screen.findByRole("checkbox", { name: "Daily orders load" })
      expect(screen.getByRole("group", { name: "Rebuild after" })).toHaveAttribute(
        "aria-describedby",
        "model-upstreams-help",
      )
    })

    // The server answers 400 for a trigger with an empty upstream set. Catching it here
    // turns a failed request into a button that says what is missing.
    it("will not submit a trigger that names no upstream", async () => {
      const calls: Array<{ url: string; method: string; body: unknown }> = []
      mockNoScheduleWithProducers((c) => calls.push(c))

      const user = userEvent.setup()
      renderDialog({ materialization: "table", targetTable: "analytics.daily_mrr" })

      await waitFor(() => {
        expect(screen.getByRole("button", { name: /save and schedule/i })).toBeEnabled()
      })

      await chooseAfterUpstream(user)

      expect(screen.getByRole("button", { name: /save and schedule/i })).toBeDisabled()
      expect(
        await screen.findByText(/choose at least one pipeline or model this query should follow/i)
      ).toBeInTheDocument()
      expect(calls).toHaveLength(0)
    })

    // An event trigger has no clock, so a timezone would be a control with no effect —
    // and, worse, would suggest the trigger fires at a time.
    it("offers no timezone", async () => {
      mockNoScheduleWithProducers()

      const user = userEvent.setup()
      renderDialog({ materialization: "table", targetTable: "analytics.daily_mrr" })

      await waitFor(() => {
        expect(screen.getByRole("combobox", { name: /timezone/i })).toBeInTheDocument()
      })

      await chooseAfterUpstream(user)

      expect(screen.queryByRole("combobox", { name: /timezone/i })).not.toBeInTheDocument()
    })

    // A model that writes nowhere never runs on its own, so it never finishes, so a
    // trigger pointing at it is silent by construction. And a model cannot follow
    // itself — the server calls that a cycle and refuses it. Both are filtered before
    // the box is drawn rather than explained after the save fails.
    it("offers other models that run, but not itself and not a query that writes nowhere", async () => {
      const SCRATCH_ID = "6b0f8d24-1a5c-4e37-9d80-7c2a4f6b3e15"
      mockFetch.mockImplementation(async (url: string, init?: RequestInit) => {
        if (url.includes("/pipelines")) return res(200, { pipelines: [] })
        if (url.endsWith("/explorer/saved")) {
          return res(200, {
            saved_queries: [
              { id: QUERY_ID, name: "Daily MRR", materialization: "table" },
              { id: MODEL_ID, name: "Customer dim", materialization: "table" },
              { id: SCRATCH_ID, name: "Scratch query", materialization: "none" },
            ],
          })
        }
        if (url.endsWith("/schedule") && (init?.method ?? "GET") === "GET") {
          return res(404, { error: "no schedule for this saved query" })
        }
        return res(200, {})
      })

      const user = userEvent.setup()
      renderDialog({ materialization: "table", targetTable: "analytics.daily_mrr" })

      await waitFor(() => {
        expect(screen.getByRole("button", { name: /save and schedule/i })).toBeInTheDocument()
      })

      await chooseAfterUpstream(user)

      expect(await screen.findByRole("checkbox", { name: "Customer dim" })).toBeInTheDocument()
      expect(screen.queryByRole("checkbox", { name: "Daily MRR" })).not.toBeInTheDocument()
      expect(screen.queryByRole("checkbox", { name: "Scratch query" })).not.toBeInTheDocument()
    })

    // Editing an existing trigger has to start from the trigger that is running. Seeding
    // the editor with the cron default instead would turn an unrelated edit — a pause, a
    // target change — into a silent conversion back to a clock schedule.
    it("opens an existing trigger on the upstreams it has, not on the cron default", async () => {
      mockFetch.mockImplementation(async (url: string, init?: RequestInit) => {
        if (url.includes("/pipelines")) {
          return res(200, { pipelines: [{ id: PIPELINE_ID, name: "Daily orders load" }] })
        }
        if (url.endsWith("/explorer/saved")) {
          return res(200, {
            saved_queries: [{ id: MODEL_ID, name: "Customer dim", materialization: "table" }],
          })
        }
        if (url.endsWith("/schedule") && (init?.method ?? "GET") === "GET") {
          return res(
            200,
            schedule({
              schedule_type: "after_upstream",
              schedule_spec: {},
              status: "active",
              upstreams: [
                { kind: "pipeline", id: PIPELINE_ID, name: "Daily orders load" },
                { kind: "model", id: MODEL_ID, name: "Customer dim" },
              ],
            })
          )
        }
        return res(200, {})
      })

      const user = userEvent.setup()
      renderDialog()

      // "any of", not a bare list: the fan-in fires on the first producer to finish, and
      // a comma list reads as a set that all has to land before anything happens.
      expect(
        within(await screen.findByRole("group", { name: "When it runs" })).getByText(
          /after any of daily orders load, customer dim runs/i
        )
      ).toBeInTheDocument()
      await user.click(screen.getByRole("button", { name: "Change when it runs" }))
      expect(screen.getByRole("radio", { name: /when its inputs refresh/i })).toBeChecked()
      // Shown as chips, in the order they were chosen...
      const chosen = await screen.findByRole("list", { name: "Chosen" })
      expect(within(chosen).getAllByRole("listitem").map((li) => li.textContent)).toEqual([
        "PipelineDaily orders load",
        "ModelCustomer dim",
      ])
      // ...and ticked in the panel.
      await openPicker(user)
      expect(screen.getByRole("checkbox", { name: "Daily orders load" })).toBeChecked()
      expect(screen.getByRole("checkbox", { name: "Customer dim" })).toBeChecked()
      expect(screen.getByRole("button", { name: /save changes/i })).toBeEnabled()
    })

    // "Rebuild after its inputs" on the Lineage page is the user asking for exactly this,
    // so the dialog opens with those inputs chosen. Nothing else ticks an upstream.
    describe("asked to rebuild after its inputs", () => {
      const PRESET = { upstreams: [{ kind: "pipeline" as const, id: PIPELINE_ID, name: "Daily orders load" }] }
      const chips = async () =>
        within(await screen.findByRole("list", { name: "Chosen" }))
          .getAllByRole("listitem")
          .map((li) => li.textContent)

      it("opens an unscheduled model on its inputs, chosen, and schedules them", async () => {
        const calls: Array<{ url: string; method: string; body: unknown }> = []
        mockNoScheduleWithProducers((c) => calls.push(c))
        const user = userEvent.setup()
        renderDialog({ preset: PRESET })

        await waitFor(() => expect(screen.getByRole("radio", { name: /when its inputs refresh/i })).toBeChecked())
        expect(await chips()).toEqual(["PipelineDaily orders load"])
        await user.click(screen.getByRole("button", { name: /save and schedule/i }))
        await waitFor(() => expect(calls.filter((c) => c.method === "POST")).toHaveLength(1))
        const posted = calls.find((c) => c.method === "POST")!.body as Record<string, unknown>
        expect(posted.schedule_type).toBe("after_upstream")
        expect(posted.upstreams).toEqual([{ kind: "pipeline", id: PIPELINE_ID }])
      })

      it("adds them to what a live trigger follows, with its section open, and saves the union", async () => {
        const calls: Array<{ method: string; body: Record<string, unknown> }> = []
        mockFetch.mockImplementation(async (url: string, init?: RequestInit) => {
          const method = init?.method ?? "GET"
          if (url.includes("/pipelines")) return res(200, { pipelines: [{ id: PIPELINE_ID, name: "Daily orders load" }] })
          if (url.endsWith("/explorer/saved"))
            return res(200, { saved_queries: [{ id: MODEL_ID, name: "Customer dim", materialization: "table" }] })
          if (url.endsWith("/schedule") && method === "GET")
            return res(
              200,
              schedule({
                schedule_type: "after_upstream",
                schedule_spec: {},
                status: "active",
                upstreams: [{ kind: "model", id: MODEL_ID, name: "Customer dim" }],
              })
            )
          if (url.includes("/upstreams")) return res(200, { references: [], unresolved: [], candidates: [], ambiguous: false })
          if (method !== "GET") calls.push({ method, body: JSON.parse(String(init?.body ?? "{}")) })
          return res(200, {})
        })
        const user = userEvent.setup()
        renderDialog({ preset: { upstreams: [...PRESET.upstreams, { kind: "model", id: MODEL_ID }] } })

        // Open without pressing Change: the click on Lineage was the change.
        expect(await screen.findByRole("radio", { name: /when its inputs refresh/i })).toBeChecked()
        expect(await chips()).toEqual(["ModelCustomer dim", "PipelineDaily orders load"])
        await user.click(screen.getByRole("button", { name: /save changes/i }))
        await waitFor(() => expect(calls.filter((c) => c.method === "PUT")).toHaveLength(1))
        expect(calls.find((c) => c.method === "PUT")!.body.upstreams).toEqual([
          { kind: "model", id: MODEL_ID },
          { kind: "pipeline", id: PIPELINE_ID },
        ])
      })

      it("leaves an ordinary open alone", async () => {
        mockNoScheduleWithProducers()
        renderDialog()
        await waitFor(() => expect(screen.getByRole("button", { name: /save and schedule/i })).toBeInTheDocument())
        expect(screen.getByRole("radio", { name: /when its inputs refresh/i })).not.toBeChecked()
        expect(screen.queryByRole("list", { name: "Chosen" })).toBeNull()
      })
    })

    // The server reads a missing policy as "any". An Update that left it out would turn
    // a fan-in the user set to wait for every upstream back into one that fires on each,
    // with nothing on screen having changed.
    it("keeps a wait-for-all trigger waiting for all when it is updated", async () => {
      const calls: Array<{ method: string; body: Record<string, unknown> }> = []
      mockFetch.mockImplementation(async (url: string, init?: RequestInit) => {
        const method = init?.method ?? "GET"
        if (url.includes("/pipelines")) {
          return res(200, { pipelines: [{ id: PIPELINE_ID, name: "Daily orders load" }] })
        }
        if (url.endsWith("/explorer/saved")) {
          return res(200, {
            saved_queries: [{ id: MODEL_ID, name: "Customer dim", materialization: "table" }],
          })
        }
        if (url.includes("/upstreams")) {
          return res(200, { references: [], unresolved: [], candidates: [], ambiguous: false })
        }
        if (url.endsWith("/schedule") && method === "GET") {
          return res(
            200,
            schedule({
              schedule_type: "after_upstream",
              schedule_spec: {},
              status: "active",
              upstream_policy: "all",
              upstreams: [
                { kind: "pipeline", id: PIPELINE_ID, name: "Daily orders load" },
                { kind: "model", id: MODEL_ID, name: "Customer dim" },
              ],
            })
          )
        }
        if (url.endsWith("/schedule")) {
          calls.push({ method, body: JSON.parse(String(init?.body ?? "{}")) })
        }
        return res(200, {})
      })

      const user = userEvent.setup()
      renderDialog({ materialization: "table", targetTable: "analytics.daily_mrr" })

      expect(
        within(await screen.findByRole("group", { name: "When it runs" })).getByText(
          /after all of daily orders load, customer dim run/i
        )
      ).toBeInTheDocument()
      await user.click(screen.getByRole("button", { name: "Change when it runs" }))
      await waitFor(() => {
        expect(screen.getByRole("radio", { name: /all of them have run/i })).toBeChecked()
      })

      await user.click(screen.getByRole("button", { name: /save changes/i }))
      await waitFor(() => {
        expect(calls.filter((c) => c.method === "PUT")).toHaveLength(1)
      })
      expect(calls.find((c) => c.method === "PUT")!.body.upstream_policy).toBe("all")
    })

    it("sends the policy the user picks, and offers the choice only with two upstreams", async () => {
      const calls: Array<{ url: string; method: string; body: unknown }> = []
      mockNoScheduleWithProducers((c) => calls.push(c))

      const user = userEvent.setup()
      renderDialog({ materialization: "table", targetTable: "analytics.daily_mrr" })
      await waitFor(() => {
        expect(screen.getByRole("button", { name: /save and schedule/i })).toBeInTheDocument()
      })

      await chooseAfterUpstream(user)
      await user.click(await screen.findByRole("checkbox", { name: "Daily orders load" }))
      // With one upstream "any" and "all" are the same trigger — a choice between them
      // would be a control with no effect.
      expect(screen.queryByRole("radio", { name: /all of them have run/i })).toBeNull()

      await user.click(screen.getByRole("checkbox", { name: "Customer dim" }))
      const any = screen.getByRole("radio", { name: /any one of them runs/i })
      expect(any).toBeChecked()
      await user.click(screen.getByRole("radio", { name: /all of them have run/i }))
      await user.click(screen.getByRole("button", { name: /save and schedule/i }))

      await waitFor(() => {
        expect(calls.filter((c) => c.method === "POST")).toHaveLength(1)
      })
      const posted = calls.find((c) => c.method === "POST")!.body as Record<string, unknown>
      expect(posted.upstream_policy).toBe("all")
    })

    // A producer deleted out from under a live trigger drops out of both lists. Drawing
    // only the boxes the lists returned would render nothing checked for a trigger that
    // is still stored, and the next Update would look like a no-op while actually
    // dropping an upstream the user never touched.
    it("still shows an upstream a live trigger points at when it is gone from the list", async () => {
      mockFetch.mockImplementation(async (url: string, init?: RequestInit) => {
        if (url.includes("/pipelines")) return res(200, { pipelines: [] })
        if (url.endsWith("/explorer/saved")) return res(200, { saved_queries: [] })
        if (url.endsWith("/schedule") && (init?.method ?? "GET") === "GET") {
          return res(
            200,
            schedule({
              schedule_type: "after_upstream",
              schedule_spec: {},
              status: "active",
              upstreams: [{ kind: "pipeline", id: PIPELINE_ID, name: "Retired loader" }],
            })
          )
        }
        return res(200, {})
      })

      const user = userEvent.setup()
      renderDialog()

      await user.click(await screen.findByRole("button", { name: "Change when it runs" }))
      expect(await screen.findByRole("button", { name: "Remove Retired loader" })).toBeInTheDocument()
      // And the button stays live: the trigger is complete, so this must not be
      // mistaken for the "no upstream chosen" case above.
      expect(screen.getByRole("button", { name: /save changes/i })).toBeEnabled()

      // Removing it is a real removal: with nothing left the trigger is incomplete.
      await user.click(screen.getByRole("button", { name: "Remove Retired loader" }))
      expect(screen.queryByRole("button", { name: "Remove Retired loader" })).not.toBeInTheDocument()
      expect(screen.getByRole("button", { name: /save changes/i })).toBeDisabled()
    })

    // Offering the option against two empty lists produces a dead end: nothing to tick
    // and a button that never enables, with nothing on screen saying why.
    it("says why when the workspace has nothing to follow", async () => {
      mockFetch.mockImplementation(async (url: string, init?: RequestInit) => {
        if (url.includes("/pipelines")) return res(200, { pipelines: [] })
        if (url.endsWith("/explorer/saved")) return res(200, { saved_queries: [] })
        if (url.endsWith("/schedule") && (init?.method ?? "GET") === "GET") {
          return res(404, { error: "no schedule for this saved query" })
        }
        return res(200, {})
      })

      const user = userEvent.setup()
      renderDialog({ materialization: "table", targetTable: "analytics.daily_mrr" })

      await waitFor(() => {
        expect(screen.getByRole("button", { name: /save and schedule/i })).toBeInTheDocument()
      })

      await chooseAfterUpstream(user)

      expect(
        screen.getByText(/this workspace has no pipelines, and no other model that runs/i)
      ).toBeInTheDocument()
      expect(screen.getByRole("button", { name: /save and schedule/i })).toBeDisabled()
    })

    // A failed list is not an empty list. Telling someone "you have nothing to follow"
    // when the request 500'd sends them off to create something they already have.
    //
    // And the two lists fail independently, which is why there are two messages rather
    // than one "something failed". A picker showing every model and no pipeline looks
    // complete; without a line naming the half that is missing, the user picks from
    // what is there and never learns the rest existed.
    it("names the source that failed, and keeps the other one usable", async () => {
      mockFetch.mockImplementation(async (url: string, init?: RequestInit) => {
        if (url.includes("/pipelines")) return res(500, { error: "boom" })
        if (url.endsWith("/explorer/saved")) {
          return res(200, {
            saved_queries: [{ id: MODEL_ID, name: "Customer dim", materialization: "table" }],
          })
        }
        if (url.endsWith("/schedule") && (init?.method ?? "GET") === "GET") {
          return res(404, { error: "no schedule for this saved query" })
        }
        return res(200, {})
      })

      const user = userEvent.setup()
      renderDialog({ materialization: "table", targetTable: "analytics.daily_mrr" })

      await waitFor(() => {
        expect(screen.getByRole("button", { name: /save and schedule/i })).toBeInTheDocument()
      })

      await chooseAfterUpstream(user)

      expect(
        await screen.findByText(/could not load this workspace's pipelines/i)
      ).toBeInTheDocument()
      expect(screen.queryByText(/could not load this workspace's other models/i)).not.toBeInTheDocument()
      // The half that arrived is still pickable, and the empty-state copy must not
      // claim the workspace has nothing.
      expect(screen.getByRole("checkbox", { name: "Customer dim" })).toBeEnabled()
      expect(
        screen.queryByText(/this workspace has no pipelines, and no other model that runs/i)
      ).not.toBeInTheDocument()
    })

    it("names the models list when that is the half that failed", async () => {
      mockFetch.mockImplementation(async (url: string, init?: RequestInit) => {
        if (url.includes("/pipelines")) {
          return res(200, { pipelines: [{ id: PIPELINE_ID, name: "Daily orders load" }] })
        }
        if (url.endsWith("/explorer/saved")) return res(500, { error: "boom" })
        if (url.endsWith("/schedule") && (init?.method ?? "GET") === "GET") {
          return res(404, { error: "no schedule for this saved query" })
        }
        return res(200, {})
      })

      const user = userEvent.setup()
      renderDialog({ materialization: "table", targetTable: "analytics.daily_mrr" })

      await waitFor(() => {
        expect(screen.getByRole("button", { name: /save and schedule/i })).toBeInTheDocument()
      })

      await chooseAfterUpstream(user)

      expect(
        await screen.findByText(/could not load this workspace's other models/i)
      ).toBeInTheDocument()
      expect(screen.queryByText(/could not load this workspace's pipelines/i)).not.toBeInTheDocument()
      expect(screen.getByRole("checkbox", { name: "Daily orders load" })).toBeEnabled()
    })

    // The pipelines list is paged server-side (50 by default with no paging params). A
    // picker built from its first page silently left out every pipeline after the 50th,
    // so a workspace with more could not follow most of them. The mock pages the way
    // ListPipelines does: no `page` param → the first 50 only.
    it("offers every pipeline in the workspace, not just the first page", async () => {
      const all = Array.from({ length: 120 }, (_, i) => ({
        id: `00000000-0000-4000-8000-${String(i + 1).padStart(12, "0")}`,
        name: `Pipeline ${i + 1}`,
      }))
      const pipelineUrls: string[] = []
      mockFetch.mockImplementation(async (url: string, init?: RequestInit) => {
        if (url.includes("/pipelines")) {
          pipelineUrls.push(url)
          const params = new URL(url, "http://x").searchParams
          if (!params.get("page")) {
            return res(200, { pipelines: all.slice(0, 50), pagination: { has_next: true } })
          }
          const page = Number(params.get("page"))
          const per = Number(params.get("per_page") || 25)
          const slice = all.slice((page - 1) * per, page * per)
          return res(200, {
            pipelines: slice,
            pagination: { page, per_page: per, total: all.length, has_next: page * per < all.length },
          })
        }
        if (url.endsWith("/explorer/saved")) return res(200, { saved_queries: [] })
        if (url.endsWith("/schedule") && (init?.method ?? "GET") === "GET") {
          return res(404, { error: "no schedule for this saved query" })
        }
        return res(200, {})
      })

      const user = userEvent.setup()
      renderDialog({ materialization: "table", targetTable: "analytics.daily_mrr" })
      await waitFor(() => {
        expect(screen.getByRole("button", { name: /save and schedule/i })).toBeInTheDocument()
      })
      await chooseAfterUpstream(user)

      expect(await screen.findByRole("checkbox", { name: "Pipeline 120" })).toBeInTheDocument()
      expect(screen.getByRole("checkbox", { name: "Pipeline 51" })).toBeInTheDocument()
      expect(screen.getAllByRole("checkbox", { name: /^Pipeline \d+$/ })).toHaveLength(120)
      // Paged at the server's ceiling, not one request per 25.
      expect(pipelineUrls).toHaveLength(2)
    })

    // A model reads its own connection's tables, so what loads that connection is what
    // it most likely follows. The rest stays reachable, one click or one search away,
    // and every row says which pipeline or model it is rather than leaving a bare name.
    describe("grouped by the model's connection", () => {
      const CONN = "c0000000-0000-4000-8000-000000000001"
      const OTHER_CONN = "c0000000-0000-4000-8000-000000000002"
      const ADS_ID = "a0000000-0000-4000-8000-000000000003"

      function mockConnectionProducers() {
        mockFetch.mockImplementation(async (url: string, init?: RequestInit) => {
          if (url.includes("/pipelines")) {
            return res(200, {
              pipelines: [
                {
                  id: PIPELINE_ID,
                  name: "Daily orders load",
                  source_connection: { id: "s1", name: "Shop DB", connector_type: "postgresql" },
                  destination_connection: { id: CONN, name: "Warehouse", connector_type: "bigquery" },
                  sync_mode: "cdc",
                },
                {
                  id: ADS_ID,
                  name: "Ads load",
                  // No connection name: the connector's display name stands in.
                  source_connection: { id: "s2", connector_type: "postgresql" },
                  destination_connection: { id: OTHER_CONN, name: "Lake" },
                  sync_mode: "batch",
                },
              ],
              pagination: { has_next: false },
            })
          }
          if (url.endsWith("/explorer/saved")) {
            return res(200, {
              saved_queries: [
                {
                  id: MODEL_ID,
                  name: "Customer dim",
                  materialization: "table",
                  target_table: "analytics.customer_dim",
                  connection_id: CONN,
                },
              ],
            })
          }
          if (url.endsWith("/schedule") && (init?.method ?? "GET") === "GET") {
            return res(404, { error: "no schedule for this saved query" })
          }
          return res(200, {})
        })
      }

      it("puts this connection's producers first and says what each one is", async () => {
        mockConnectionProducers()
        const user = userEvent.setup()
        renderDialog({ connectionId: CONN })
        await waitFor(() => {
          expect(screen.getByRole("button", { name: /save and schedule/i })).toBeInTheDocument()
        })
        await chooseAfterUpstream(user)

        const pipelines = await screen.findByRole("group", { name: "Pipelines loading this connection" })
        const orders = within(pipelines).getByRole("checkbox", { name: "Daily orders load" })
        expect(orders).toHaveAccessibleDescription("Shop DB → Warehouse · CDC")
        const models = screen.getByRole("group", { name: "Models on this connection" })
        expect(within(models).getByRole("checkbox", { name: "Customer dim" })).toHaveAccessibleDescription(
          "Builds analytics.customer_dim"
        )

        // The other connection's pipeline is one click away, not gone.
        expect(screen.queryByRole("checkbox", { name: "Ads load" })).not.toBeInTheDocument()
        await user.click(screen.getByRole("button", { name: "Show 1 from other connections" }))
        const others = screen.getByRole("group", { name: "Pipelines loading other connections" })
        expect(within(others).getByRole("checkbox", { name: "Ads load" })).toHaveAccessibleDescription(
          "PostgreSQL → Lake · Batch"
        )
      })

      it("searches every connection without the toggle", async () => {
        mockConnectionProducers()
        const user = userEvent.setup()
        renderDialog({ connectionId: CONN })
        await waitFor(() => {
          expect(screen.getByRole("button", { name: /save and schedule/i })).toBeInTheDocument()
        })
        await chooseAfterUpstream(user)

        await user.type(await screen.findByRole("searchbox", { name: /search pipelines and models/i }), "lake")
        expect(screen.getByRole("checkbox", { name: "Ads load" })).toBeInTheDocument()
        expect(screen.queryByRole("checkbox", { name: "Daily orders load" })).not.toBeInTheDocument()

        await user.clear(screen.getByRole("searchbox", { name: /search pipelines and models/i }))
        await user.type(screen.getByRole("searchbox", { name: /search pipelines and models/i }), "nothing like it")
        expect(screen.getByText("Nothing matches “nothing like it”.")).toBeInTheDocument()
      })

      // The dialog closes on Escape. Inside the picker the first Escape is the picker's:
      // closing a half-edited schedule because the user dismissed a list loses the edit.
      it("closes the picker on Escape, not the dialog", async () => {
        mockConnectionProducers()
        const onOpenChange = vi.fn()
        const user = userEvent.setup()
        renderDialog({ connectionId: CONN, onOpenChange })
        await waitFor(() => {
          expect(screen.getByRole("button", { name: /save and schedule/i })).toBeInTheDocument()
        })
        await chooseAfterUpstream(user)

        await user.click(await screen.findByRole("checkbox", { name: "Daily orders load" }))
        await user.click(screen.getByRole("searchbox", { name: /search pipelines and models/i }))
        await user.keyboard("{Escape}")

        expect(screen.queryByRole("searchbox", { name: /search pipelines and models/i })).not.toBeInTheDocument()
        expect(onOpenChange).not.toHaveBeenCalled()
        // Focus goes back to the button that opened it, and the choice stands.
        expect(screen.getByRole("button", { name: /add a pipeline or model/i })).toHaveFocus()
        expect(screen.getByRole("button", { name: "Remove Daily orders load" })).toBeInTheDocument()
      })
    })

    // The server refuses a seventeenth upstream, so the seventeenth box has to be
    // unavailable rather than refused after the click. The half that is easy to lose is
    // the other one: at the cap the CHECKED boxes must stay live, or the only way out of
    // a full set is to delete the schedule.
    it("stops at sixteen upstreams without trapping the set", async () => {
      const full = Array.from({ length: 16 }, (_, i) => ({
        kind: "model" as const,
        id: `0000000${i.toString(16)}-0000-4000-8000-000000000000`,
        name: `Model ${i + 1}`,
      }))
      mockFetch.mockImplementation(async (url: string, init?: RequestInit) => {
        if (url.includes("/pipelines")) {
          return res(200, { pipelines: [{ id: PIPELINE_ID, name: "Daily orders load" }] })
        }
        if (url.endsWith("/explorer/saved")) return res(200, { saved_queries: [] })
        if (url.endsWith("/schedule") && (init?.method ?? "GET") === "GET") {
          return res(
            200,
            schedule({
              schedule_type: "after_upstream",
              schedule_spec: {},
              status: "active",
              upstreams: full,
            })
          )
        }
        return res(200, {})
      })

      const user = userEvent.setup()
      renderDialog()

      await user.click(await screen.findByRole("button", { name: "Change when it runs" }))
      expect(await screen.findByText(/16 is the most one model can wait on/i)).toBeInTheDocument()
      await openPicker(user)
      expect(screen.getByRole("checkbox", { name: "Model 1" })).toBeChecked()
      // Nothing new can be added...
      expect(screen.getByRole("checkbox", { name: "Daily orders load" })).toBeDisabled()
      // ...but what is already chosen can still be given up.
      expect(screen.getByRole("checkbox", { name: "Model 1" })).toBeEnabled()
    })

    describe("from the asset graph", () => {
      /** This model reading analytics.orders, which `producer` writes. */
      function readingOrdersFrom(producer: string): ModelSurroundings {
        const model = { id: `model:${QUERY_ID}`, kind: "model", name: "Daily MRR" }
        const orders = { id: "table:c1:analytics.orders", kind: "table", name: "analytics.orders" }
        return {
          model,
          inputs: [{ table: orders, producers: [{ id: producer, kind: "pipeline", name: "Daily orders load" }] }],
          builds: [{ id: "table:c1:analytics.daily_mrr", kind: "table", name: "analytics.daily_mrr" }],
          readers: [],
        }
      }

      // The same rule as the suggestion: a mark on the choice, never a tick in a box.
      it("recommends following inputs when a producer of one is in the picker, and picks nothing", async () => {
        mockSurroundings.value = readingOrdersFrom(`pipeline:${PIPELINE_ID}`)
        mockNoScheduleWithProducers()
        const user = userEvent.setup()
        renderDialog({ materialization: "table", targetTable: "analytics.daily_mrr" })

        const inputs = await screen.findByRole("radio", { name: /when its inputs refresh/i })
        await waitFor(() => expect(inputs).toHaveAccessibleName(/recommended/i))
        expect(inputs).not.toBeChecked()

        await chooseAfterUpstream(user)
        expect(await screen.findByRole("checkbox", { name: "Daily orders load" })).not.toBeChecked()
        expect(screen.getByRole("button", { name: /save and schedule/i })).toBeDisabled()
      })

      it("does not recommend it when the picker has no producer of what the model reads", async () => {
        mockSurroundings.value = readingOrdersFrom("pipeline:00000000-0000-0000-0000-000000000000")
        mockNoScheduleWithProducers()
        renderDialog({ materialization: "table", targetTable: "analytics.daily_mrr" })

        await screen.findByRole("region", { name: "What will happen" })
        expect(screen.getByRole("radio", { name: /when its inputs refresh/i })).not.toHaveAccessibleName(
          /recommended/i
        )
      })

      it("shows where the model sits, and which inputs wake it as boxes are ticked", async () => {
        mockSurroundings.value = readingOrdersFrom(`pipeline:${PIPELINE_ID}`)
        mockNoScheduleWithProducers()
        const user = userEvent.setup()
        renderDialog({ materialization: "table", targetTable: "analytics.daily_mrr" })

        const strip = await screen.findByRole("region", { name: "Where it sits" })
        expect(within(strip).getByRole("link", { name: "Open in Lineage" })).toHaveAttribute(
          "href",
          `/explorer/lineage?focus=model%3A${QUERY_ID}`
        )
        expect(within(strip).getByText("analytics.daily_mrr")).toBeInTheDocument()
        expect(within(strip).getByText("Read by nothing yet")).toBeInTheDocument()
        expect(within(strip).getByText(/runs on a clock/i)).toBeInTheDocument()

        await chooseAfterUpstream(user)
        const input = within(within(strip).getByRole("list", { name: "Reads from" })).getByRole("listitem")
        expect(input).toHaveAttribute("data-wakes", "false")
        expect(within(strip).getByText(/0 of 1 inputs wake this model/i)).toBeInTheDocument()

        await user.click(await screen.findByRole("checkbox", { name: "Daily orders load" }))
        expect(input).toHaveAttribute("data-wakes", "true")
        expect(within(strip).getByText("Its input wakes this model.")).toBeInTheDocument()
      })

      it("leaves the strip out when the graph does not have the model", async () => {
        mockNoScheduleWithProducers()
        renderDialog({ materialization: "table", targetTable: "analytics.daily_mrr" })

        await screen.findByRole("region", { name: "What will happen" })
        expect(screen.queryByRole("region", { name: "Where it sits" })).not.toBeInTheDocument()
      })
    })

    // The dialog can infer which pipeline produces this query's inputs by reading its
    // SQL. That inference is a shortcut, and the tests below exist because the failure
    // mode of a shortcut is that it stops being one:
    //
    //  1. it must never select a pipeline on its own — a table can have two producers,
    //     a name can match without a schema, and the SQL can change tomorrow, so an
    //     auto-selected upstream is a schedule nobody chose;
    //  2. an ambiguous answer has to say so rather than offer the first row;
    //  3. when the inference is unavailable, the picker it was shortcutting must still
    //     be there, unmentioned and working.
    describe("suggesting the upstream", () => {
      const OTHER_ID = "5d1c2b33-9e88-4a77-b6c5-1f0e9d8c7b6a"

      function mockWithUpstreams(
        upstreams: unknown,
        opts: {
          pipelines?: { id: string; name: string }[]
          models?: { id: string; name: string; materialization: string }[]
          onCall?: (call: { url: string; method: string; body: unknown }) => void
        } = {}
      ) {
        const list = opts.pipelines ?? [{ id: PIPELINE_ID, name: "Daily orders load" }]
        mockFetch.mockImplementation(async (url: string, init?: RequestInit) => {
          const method = init?.method ?? "GET"
          opts.onCall?.({ url, method, body: init?.body ? JSON.parse(String(init.body)) : null })
          if (url.includes("/upstreams")) {
            if (upstreams === null) return res(500, { error: "boom" })
            return res(200, upstreams)
          }
          if (url.includes("/pipelines")) return res(200, { pipelines: list })
          if (url.endsWith("/explorer/saved")) return res(200, { saved_queries: opts.models ?? [] })
          if (url.endsWith("/schedule") && method === "GET") {
            return res(404, { error: "no schedule for this saved query" })
          }
          return res(200, { schedule_id: "b2c3d4e5-1111-2222-3333-444455556666" })
        })
      }

      const oneProducer = {
        references: ["analytics.orders"],
        unresolved: [],
        candidates: [
          {
            kind: "pipeline",
            id: PIPELINE_ID,
            name: "Daily orders load",
            table: "analytics.orders",
            matched_reference: "analytics.orders",
            qualified: true,
          },
        ],
        ambiguous: false,
      }

      const customerDim = { id: MODEL_ID, name: "Customer dim", materialization: "table" }
      const modelProducer = {
        references: ["analytics.customer_dim"],
        unresolved: [],
        candidates: [
          {
            kind: "model",
            id: MODEL_ID,
            name: "Customer dim",
            table: "analytics.customer_dim",
            matched_reference: "analytics.customer_dim",
            qualified: true,
          },
        ],
        ambiguous: false,
      }

      it("offers the producing pipeline without selecting it", async () => {
        mockWithUpstreams(oneProducer)
        const user = userEvent.setup()
        renderDialog({ materialization: "table", targetTable: "analytics.daily_mrr" })

        await waitFor(() => {
          expect(screen.getByRole("button", { name: /save and schedule/i })).toBeInTheDocument()
        })
        await chooseAfterUpstream(user)

        expect(await screen.findByRole("button", { name: /follow daily orders load/i })).toBeInTheDocument()
        expect(screen.getByText(/writes analytics\.orders/i)).toBeInTheDocument()

        // The whole point: the suggestion is visible and nothing has been chosen. If the
        // dialog pre-selected it, Create would be live and a click away from scheduling
        // against a pipeline the user never picked.
        expect(screen.getByRole("checkbox", { name: "Daily orders load" })).not.toBeChecked()
        expect(screen.getByRole("button", { name: /save and schedule/i })).toBeDisabled()
      })

      it("submits the suggested pipeline once the user accepts it", async () => {
        const calls: Array<{ url: string; method: string; body: unknown }> = []
        mockWithUpstreams(oneProducer, { onCall: (c) => calls.push(c) })
        const user = userEvent.setup()
        renderDialog({ materialization: "table", targetTable: "analytics.daily_mrr" })

        await waitFor(() => {
          expect(screen.getByRole("button", { name: /save and schedule/i })).toBeInTheDocument()
        })
        await chooseAfterUpstream(user)
        await user.click(await screen.findByRole("button", { name: /follow daily orders load/i }))

        // The box and the suggestion agree afterwards — they are one value, not two.
        expect(screen.getByRole("checkbox", { name: "Daily orders load" })).toBeChecked()
        expect(screen.getByRole("button", { name: /following daily orders load/i })).toBeDisabled()

        await user.click(screen.getByRole("button", { name: /save and schedule/i }))
        await waitFor(() => {
          expect(calls.filter((c) => c.method === "POST")).toHaveLength(1)
        })
        const posted = calls.find((c) => c.method === "POST")!.body as Record<string, unknown>
        expect(posted.upstreams).toEqual([{ kind: "pipeline", id: PIPELINE_ID }])
      })

      // The shortcut adds and never removes, so the box is the only way back. If the
      // Follow button also un-picked, one control would carry two meanings and the
      // "Following …" state would be a toggle nobody labelled as one.
      it("lets the box undo what the shortcut added", async () => {
        mockWithUpstreams(oneProducer)
        const user = userEvent.setup()
        renderDialog({ materialization: "table", targetTable: "analytics.daily_mrr" })

        await waitFor(() => {
          expect(screen.getByRole("button", { name: /save and schedule/i })).toBeInTheDocument()
        })
        await chooseAfterUpstream(user)
        await user.click(await screen.findByRole("button", { name: /follow daily orders load/i }))
        expect(screen.getByRole("button", { name: /save and schedule/i })).toBeEnabled()

        await user.click(screen.getByRole("checkbox", { name: "Daily orders load" }))

        expect(screen.getByRole("checkbox", { name: "Daily orders load" })).not.toBeChecked()
        expect(screen.getByRole("button", { name: /follow daily orders load/i })).toBeEnabled()
        expect(screen.getByRole("button", { name: /save and schedule/i })).toBeDisabled()
      })

      // `FROM orders` with analytics.orders and staging.orders both produced: two
      // different tables answer to the name, and only the author knows which one it is.
      it("announces ambiguity instead of choosing for the user", async () => {
        mockWithUpstreams(
          {
            references: ["orders"],
            unresolved: [],
            candidates: [
              { ...oneProducer.candidates[0], matched_reference: "orders", qualified: false },
              {
                kind: "pipeline",
                id: OTHER_ID,
                name: "Orders staging load",
                table: "staging.orders",
                matched_reference: "orders",
                qualified: false,
              },
            ],
            ambiguous: true,
          },
          {
            pipelines: [
              { id: PIPELINE_ID, name: "Daily orders load" },
              { id: OTHER_ID, name: "Orders staging load" },
            ],
          }
        )
        const user = userEvent.setup()
        renderDialog({ materialization: "table", targetTable: "analytics.daily_mrr" })

        await waitFor(() => {
          expect(screen.getByRole("button", { name: /save and schedule/i })).toBeInTheDocument()
        })
        await chooseAfterUpstream(user)

        expect(await screen.findByRole("button", { name: /follow daily orders load/i })).toBeInTheDocument()
        expect(screen.getByRole("button", { name: /follow orders staging load/i })).toBeInTheDocument()
        expect(screen.getByText(/matches more than one table/i)).toBeInTheDocument()
        expect(screen.getByRole("button", { name: /save and schedule/i })).toBeDisabled()
      })

      // Two producers of ONE table is fan-in, which a schedule can follow in full. It is
      // not a question the user has to settle, and a warning saying it is would be wrong.
      it("offers every producer of one table without calling it ambiguous", async () => {
        mockWithUpstreams(
          {
            references: ["analytics.orders"],
            unresolved: [],
            candidates: [
              { ...oneProducer.candidates[0] },
              {
                kind: "model",
                id: MODEL_ID,
                name: "Orders corrections",
                table: "analytics.orders",
                matched_reference: "analytics.orders",
                qualified: true,
              },
            ],
            ambiguous: false,
          },
          { models: [{ id: MODEL_ID, name: "Orders corrections", materialization: "table" }] }
        )
        const user = userEvent.setup()
        renderDialog({ materialization: "table", targetTable: "analytics.daily_mrr" })

        await waitFor(() => {
          expect(screen.getByRole("button", { name: /save and schedule/i })).toBeInTheDocument()
        })
        await chooseAfterUpstream(user)

        expect(await screen.findByRole("button", { name: /follow daily orders load/i })).toBeInTheDocument()
        expect(screen.getByRole("button", { name: /follow orders corrections/i })).toBeInTheDocument()
        expect(screen.getByText(/these pipelines and models produce/i)).toBeInTheDocument()
        expect(screen.queryByText(/more than one table/i)).not.toBeInTheDocument()
      })

      // A model is offered the way a pipeline is, and what gets posted says it is a
      // model: the id alone would name a row in the wrong table.
      it("offers a model that builds the table and submits it as a model", async () => {
        const calls: Array<{ url: string; method: string; body: unknown }> = []
        mockWithUpstreams(modelProducer, { models: [customerDim], onCall: (c) => calls.push(c) })
        const user = userEvent.setup()
        renderDialog({ materialization: "table", targetTable: "analytics.daily_mrr" })

        await waitFor(() => {
          expect(screen.getByRole("button", { name: /save and schedule/i })).toBeInTheDocument()
        })
        await chooseAfterUpstream(user)

        expect(await screen.findByText(/another model builds/i)).toBeInTheDocument()
        expect(screen.getByText(/builds analytics\.customer_dim/i)).toBeInTheDocument()
        expect(screen.getByRole("checkbox", { name: "Customer dim" })).not.toBeChecked()

        await user.click(screen.getByRole("button", { name: /follow customer dim/i }))
        expect(screen.getByRole("checkbox", { name: "Customer dim" })).toBeChecked()

        await user.click(screen.getByRole("button", { name: /save and schedule/i }))
        await waitFor(() => {
          expect(calls.filter((c) => c.method === "POST")).toHaveLength(1)
        })
        const posted = calls.find((c) => c.method === "POST")!.body as Record<string, unknown>
        expect(posted.upstreams).toEqual([{ kind: "model", id: MODEL_ID }])
      })

      // A name-only match is a weaker claim than a schema-qualified one, and the user is
      // the one deciding. Presenting both the same way asks them to trust the two equally.
      it("says when a match was on table name alone", async () => {
        mockWithUpstreams({
          references: ["orders"],
          unresolved: [],
          candidates: [{ ...oneProducer.candidates[0], matched_reference: "orders", qualified: false }],
          ambiguous: false,
        })
        const user = userEvent.setup()
        renderDialog({ materialization: "table", targetTable: "analytics.daily_mrr" })

        await waitFor(() => {
          expect(screen.getByRole("button", { name: /save and schedule/i })).toBeInTheDocument()
        })
        await chooseAfterUpstream(user)

        expect(await screen.findByText(/matched on table name only/i)).toBeInTheDocument()
      })

      // Clicking this would add an upstream with no box beside it, so the user could not
      // take back what the click just did.
      it("does not offer a pipeline the picker has no entry for", async () => {
        mockWithUpstreams(
          {
            references: ["analytics.orders"],
            unresolved: [],
            candidates: [
              {
                kind: "pipeline",
                id: OTHER_ID,
                name: "Deleted loader",
                table: "analytics.orders",
                matched_reference: "analytics.orders",
                qualified: true,
              },
            ],
            ambiguous: false,
          },
          { pipelines: [{ id: PIPELINE_ID, name: "Daily orders load" }] }
        )
        const user = userEvent.setup()
        renderDialog({ materialization: "table", targetTable: "analytics.daily_mrr" })

        await waitFor(() => {
          expect(screen.getByRole("button", { name: /save and schedule/i })).toBeInTheDocument()
        })
        await chooseAfterUpstream(user)

        expect(screen.queryByRole("button", { name: /follow deleted loader/i })).not.toBeInTheDocument()
      })

      // Pipelines and models are separate id spaces. An id the picker lists under the
      // OTHER kind is still not an entry for this candidate, and following it would add
      // a "model:<pipeline id>" upstream with no box beside it.
      it("does not offer a candidate the picker lists only under the other kind", async () => {
        mockWithUpstreams(
          {
            references: ["analytics.orders"],
            unresolved: [],
            candidates: [
              {
                kind: "model",
                id: PIPELINE_ID,
                name: "Orders rollup",
                table: "analytics.orders",
                matched_reference: "analytics.orders",
                qualified: true,
              },
            ],
            ambiguous: false,
          },
          { pipelines: [{ id: PIPELINE_ID, name: "Daily orders load" }], models: [] }
        )
        const user = userEvent.setup()
        renderDialog({ materialization: "table", targetTable: "analytics.daily_mrr" })

        await waitFor(() => {
          expect(screen.getByRole("button", { name: /save and schedule/i })).toBeInTheDocument()
        })
        await chooseAfterUpstream(user)

        expect(await screen.findByRole("checkbox", { name: "Daily orders load" })).toBeInTheDocument()
        expect(screen.queryByRole("button", { name: /follow orders rollup/i })).not.toBeInTheDocument()
      })

      // The picker works on its own and always has. A broken shortcut that announces
      // itself turns a working dialog into one that looks broken.
      it("stays silent and leaves the picker usable when the lookup fails", async () => {
        mockWithUpstreams(null)
        const user = userEvent.setup()
        renderDialog({ materialization: "table", targetTable: "analytics.daily_mrr" })

        await waitFor(() => {
          expect(screen.getByRole("button", { name: /save and schedule/i })).toBeInTheDocument()
        })
        await chooseAfterUpstream(user)

        expect(screen.queryByText(/this query reads/i)).not.toBeInTheDocument()
        await user.click(await screen.findByRole("checkbox", { name: "Daily orders load" }))
        expect(screen.getByRole("button", { name: /save and schedule/i })).toBeEnabled()
      })

      // Parsing SQL and querying table stats to answer a question nobody asked. Most
      // schedules are clock schedules, so this is the common path.
      it("does not ask for upstreams for a clock schedule", async () => {
        const calls: Array<{ url: string; method: string; body: unknown }> = []
        mockWithUpstreams(oneProducer, { onCall: (c) => calls.push(c) })
        renderDialog({ materialization: "table", targetTable: "analytics.daily_mrr" })

        await waitFor(() => {
          expect(screen.getByRole("button", { name: /save and schedule/i })).toBeInTheDocument()
        })

        expect(calls.filter((c) => c.url.includes("/upstreams"))).toHaveLength(0)
      })
    })
  })
})
