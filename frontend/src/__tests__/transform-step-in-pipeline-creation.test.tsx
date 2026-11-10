/**
 * The transform step of pipeline creation: the dialog that opens after the tables
 * are picked.
 *
 * It used to replace the pipeline's saved rules with the checked suggestions
 * without reading them first (the save is a REPLACE, so a second pass through
 * this step deleted what the Transform Builder had saved), and it offered no way
 * to add a rule of the operator's own. These pin the step that replaced it: every
 * rule it saves runs on both sides, the rules already saved are kept, and a rule
 * added here picks its table and columns from the selected tables.
 */
import { describe, it, expect, vi, beforeAll, beforeEach, afterEach } from "vitest"
import { render, screen, cleanup, waitFor, within } from "@testing-library/react"
import userEvent from "@testing-library/user-event"
import "@testing-library/jest-dom"

beforeAll(() => {
  window.ResizeObserver ||= class {
    observe() {}
    unobserve() {}
    disconnect() {}
  } as unknown as typeof ResizeObserver
})

type Row = {
  id: string
  transform_type: "producer" | "consumer"
  transform_order: number
  transform_config: Record<string, unknown>
  enabled: boolean
}

// A stand-in for the gateway's /transforms/pipeline/:id: GET answers `saved`
// (or `loadStatus`), POST records the body it was sent.
const gw = vi.hoisted(() => ({
  saved: [] as Row[],
  loadStatus: [] as number[],
  posts: [] as { transforms: Row[] }[],
  gets: 0,
  // What GET says a consumer rule may be on this pipeline; only a MongoDB CDC
  // pipeline names it (transforms.go GetPipelineTransforms).
  consumerRuleTypes: undefined as string[] | undefined,
}))

vi.mock("@/lib/api/auth-fetch", () => ({
  authFetch: vi.fn(async (url: string, init?: RequestInit) => {
    const reply = (status: number, body: unknown) =>
      new Response(JSON.stringify(body), { status, headers: { "Content-Type": "application/json" } })
    if (String(url).includes("/transforms/pipeline/")) {
      if ((init?.method || "GET") === "POST") {
        gw.posts.push(JSON.parse(String(init?.body)))
        return reply(200, { saved: 0 })
      }
      gw.gets++
      const status = gw.loadStatus.shift() ?? 200
      if (status !== 200) return reply(status, { error: "transform store is down" })
      return reply(200, {
        producer_transforms: gw.saved.filter((r) => r.transform_type === "producer"),
        consumer_transforms: gw.saved.filter((r) => r.transform_type === "consumer"),
        ...(gw.consumerRuleTypes ? { consumer_rule_types: gw.consumerRuleTypes } : {}),
      })
    }
    return reply(200, {})
  }),
}))

const toastSuccess = vi.fn()
vi.mock("sonner", () => ({
  toast: { success: (...args: unknown[]) => toastSuccess(...args), error: vi.fn(), info: vi.fn(), dismiss: vi.fn() },
}))

const generateSuggestions = vi.fn()
vi.mock("@/lib/api/suggestions", () => ({
  generateSuggestions: (...args: unknown[]) => generateSuggestions(...args),
}))

import { SuggestionsReviewDialog } from "@/components/chat/SuggestionsReviewDialog"

const SAVED_P = "11111111-1111-4111-8111-111111111111"
const SAVED_C = "22222222-2222-4222-8222-222222222222"

const schema = [
  { name: "users", columns: [{ name: "id", type: "int" }, { name: "email", type: "varchar" }] },
  { name: "orders", columns: [{ name: "id", type: "int" }, { name: "amount", type: "numeric" }] },
]

function savedPair(column: string): Row[] {
  const transform_config = { operation: "mask", column, mask_type: "redact" }
  return [
    { id: SAVED_P, transform_type: "producer", transform_order: 0, transform_config, enabled: true },
    { id: SAVED_C, transform_type: "consumer", transform_order: 1, transform_config, enabled: true },
  ]
}

function open(onApplyContinue = vi.fn<(saved: { count: number }) => Promise<void>>(async () => {})) {
  render(
    <SuggestionsReviewDialog
      isOpen
      onClose={vi.fn()}
      pipelineId="p1"
      sourceConnectionId="c1"
      selectedTables={["users", "orders"]}
      onApplyContinue={onApplyContinue}
      prefetchedSchema={schema}
      destinationType="mongodb"
    />
  )
  return onApplyContinue
}

function suggest(pii: boolean) {
  generateSuggestions.mockResolvedValue({
    pii_columns: pii
      ? [{ column: "users.email", pii_types: ["email"], confidence: "high", suggested_action: "mask" }]
      : [],
    transforms: [],
    optimizations: [],
  })
}

async function pick(user: ReturnType<typeof userEvent.setup>, trigger: string, option: string) {
  await user.click(screen.getByRole("combobox", { name: trigger }))
  await user.click(await screen.findByRole("option", { name: option }))
}

beforeEach(() => {
  gw.saved = []
  gw.loadStatus = []
  gw.posts = []
  gw.gets = 0
  gw.consumerRuleTypes = undefined
  toastSuccess.mockReset()
  generateSuggestions.mockReset()
})
afterEach(() => cleanup())

describe("transform step — every rule it saves runs on batch and on CDC", () => {
  it("saves a checked PII suggestion as a producer row and a consumer row", async () => {
    const user = userEvent.setup({ pointerEventsCheck: 0 })
    suggest(true)
    const onApplyContinue = open()
    await user.click(await screen.findByRole("button", { name: "Save & continue (1)" }))

    await waitFor(() => expect(onApplyContinue).toHaveBeenCalledWith({ count: 1 }))
    expect(gw.posts).toHaveLength(1)
    const rows = gw.posts[0].transforms
    expect(rows.map((r) => r.transform_type)).toEqual(["producer", "consumer"])
    for (const r of rows) {
      expect(r.transform_config).toMatchObject({ operation: "mask_pii", column: "users.email", mask_type: "hash" })
    }
  })

  it("adds the operator's own rule, scoped to the table and column picked from the schema", async () => {
    const user = userEvent.setup({ pointerEventsCheck: 0 })
    suggest(false)
    const onApplyContinue = open()
    await screen.findByRole("button", { name: "Save & continue (0)" })

    await user.click(screen.getByRole("tab", { name: /Add a transform/ }))
    await pick(user, "Operation", "Hash Column")
    await pick(user, "Applies to", "orders")

    // Scoped to orders, the column list is orders' columns only.
    await user.click(screen.getByRole("combobox", { name: "Column" }))
    const listbox = await screen.findByRole("listbox")
    expect(within(listbox).queryByRole("option", { name: "email" })).toBeNull()
    await user.click(within(listbox).getByRole("option", { name: "amount" }))

    await user.click(screen.getByRole("button", { name: /Add to plan/ }))
    await user.click(await screen.findByRole("button", { name: "Save & continue (1)" }))

    await waitFor(() => expect(onApplyContinue).toHaveBeenCalledWith({ count: 1 }))
    const rows = gw.posts[0].transforms
    expect(rows.map((r) => r.transform_type)).toEqual(["producer", "consumer"])
    for (const r of rows) {
      expect(r.transform_config).toMatchObject({ operation: "hash", column: "amount", table: "orders" })
    }
  })

  it("will not add a rule that is missing a required field", async () => {
    const user = userEvent.setup({ pointerEventsCheck: 0 })
    suggest(false)
    open()
    await screen.findByRole("button", { name: "Save & continue (0)" })

    await user.click(screen.getByRole("tab", { name: /Add a transform/ }))
    await pick(user, "Operation", "Hash Column")
    await user.click(screen.getByRole("button", { name: /Add to plan/ }))

    expect(await screen.findByRole("alert")).toHaveTextContent("Column is required.")
    expect(screen.getByRole("button", { name: "Save & continue (0)" })).toBeInTheDocument()
  })
})

describe("transform step — the pipeline's saved rules", () => {
  it("keeps them, first and with their ids, when it adds a rule", async () => {
    const user = userEvent.setup({ pointerEventsCheck: 0 })
    gw.saved = savedPair("ssn")
    suggest(true)
    const onApplyContinue = open()
    await user.click(await screen.findByRole("button", { name: "Save & continue (2)" }))

    await waitFor(() => expect(gw.posts).toHaveLength(1))
    const rows = gw.posts[0].transforms
    expect(rows).toHaveLength(4)
    expect(rows.slice(0, 2).map((r) => [r.id, r.transform_type, r.transform_config.column])).toEqual([
      [SAVED_P, "producer", "ssn"],
      [SAVED_C, "consumer", "ssn"],
    ])
    expect(rows.slice(2).map((r) => r.transform_config.column)).toEqual(["users.email", "users.email"])
    await waitFor(() => expect(onApplyContinue).toHaveBeenCalledWith({ count: 1 }))
  })

  it("writes nothing when the step adds nothing", async () => {
    const user = userEvent.setup({ pointerEventsCheck: 0 })
    gw.saved = savedPair("ssn")
    suggest(false)
    const onApplyContinue = open()
    expect(await screen.findByRole("button", { name: "Skip — add nothing" })).toBeInTheDocument()
    await user.click(await screen.findByRole("button", { name: "Save & continue (1)" }))

    await waitFor(() => expect(onApplyContinue).toHaveBeenCalledWith({ count: 0 }))
    expect(gw.posts).toHaveLength(0)
    expect(toastSuccess).not.toHaveBeenCalled()
  })

  it("will not save while they have not loaded, and loads them again on Retry", async () => {
    const user = userEvent.setup({ pointerEventsCheck: 0 })
    gw.saved = savedPair("ssn")
    gw.loadStatus = [500]
    suggest(true)
    const onApplyContinue = open()

    const alert = await screen.findByText(/Couldn.t load this pipeline.s saved transforms/)
    expect(alert).toHaveTextContent(/could delete them/)
    expect(screen.getByRole("button", { name: /Save & continue/ })).toBeDisabled()

    await user.click(screen.getByRole("button", { name: "Retry" }))
    const save = await screen.findByRole("button", { name: "Save & continue (2)" })
    await waitFor(() => expect(save).toBeEnabled())
    expect(gw.gets).toBe(2)

    await user.click(save)
    await waitFor(() => expect(onApplyContinue).toHaveBeenCalledWith({ count: 1 }))
    expect(gw.posts[0].transforms.slice(0, 2).map((r) => r.id)).toEqual([SAVED_P, SAVED_C])
  })
})

// A MongoDB CDC change reaches the consumer rules packed as {_id, document}, so
// only mask_pii and type_convert run on it, and the save refuses any other rule.
// The step offered and pre-checked array_expand there anyway: the first save of a
// new MongoDB CDC pipeline was refused, and none of its suggestions were kept.
describe("transform step — a pipeline that can run only some rule types", () => {
  function suggestMixed() {
    generateSuggestions.mockResolvedValue({
      pii_columns: [{ column: "users.email", pii_types: ["email"], confidence: "high", suggested_action: "mask" }],
      transforms: [
        {
          type: "array_expand",
          config: { column: "users.tags", output_prefix: "tags_", max_elements: 10 },
          summary: "Expand array column tags",
          reason: "Splits array elements into wide columns",
        },
        {
          type: "type_convert",
          config: { column: "orders.amount", to: "float" },
          summary: "Convert amount to float",
          reason: "Numeric text",
        },
      ],
      optimizations: [],
    })
  }

  it("offers and saves only the types the pipeline names, and says what it left out", async () => {
    const user = userEvent.setup({ pointerEventsCheck: 0 })
    gw.consumerRuleTypes = ["mask_pii", "type_convert"]
    suggestMixed()
    const onApplyContinue = open()

    await user.click(await screen.findByRole("tab", { name: "Transforms (1)" }))
    expect(screen.getByText(/Left out: 1 suggestion \(Expand array columns\)/)).toBeInTheDocument()
    expect(screen.queryByText("Expand array columns")).toBeNull()

    await user.click(screen.getByRole("button", { name: "Save & continue (2)" }))
    await waitFor(() => expect(onApplyContinue).toHaveBeenCalledWith({ count: 2 }))
    expect(gw.posts[0].transforms.map((r) => r.transform_config.operation)).toEqual([
      "mask_pii",
      "mask_pii",
      "type_convert",
      "type_convert",
    ])
  })

  it("offers only those operations for a rule of the operator's own", async () => {
    const user = userEvent.setup({ pointerEventsCheck: 0 })
    gw.consumerRuleTypes = ["mask_pii", "type_convert"]
    suggest(false)
    open()
    await screen.findByRole("button", { name: "Save & continue (0)" })

    await user.click(screen.getByRole("tab", { name: /Add a transform/ }))
    await screen.findByText(/only Mask PII and Convert types can run on it/)
    await user.click(screen.getByRole("combobox", { name: "Operation" }))
    const listbox = await screen.findByRole("listbox")
    expect(within(listbox).getAllByRole("option").map((o) => o.textContent)).toEqual([
      "Mask PII",
      "Hash Column",
      "Type Conversion",
    ])
  })

  // The control: with no list from the server, nothing is held back.
  it("offers every suggestion when the pipeline names no limit", async () => {
    const user = userEvent.setup({ pointerEventsCheck: 0 })
    suggestMixed()
    const onApplyContinue = open()

    await user.click(await screen.findByRole("button", { name: "Save & continue (3)" }))
    await waitFor(() => expect(onApplyContinue).toHaveBeenCalledWith({ count: 3 }))
    expect(gw.posts[0].transforms.map((r) => r.transform_config.operation)).toContain("array_expand")
    expect(screen.queryByText(/Left out:/)).toBeNull()
  })
})

describe("transform step — what the operator is told", () => {
  it("lists the plan and says changes apply only to data moved after the save", async () => {
    const user = userEvent.setup({ pointerEventsCheck: 0 })
    suggest(true)
    open()
    expect(await screen.findByRole("button", { name: "Skip — no transforms" })).toBeInTheDocument()
    expect(
      screen.getByText("You can change these later in the pipeline's Transforms tab; changes apply to data moved after the save.")
    ).toBeInTheDocument()

    await user.click(screen.getByRole("tab", { name: /Plan \(1\)/ }))
    const plan = await screen.findByTestId("transform-plan")
    expect(plan).toHaveTextContent(/users\.email|email/)
  })
})

// KI-TRANSFORM-STEP-CLOSE-DISCARDS-RULES: Escape and X both called
// the Skip resume, so rules added here were thrown away without a word and the
// pipeline started copying without them. Live, it was an Escape meant for an open
// column dropdown that did it. Only Skip skips without asking now.
describe("transform step — closing it", () => {
  function openStep() {
    const onClose = vi.fn(async () => {})
    const onApplyContinue = vi.fn(async () => {})
    render(
      <SuggestionsReviewDialog
        isOpen
        onClose={onClose}
        pipelineId="p1"
        sourceConnectionId="c1"
        selectedTables={["users", "orders"]}
        onApplyContinue={onApplyContinue}
        prefetchedSchema={schema}
        destinationType="mongodb"
      />
    )
    return { onClose, onApplyContinue }
  }

  async function addHashRule(user: ReturnType<typeof userEvent.setup>) {
    await user.click(screen.getByRole("tab", { name: /Add a transform/ }))
    await pick(user, "Operation", "Hash Column")
    await pick(user, "Applies to", "orders")
    await pick(user, "Column", "amount")
    await user.click(screen.getByRole("button", { name: /Add to plan/ }))
    await screen.findByRole("button", { name: "Save & continue (1)" })
  }

  it("asks before Escape discards a rule added here, and Keep editing keeps it", async () => {
    const user = userEvent.setup({ pointerEventsCheck: 0 })
    suggest(false)
    const { onClose } = openStep()
    await screen.findByRole("button", { name: "Save & continue (0)" })
    await addHashRule(user)

    await user.keyboard("{Escape}")
    expect(onClose).not.toHaveBeenCalled()
    expect(screen.getByText("Discard 1 rule and start without it?")).toBeInTheDocument()

    await user.click(screen.getByRole("button", { name: "Keep editing" }))
    expect(screen.queryByText("Discard 1 rule and start without it?")).toBeNull()
    expect(screen.getByRole("button", { name: "Save & continue (1)" })).toBeInTheDocument()
    expect(onClose).not.toHaveBeenCalled()
  })

  it("asks on the X too", async () => {
    const user = userEvent.setup({ pointerEventsCheck: 0 })
    suggest(false)
    const { onClose } = openStep()
    await screen.findByRole("button", { name: "Save & continue (0)" })
    await addHashRule(user)

    await user.click(screen.getByRole("button", { name: "Close" }))
    expect(onClose).not.toHaveBeenCalled()
    expect(screen.getByText("Discard 1 rule and start without it?")).toBeInTheDocument()
  })

  it("skips, saving nothing, once Discard and start is chosen", async () => {
    const user = userEvent.setup({ pointerEventsCheck: 0 })
    suggest(false)
    const { onClose, onApplyContinue } = openStep()
    await screen.findByRole("button", { name: "Save & continue (0)" })
    await addHashRule(user)

    await user.keyboard("{Escape}")
    await user.click(screen.getByRole("button", { name: "Discard and start" }))
    await waitFor(() => expect(onClose).toHaveBeenCalledTimes(1))
    expect(onApplyContinue).not.toHaveBeenCalled()
    expect(gw.posts).toHaveLength(0)
  })

  it("treats a changed suggestion as a change worth asking about", async () => {
    const user = userEvent.setup({ pointerEventsCheck: 0 })
    // pii_detected is what shows the PII cards, and their checkboxes.
    generateSuggestions.mockResolvedValue({
      pii_detected: true,
      pii_columns: [{ column: "users.email", pii_types: ["email"], confidence: "high", suggested_action: "mask" }],
      transforms: [],
      optimizations: [],
    })
    const { onClose } = openStep()
    await screen.findByRole("button", { name: "Save & continue (1)" })

    await user.click(screen.getAllByRole("checkbox")[0])
    await screen.findByRole("button", { name: "Save & continue (0)" })
    await user.keyboard("{Escape}")
    expect(onClose).not.toHaveBeenCalled()
    expect(screen.getByText("Discard your changes and start without new transforms?")).toBeInTheDocument()
  })

  // The controls: the two ways out that must stay one step.
  it("still skips at once from the Skip button, rules or not", async () => {
    const user = userEvent.setup({ pointerEventsCheck: 0 })
    suggest(false)
    const { onClose } = openStep()
    await screen.findByRole("button", { name: "Save & continue (0)" })
    await addHashRule(user)

    await user.click(screen.getByRole("button", { name: "Skip — no transforms" }))
    await waitFor(() => expect(onClose).toHaveBeenCalledTimes(1))
    expect(screen.queryByRole("button", { name: "Discard and start" })).toBeNull()
  })

  it("still closes at once on Escape when nothing was changed", async () => {
    const user = userEvent.setup({ pointerEventsCheck: 0 })
    suggest(true)
    const { onClose } = openStep()
    await screen.findByRole("button", { name: "Save & continue (1)" })

    await user.keyboard("{Escape}")
    await waitFor(() => expect(onClose).toHaveBeenCalledTimes(1))
    expect(screen.queryByRole("button", { name: "Discard and start" })).toBeNull()
  })

  // The live repro: the dropdown handled the Escape, and the dialog closed too.
  it("lets Escape close an open dropdown without closing the step", async () => {
    const user = userEvent.setup({ pointerEventsCheck: 0 })
    suggest(false)
    const { onClose } = openStep()
    await screen.findByRole("button", { name: "Save & continue (0)" })
    await user.click(screen.getByRole("tab", { name: /Add a transform/ }))
    await user.click(screen.getByRole("combobox", { name: "Operation" }))
    await screen.findByRole("listbox")

    await user.keyboard("{Escape}")
    await waitFor(() => expect(screen.queryByRole("listbox")).toBeNull())
    expect(onClose).not.toHaveBeenCalled()
    expect(screen.getByRole("button", { name: "Save & continue (0)" })).toBeInTheDocument()
  })
})
