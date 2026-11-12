/**
 * The transform editor in a pipeline's Transforms tab — what the standalone
 * Transform Builder page (/transforms) became. These tests started on that page,
 * whose "Save Plan" button shipped with no `onClick` at all; they now drive the
 * editor, which is already bound to one pipeline, so there is no picker.
 *
 * Three things here are load-bearing and none of them are visible in review:
 *
 *  1. Saving is a REPLACE (transforms.go:432-462 deletes every row for the
 *     pipeline inside the transaction, then re-inserts the body). So the
 *     editor must first LOAD what is stored, and the confirm dialog must say
 *     in numbers what is about to go.
 *  2. `mask_pii` is not one of the editor's operations — the natural-language
 *     pipeline setup materializes it. It therefore has no card, and a naive
 *     save would leave it out of the request and delete it, which makes the
 *     next run copy those columns to the destination in the clear. It is
 *     carried through untouched. That is the assertion this file exists for.
 *  3. Generate and Preview used a bare `fetch` against /api/v1, which is behind
 *     AuthRequiredMiddleware + CSRFMiddleware (main.go:866-875) — so both were
 *     a 401/403 every time, and `if (res.ok)` turned that into silence.
 */

import { afterEach, beforeEach, describe, expect, it, vi } from "vitest"
import { cleanup, render, screen, waitFor, within } from "@testing-library/react"
import userEvent from "@testing-library/user-event"
import "@testing-library/jest-dom"

let mockRole = "member"

const authFetch = vi.fn()
vi.mock("@/lib/api/auth-fetch", () => ({
  authFetch: (...args: unknown[]) => authFetch(...args),
}))

const toastError = vi.fn()
const toastSuccess = vi.fn()
vi.mock("sonner", () => ({
  toast: {
    error: (...a: unknown[]) => toastError(...a),
    success: (...a: unknown[]) => toastSuccess(...a),
  },
}))

vi.mock("@/contexts/WorkspaceContext", () => ({
  useWorkspaceRole: () => ({
    role: mockRole,
    isLoading: false,
    error: false,
    activeWorkspace: null,
    can: () => false,
    // Delegate to the real ladder so this test can't drift from roles.ts.
    meets: (min: string) => realMeetsRole(mockRole, min as never),
  }),
}))

import { meetsRole as realMeetsRole } from "@/lib/workspace/roles"
import { NL_SUGGESTION, TransformPlanEditor } from "@/components/transforms/TransformPlanEditor"

// Real UUIDs: toDefinitions blanks an id that is not one, because the column is
// `uuid` and a client-minted non-UUID would roll the whole save back.
const FILTER_ID = "11111111-1111-4111-8111-111111111111"
const MASK_ID = "22222222-2222-4222-8222-222222222222"

/** A stored plan: one rule the editor can draw, one it cannot. */
const STORED = {
  pipeline_id: "p1",
  producer_transforms: [
    {
      id: FILTER_ID,
      transform_type: "producer",
      transform_order: 0,
      transform_config: { operation: "filter", condition: "amount > 100" },
      enabled: true,
    },
  ],
  consumer_transforms: [
    {
      id: MASK_ID,
      transform_type: "consumer",
      transform_order: 1,
      transform_config: { operation: "mask_pii", column: "email", mask_type: "hash" },
      enabled: true,
    },
  ],
}

/** What `Generate` returns: one rule the editor appends to whatever is there. */
const GENERATED = {
  transforms: [
    {
      transform_type: "producer",
      transform_order: 0,
      transform_config: { operation: "rename", column: "cust", new_name: "customer" },
      enabled: true,
    },
  ],
}

function json(status: number, body: unknown) {
  return { ok: status < 400, status, json: async () => body }
}

type Routes = {
  plan?: unknown
  planStatus?: number
  save?: { status: number; body: unknown }
  parse?: { status: number; body: unknown }
  preview?: { status: number; body: unknown }
}

function serve(routes: Routes = {}) {
  authFetch.mockImplementation(async (url: string, init?: RequestInit) => {
    const u = String(url)
    const post = init?.method === "POST"
    if (u.includes("/transforms/pipeline/")) {
      if (post) return json(routes.save?.status ?? 200, routes.save?.body ?? { count: 2 })
      return json(routes.planStatus ?? 200, routes.plan ?? { producer_transforms: [], consumer_transforms: [] })
    }
    if (u.includes("/transforms/parse")) {
      return json(routes.parse?.status ?? 200, routes.parse?.body ?? { transforms: [] })
    }
    if (u.includes("/transforms/preview")) {
      return json(routes.preview?.status ?? 200, routes.preview?.body ?? { preview: [] })
    }
    return json(404, { error: `unexpected ${u}` })
  })
}

function callsTo(fragment: string, method?: string) {
  return authFetch.mock.calls
    .filter(([url, init]) => {
      const m = (init as RequestInit | undefined)?.method
      return String(url).includes(fragment) && (method ? m === method : true)
    })
    .map(([url, init]) => ({
      url: String(url),
      body: (init as RequestInit | undefined)?.body
        ? JSON.parse(String((init as RequestInit).body))
        : undefined,
    }))
}

const onClose = vi.fn()

function renderEditor() {
  return render(<TransformPlanEditor pipelineId="p1" onClose={onClose} />)
}

const saveButton = () => screen.getByRole("button", { name: /save transforms/i })
const nlBox = () => screen.getByLabelText(/describe your transformations in natural language/i)

beforeEach(() => {
  mockRole = "member"
  authFetch.mockReset()
  toastError.mockReset()
  toastSuccess.mockReset()
  onClose.mockReset()
  serve()
})
afterEach(() => cleanup())

describe("Transform editor — saving the pipeline's plan", () => {
  it("cannot save until the stored plan has loaded", async () => {
    let release: () => void = () => {}
    authFetch.mockImplementation(
      () =>
        new Promise((resolve) => {
          release = () => resolve(json(200, STORED))
        })
    )
    renderEditor()

    // Saving before the load would replace the stored plan with an empty one.
    expect(saveButton()).toBeDisabled()
    release()
    await waitFor(() => expect(saveButton()).toBeEnabled())
  })

  it("opens holding the pipeline's stored plan", async () => {
    serve({ plan: STORED })
    renderEditor()

    await waitFor(() => expect(screen.getByText("Filter Rows")).toBeInTheDocument())
    expect(callsTo("/api/v1/transforms/pipeline/p1")).toHaveLength(1)
    expect(saveButton()).toBeEnabled()
  })

  it("names the rules it cannot draw instead of hiding them", async () => {
    serve({ plan: STORED })
    renderEditor()

    await waitFor(() =>
      expect(screen.getByText(/1 rule uses an operation this editor cannot display/)).toBeInTheDocument()
    )
    expect(screen.getByText(/\(mask_pii\)/)).toBeInTheDocument()
    expect(screen.getByText(/kept unchanged when you save/)).toBeInTheDocument()
  })

  it("asks before replacing, in counts, and sends nothing if the answer is no", async () => {
    serve({ plan: STORED })
    renderEditor()
    const user = userEvent.setup()
    await waitFor(() => expect(saveButton()).toBeEnabled())

    await user.click(saveButton())

    const dialog = await screen.findByRole("alertdialog")
    expect(within(dialog).getByText("Replace this pipeline's transforms?")).toBeInTheDocument()
    expect(within(dialog).getByText(/Saving REPLACES every transform on this pipeline/)).toBeInTheDocument()
    expect(within(dialog).getByText(/the 2 existing rules are deleted/)).toBeInTheDocument()
    expect(callsTo("/transforms/pipeline/", "POST")).toHaveLength(0)

    await user.click(within(dialog).getByRole("button", { name: "Cancel" }))
    expect(callsTo("/transforms/pipeline/", "POST")).toHaveLength(0)
    expect(onClose).not.toHaveBeenCalled()
  })

  // THE ONE THAT MATTERS. The save deletes everything and re-inserts the body,
  // so a mask left out of the request is a mask deleted — and the columns it
  // covered reach the destination in the clear on the next run.
  it("keeps the un-drawable mask_pii rule on the wire", async () => {
    serve({ plan: STORED })
    renderEditor()
    const user = userEvent.setup()
    await waitFor(() => expect(saveButton()).toBeEnabled())

    await user.click(saveButton())
    await user.click(await screen.findByRole("button", { name: "Replace and save" }))

    await waitFor(() => expect(callsTo("/transforms/pipeline/", "POST")).toHaveLength(1))
    const sent = callsTo("/transforms/pipeline/", "POST")[0]
    expect(sent.url).toContain("/api/v1/transforms/pipeline/p1")

    const ops = sent.body.transforms.map(
      (t: { transform_config: { operation: string } }) => t.transform_config.operation
    )
    expect(ops).toEqual(["filter", "mask_pii"])
    // Carried through byte-for-byte, id included — this is an UPDATE of the
    // same row, not a re-creation that would renumber it.
    expect(sent.body.transforms[1]).toMatchObject({
      id: MASK_ID,
      transform_type: "consumer",
      enabled: true,
      transform_config: { operation: "mask_pii", column: "email", mask_type: "hash" },
    })
  })

  it("closes as saved, so the tab re-reads the plan the server stored", async () => {
    serve({ plan: STORED, save: { status: 200, body: { count: 2 } } })
    renderEditor()
    const user = userEvent.setup()
    await waitFor(() => expect(saveButton()).toBeEnabled())

    await user.click(saveButton())
    await user.click(await screen.findByRole("button", { name: "Replace and save" }))

    await waitFor(() => expect(onClose).toHaveBeenCalledWith(true))
    // Two rules are on the plan: the filter card and the carried mask.
    expect(String(toastSuccess.mock.calls[0][0])).toContain("Saved 2 transforms")
  })

  it("surfaces a rejected save instead of pretending it landed", async () => {
    serve({ plan: STORED, save: { status: 403, body: { error: "insufficient workspace role" } } })
    renderEditor()
    const user = userEvent.setup()
    await waitFor(() => expect(saveButton()).toBeEnabled())

    await user.click(saveButton())
    await user.click(await screen.findByRole("button", { name: "Replace and save" }))

    await waitFor(() => expect(toastError).toHaveBeenCalledWith("insufficient workspace role"))
    expect(toastSuccess).not.toHaveBeenCalled()
    expect(onClose).not.toHaveBeenCalled()
  })

  it("renders a failed plan read as an error, not as an empty pipeline", async () => {
    serve({ planStatus: 500, plan: { error: "database is down" } })
    renderEditor()

    await waitFor(() => expect(screen.getByText("database is down")).toBeInTheDocument())
    expect(screen.queryByText("No transforms")).not.toBeInTheDocument()
    // Saving over an unread plan would replace rows nobody saw.
    expect(saveButton()).toBeDisabled()
  })

  it("says No transforms for a plan that loaded empty", async () => {
    // Non-zero control for the test above: the empty state still exists.
    renderEditor()
    await waitFor(() => expect(screen.getByText("No transforms")).toBeInTheDocument())
  })

  it("lets a viewer build and preview a plan but not save one", async () => {
    mockRole = "viewer"
    serve({ plan: STORED })
    renderEditor()

    await waitFor(() => expect(screen.getByText("Filter Rows")).toBeInTheDocument())
    expect(saveButton()).toBeDisabled()
    expect(saveButton()).toHaveAttribute(
      "title",
      "Only workspace members and admins can change a pipeline's transforms."
    )
    expect(
      screen.getByText(/only workspace members and admins can save one to a pipeline/i)
    ).toBeInTheDocument()
  })

  it("fails closed while the workspace role is still loading", async () => {
    // `role` is "" until WorkspaceContext resolves; roleRank ranks that 0.
    mockRole = ""
    serve({ plan: STORED })
    renderEditor()

    await waitFor(() => expect(screen.getByText("Filter Rows")).toBeInTheDocument())
    expect(saveButton()).toBeDisabled()
  })
})

describe("Transform editor — Generate and Preview", () => {
  it("sends Generate through authFetch, so it carries auth and CSRF", async () => {
    renderEditor()
    const user = userEvent.setup()

    await user.type(nlBox(), "mask every email")
    await user.click(screen.getByRole("button", { name: /generate/i }))

    await waitFor(() => expect(callsTo("/transforms/parse")).toHaveLength(1))
    // Relative: authFetch prefixes the gateway and attaches X-CSRF-Token. An
    // absolute URL would skip neither, but a bare fetch() would skip both.
    expect(callsTo("/transforms/parse")[0].url).toBe("/api/v1/transforms/parse")
    expect(callsTo("/transforms/parse")[0].body).toEqual({ natural_language: "mask every email" })
  })

  it("Tab writes the example into the empty box, and Generate sends it", async () => {
    renderEditor()
    const user = userEvent.setup()
    expect(nlBox()).toHaveAttribute("placeholder", `e.g. ${NL_SUGGESTION}`)

    await user.click(nlBox())
    await user.keyboard("{Tab}")
    expect(nlBox()).toHaveValue(NL_SUGGESTION)
    expect(nlBox()).toHaveFocus()
    // With text in the box, Tab moves on as usual.
    await user.keyboard("{Tab}")
    expect(nlBox()).not.toHaveFocus()

    await user.click(screen.getByRole("button", { name: /generate/i }))
    await waitFor(() => expect(callsTo("/transforms/parse")).toHaveLength(1))
    expect(callsTo("/transforms/parse")[0].body).toEqual({ natural_language: NL_SUGGESTION })
  })

  it("says so when Generate is refused, rather than doing nothing", async () => {
    serve({ parse: { status: 403, body: { error: "CSRF token missing" } } })
    renderEditor()
    const user = userEvent.setup()

    await user.type(nlBox(), "mask every email")
    await user.click(screen.getByRole("button", { name: /generate/i }))

    await waitFor(() => expect(toastError).toHaveBeenCalledWith("CSRF token missing"))
  })

  it("sends Preview through authFetch and shows what came back wrong", async () => {
    serve({ preview: { status: 400, body: { error: "sample_data must be an array of objects" } } })
    renderEditor()

    await userEvent.setup().click(screen.getByRole("button", { name: /^Preview$/ }))

    await waitFor(() => expect(callsTo("/transforms/preview")).toHaveLength(1))
    expect(callsTo("/transforms/preview")[0].url).toBe("/api/v1/transforms/preview")
    expect(await screen.findByText("sample_data must be an array of objects")).toBeInTheDocument()
  })
})

/**
 * Leaving the editor over unsaved work is a deletion, so it is asked rather
 * than done. The Builder once replaced a half-built plan on a pipeline switch
 * with no prompt, no marker and no way back; the same guard now sits on Cancel.
 */
describe("Transform editor — unsaved work", () => {
  /** Append a generated rule, which is the cheapest way to make the plan dirty. */
  async function makeDirty(user: ReturnType<typeof userEvent.setup>) {
    await user.type(nlBox(), "rename cust to customer")
    await user.click(screen.getByRole("button", { name: /generate/i }))
    await waitFor(() => expect(screen.getByText("Rename Columns")).toBeInTheDocument())
  }

  const unsavedMarker = () => screen.queryByText("Unsaved changes")
  const cancel = () => screen.getByRole("button", { name: "Cancel" })

  it("says nothing about unsaved work on a plan that is exactly what is stored", async () => {
    // Non-zero control. A marker that is always on is a marker nobody reads,
    // and a load is not an edit.
    serve({ plan: STORED })
    renderEditor()

    await waitFor(() => expect(screen.getByText("Filter Rows")).toBeInTheDocument())
    expect(unsavedMarker()).not.toBeInTheDocument()
  })

  it("marks the plan unsaved as soon as it differs from what is stored", async () => {
    serve({ plan: STORED, parse: { status: 200, body: GENERATED } })
    renderEditor()
    const user = userEvent.setup()
    await waitFor(() => expect(screen.getByText("Filter Rows")).toBeInTheDocument())

    await makeDirty(user)

    expect(unsavedMarker()).toBeInTheDocument()
    expect(unsavedMarker()).toHaveAttribute("role", "status")
  })

  it("asks before Cancel throws that work away", async () => {
    serve({ plan: STORED, parse: { status: 200, body: GENERATED } })
    renderEditor()
    const user = userEvent.setup()
    await waitFor(() => expect(screen.getByText("Filter Rows")).toBeInTheDocument())
    await makeDirty(user)

    await user.click(cancel())

    const dialog = await screen.findByRole("alertdialog")
    expect(within(dialog).getByText("Discard unsaved changes?")).toBeInTheDocument()
    expect(within(dialog).getByText(/cannot be recovered/)).toBeInTheDocument()
    expect(onClose).not.toHaveBeenCalled()
    expect(screen.getByText("Rename Columns")).toBeInTheDocument()
  })

  it("keeps the edits when the answer is no", async () => {
    serve({ plan: STORED, parse: { status: 200, body: GENERATED } })
    renderEditor()
    const user = userEvent.setup()
    await waitFor(() => expect(screen.getByText("Filter Rows")).toBeInTheDocument())
    await makeDirty(user)
    await user.click(cancel())

    const dialog = await screen.findByRole("alertdialog")
    await user.click(within(dialog).getByRole("button", { name: "Keep editing" }))

    await waitFor(() => expect(screen.queryByRole("alertdialog")).not.toBeInTheDocument())
    expect(screen.getByText("Rename Columns")).toBeInTheDocument()
    expect(unsavedMarker()).toBeInTheDocument()
    expect(onClose).not.toHaveBeenCalled()
  })

  it("closes without saving when the answer is yes", async () => {
    serve({ plan: STORED, parse: { status: 200, body: GENERATED } })
    renderEditor()
    const user = userEvent.setup()
    await waitFor(() => expect(screen.getByText("Filter Rows")).toBeInTheDocument())
    await makeDirty(user)
    await user.click(cancel())

    const dialog = await screen.findByRole("alertdialog")
    await user.click(within(dialog).getByRole("button", { name: "Discard changes" }))

    await waitFor(() => expect(onClose).toHaveBeenCalledWith(false))
    expect(callsTo("/transforms/pipeline/", "POST")).toHaveLength(0)
  })

  it("closes with no prompt at all when there is nothing to lose", async () => {
    // Non-zero control: the guard must not turn every Cancel into a dialog.
    serve({ plan: STORED })
    renderEditor()
    const user = userEvent.setup()
    await waitFor(() => expect(screen.getByText("Filter Rows")).toBeInTheDocument())

    await user.click(cancel())

    expect(onClose).toHaveBeenCalledWith(false)
    expect(screen.queryByRole("alertdialog")).not.toBeInTheDocument()
  })

  it("lets the browser warn before a reload discards the work", async () => {
    // The editor's own dialogs cannot stop a reload or a closed tab; only
    // beforeunload can.
    serve({ plan: STORED, parse: { status: 200, body: GENERATED } })
    renderEditor()
    const user = userEvent.setup()
    await waitFor(() => expect(screen.getByText("Filter Rows")).toBeInTheDocument())

    // Non-zero control: a plan as loaded must not nag on every navigation.
    const fire = () => window.dispatchEvent(new Event("beforeunload", { cancelable: true }))
    expect(fire()).toBe(true)

    await makeDirty(user)
    // dispatchEvent returns false once a listener has called preventDefault,
    // which is what makes Chrome show its "Leave site?" prompt.
    await waitFor(() => expect(fire()).toBe(false))
  })
})

/**
 * ONE RULE, BOTH PATHS. The batch executor reads producer rows and the CDC sink
 * reads consumer rows, and a CDC pipeline's existing rows can arrive by either
 * path (a Debezium snapshot through the sink, or a batch initial load through the
 * executor). The Builder used to make the operator pick a side per rule, so a mask
 * added on the CDC tab never touched the rows a batch initial load copied.
 */
describe("Transform editor — every rule runs on batch and CDC", () => {
  const PAIR_P = "33333333-3333-4333-8333-333333333333"
  const PAIR_C = "44444444-4444-4444-8444-444444444444"
  const MASK = { operation: "mask", column: "email", mask_type: "hash" }
  /** One logical rule, stored the way the save writes it: a producer and a consumer row. */
  const PAIRED = {
    pipeline_id: "p1",
    producer_transforms: [
      { id: PAIR_P, transform_type: "producer", transform_order: 0, transform_config: MASK, enabled: true },
    ],
    consumer_transforms: [
      // Same config with its keys in another order — JSONB does not keep key order.
      {
        id: PAIR_C,
        transform_type: "consumer",
        transform_order: 1,
        transform_config: { mask_type: "hash", column: "email", operation: "mask" },
        enabled: true,
      },
    ],
  }

  async function saveAndReadBody(user: ReturnType<typeof userEvent.setup>) {
    await user.click(saveButton())
    await user.click(await screen.findByRole("button", { name: "Replace and save" }))
    await waitFor(() => expect(callsTo("/transforms/pipeline/", "POST")).toHaveLength(1))
    return callsTo("/transforms/pipeline/", "POST")[0].body.transforms as Array<{
      id: string
      transform_type: string
      transform_order: number
      transform_config: Record<string, unknown>
    }>
  }

  it("offers no Batch/CDC choice when adding a rule", async () => {
    renderEditor()
    const user = userEvent.setup()

    await user.click(screen.getByRole("button", { name: "Add" }))

    const dialog = await screen.findByRole("dialog")
    expect(within(dialog).queryByRole("tab")).not.toBeInTheDocument()
    expect(within(dialog).queryByText(/Batch \(Producer\)|CDC \(Consumer\)/)).not.toBeInTheDocument()
    // Each supported operation is offered exactly once, not once per side.
    expect(within(dialog).getAllByRole("button", { name: /Filter Rows/ })).toHaveLength(1)
  })

  it("saves an added rule to BOTH sides, with the same config", async () => {
    renderEditor()
    const user = userEvent.setup()
    await waitFor(() => expect(screen.getByText("No transforms")).toBeInTheDocument())

    await user.click(screen.getByRole("button", { name: "Add" }))
    await user.click(within(await screen.findByRole("dialog")).getByRole("button", { name: /Filter Rows/ }))
    await user.type(await screen.findByLabelText("Filter Condition"), "amount > 100")

    const rows = await saveAndReadBody(user)
    expect(rows.map((r) => r.transform_type)).toEqual(["producer", "consumer"])
    expect(rows[0].transform_config).toEqual({
      operation: "filter",
      description: "Filter rows based on a condition",
      condition: "amount > 100",
    })
    expect(rows[1].transform_config).toEqual(rows[0].transform_config)
  })

  it("draws a stored pair as ONE card and saves it back onto the same two rows", async () => {
    serve({ plan: PAIRED })
    renderEditor()
    const user = userEvent.setup()

    await waitFor(() => expect(screen.getAllByText("Mask PII")).toHaveLength(1))
    expect(screen.getByText("Batch + CDC")).toBeInTheDocument()
    expect(screen.queryByRole("button", { name: /Run .* on both/ })).not.toBeInTheDocument()

    const rows = await saveAndReadBody(user)
    expect(rows.map((r) => [r.transform_type, r.id])).toEqual([
      ["producer", PAIR_P],
      ["consumer", PAIR_C],
    ])
  })

  it("never widens a one-sided legacy rule on its own", async () => {
    serve({ plan: STORED })
    renderEditor()
    const user = userEvent.setup()
    await waitFor(() => expect(screen.getByText("Batch only")).toBeInTheDocument())

    const rows = await saveAndReadBody(user)
    // filter stays producer-only; the carried mask_pii stays consumer-only.
    expect(rows.map((r) => [r.transform_type, r.transform_config.operation])).toEqual([
      ["producer", "filter"],
      ["consumer", "mask_pii"],
    ])
  })

  it("widens a legacy rule to both sides when the operator asks", async () => {
    serve({ plan: STORED })
    renderEditor()
    const user = userEvent.setup()

    await user.click(await screen.findByRole("button", { name: "Run Filter Rows on both batch and CDC" }))
    expect(screen.getByText("Batch + CDC")).toBeInTheDocument()
    expect(screen.getByText("Unsaved changes")).toBeInTheDocument()

    const rows = await saveAndReadBody(user)
    expect(rows.map((r) => [r.transform_type, r.transform_config.operation])).toEqual([
      ["producer", "filter"],
      ["consumer", "filter"],
      ["consumer", "mask_pii"],
    ])
    // The existing producer row keeps its id; the new consumer row gets one minted.
    expect(rows[0].id).toBe(FILTER_ID)
    expect(rows[1].id).toBe("")
  })

  it("says in the save dialog what a change does to rows already moved", async () => {
    serve({ plan: STORED })
    renderEditor()
    const user = userEvent.setup()
    await waitFor(() => expect(saveButton()).toBeEnabled())

    await user.click(saveButton())

    const dialog = await screen.findByRole("alertdialog")
    const note = within(dialog).getByTestId("forward-only-note")
    expect(note).toHaveTextContent(/Applies to data moved from now on/)
    expect(note).toHaveTextContent(/new mask leaves earlier rows unmasked/)
    expect(note).toHaveTextContent(/rename/)
    expect(note).toHaveTextContent(/excluded column/)
  })
})

// A MongoDB CDC pipeline's GET names the only rule types it can run
// (consumer_rule_types), and the save refuses any other along with everything
// sent beside it. The editor offered every operation there anyway, and Generate
// appended a filter that the save then refused.
describe("Transform editor — a pipeline that can run only some rule types", () => {
  const LIMITED = {
    producer_transforms: [],
    consumer_transforms: [],
    consumer_rule_types: ["mask_pii", "type_convert"],
  }

  it("offers only the operations that run, and says why", async () => {
    serve({ plan: LIMITED })
    renderEditor()
    const user = userEvent.setup()
    await waitFor(() => expect(screen.getByText("No transforms")).toBeInTheDocument())

    await user.click(screen.getByRole("button", { name: "Add" }))
    const dialog = await screen.findByRole("dialog")
    expect(within(dialog).getByText(/only Mask PII and Convert types can run on it/)).toBeInTheDocument()
    expect(within(dialog).queryByRole("button", { name: /Filter Rows/ })).toBeNull()
    expect(within(dialog).getByRole("button", { name: /Hash Column/ })).toBeInTheDocument()
    expect(within(dialog).getByRole("button", { name: /Type Conversion/ })).toBeInTheDocument()
  })

  it("leaves a generated rule that cannot run out of the plan, and names it", async () => {
    serve({
      plan: LIMITED,
      parse: {
        status: 200,
        body: {
          transforms: [
            { transform_type: "producer", transform_order: 0, transform_config: { operation: "filter", condition: "amount > 100" }, enabled: true },
            { transform_type: "producer", transform_order: 1, transform_config: { operation: "mask", column: "email", mask_type: "hash" }, enabled: true },
          ],
        },
      },
    })
    renderEditor()
    const user = userEvent.setup()
    await waitFor(() => expect(screen.getByText("No transforms")).toBeInTheDocument())

    await user.type(nlBox(), "keep big orders and mask every email")
    await user.click(screen.getByRole("button", { name: /generate/i }))

    expect(await screen.findByText("Mask PII")).toBeInTheDocument()
    expect(screen.queryByText("Filter Rows")).toBeNull()
    expect(screen.getByText(/Left out: 1 generated rule \(Filter rows\)/)).toBeInTheDocument()
  })

  // The control: with no list from the server, Generate keeps every rule.
  it("keeps every generated rule when the pipeline names no limit", async () => {
    serve({
      parse: {
        status: 200,
        body: {
          transforms: [
            { transform_type: "producer", transform_order: 0, transform_config: { operation: "filter", condition: "amount > 100" }, enabled: true },
          ],
        },
      },
    })
    renderEditor()
    const user = userEvent.setup()
    await waitFor(() => expect(screen.getByText("No transforms")).toBeInTheDocument())

    await user.type(nlBox(), "keep big orders")
    await user.click(screen.getByRole("button", { name: /generate/i }))

    expect(await screen.findByText("Filter Rows")).toBeInTheDocument()
    expect(screen.queryByText(/Left out:/)).toBeNull()
  })
})
