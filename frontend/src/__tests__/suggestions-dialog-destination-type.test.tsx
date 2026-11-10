/**
 * The setup suggestions must know where the data lands, and must not read as
 * something Save & continue will do when it will not.
 *
 * Prod 2026-09-26 (pipeline 93e1e11d, Postgres -> GCS): the dialog offered 14
 * "Add index on ..." optimizations for an object store, because it told the
 * service the destination was the placeholder "storage". The button read
 * "Apply & Continue (1)" (the PII rule), under copy that said everything listed
 * "will be applied". Nothing applies an optimization, on any destination.
 * (The button is "Save & continue (N)" since the transform step counts its plan.)
 */
import { describe, it, expect, vi, beforeAll, beforeEach, afterEach } from "vitest"
import { render, screen, cleanup, waitFor } from "@testing-library/react"
import userEvent from "@testing-library/user-event"
import "@testing-library/jest-dom"

beforeAll(() => {
  window.ResizeObserver ||= class {
    observe() {}
    unobserve() {}
    disconnect() {}
  } as unknown as typeof ResizeObserver
})

vi.mock("@/lib/api/auth-fetch", () => ({
  authFetch: vi.fn(async () => ({ ok: true, status: 200, json: async () => ({}) })),
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

const schema = [{ name: "users", columns: [{ name: "id", type: "int" }, { name: "email", type: "varchar" }] }]

function open(
  destinationType?: string,
  onApplyContinue: (saved: { count: number }) => Promise<void> = vi.fn(async () => {})
) {
  return render(
    <SuggestionsReviewDialog
      isOpen
      onClose={vi.fn()}
      pipelineId="p1"
      sourceConnectionId="c1"
      selectedTables={["users"]}
      onApplyContinue={onApplyContinue}
      prefetchedSchema={schema}
      destinationType={destinationType}
    />
  )
}

function sentDestination(): unknown {
  const req = generateSuggestions.mock.calls[0]?.[0] as { intent?: { destination_type?: unknown } } | undefined
  return req?.intent?.destination_type
}

beforeEach(() => {
  toastSuccess.mockReset()
  generateSuggestions.mockReset()
  generateSuggestions.mockResolvedValue({
    pii_columns: [{ column: "users.email", pii_types: ["email"], confidence: "high", suggested_action: "mask" }],
    transforms: [],
    optimizations: [{ type: "compression", suggestion: "Enable Parquet compression (snappy) for storage efficiency" }],
  })
})
afterEach(() => cleanup())

describe("SuggestionsReviewDialog — the destination it asks about", () => {
  it("sends the pipeline's destination type", async () => {
    open("gcs")
    await screen.findByText(/Save & continue/)
    expect(sentDestination()).toBe("gcs")
  })

  it("sends 'unknown', never the old 'storage' placeholder, when the type is not loaded", async () => {
    open(undefined)
    await screen.findByText(/Save & continue/)
    expect(sentDestination()).toBe("unknown")
  })
})

describe("SuggestionsReviewDialog — optimizations are advice", () => {
  it("says so in the description and on the tab, and the Save count leaves them out", async () => {
    const user = userEvent.setup({ pointerEventsCheck: 0 })
    open("gcs")
    expect(await screen.findByText("Save & continue (1)")).toBeInTheDocument()
    expect(screen.getByText(/Optimizations are advice only/)).toBeInTheDocument()
    expect(screen.queryByText(/These will be applied to\s+your data/)).toBeNull()

    await user.click(screen.getByRole("tab", { name: /Optimizations \(1\)/ }))
    expect(await screen.findByText(/^Advice only: Save & continue does not change any of these\.$/)).toBeInTheDocument()
    expect(screen.getByText(/Enable Parquet compression/)).toBeInTheDocument()
  })
})

// Prod 2026-10-01: "Transforms saved…" stayed on screen after the pipeline had
// moved on. It was raised before the resume was sent, so it outlived the
// dialog, and it said "saved" even when the resume then failed.
describe("SuggestionsReviewDialog — the 'Transforms saved' toast", () => {
  it("is raised only once the resume has gone through, and dismisses itself", async () => {
    const user = userEvent.setup({ pointerEventsCheck: 0 })
    let finish!: () => void
    const onApplyContinue = vi.fn(() => new Promise<void>((resolve) => (finish = resolve)))
    open("gcs", onApplyContinue)
    await user.click(await screen.findByText("Save & continue (1)"))

    await waitFor(() => expect(onApplyContinue).toHaveBeenCalledTimes(1))
    expect(toastSuccess).not.toHaveBeenCalled()

    finish()
    await waitFor(() => expect(toastSuccess).toHaveBeenCalledTimes(1))
    expect(toastSuccess.mock.calls[0][0]).toMatch(/^Transforms saved/)
    expect(toastSuccess.mock.calls[0][1]).toMatchObject({ duration: 5000 })
  })

  it("is not raised when the resume fails, and the error shows in the dialog", async () => {
    const user = userEvent.setup({ pointerEventsCheck: 0 })
    const onApplyContinue = vi.fn(async () => {
      throw new Error("resume refused")
    })
    open("gcs", onApplyContinue)
    await user.click(await screen.findByText("Save & continue (1)"))

    expect(await screen.findByText(/resume refused/)).toBeInTheDocument()
    expect(toastSuccess).not.toHaveBeenCalled()
  })
})
