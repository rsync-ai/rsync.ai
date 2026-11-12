/**
 * "Flag Overdue after…" on the model page (ModelFreshnessDeadline.tsx). The gateway
 * stored a freshness deadline (PUT /explorer/saved/:id/freshness) and the sweep flagged
 * models past it, but nothing in the UI could read or set the deadline.
 *
 * GET /explorer/saved/:id/freshness also answers the longest gap the schedules above the
 * model can leave between rebuilds. Loan portfolio had a 6h deadline and rebuilt daily, so
 * it showed Overdue every day with every run green; the row and the dialog now say so.
 */

import { afterEach, beforeEach, describe, expect, it, vi } from "vitest"
import type { Mock } from "vitest"
import { cleanup, render, screen, waitFor, within } from "@testing-library/react"
import userEvent from "@testing-library/user-event"
import "@testing-library/jest-dom"

import { authFetch } from "@/lib/api/auth-fetch"
import {
  ModelFreshnessDeadline,
  deadlineFrom,
  splitDeadline,
  suggestDeadline,
} from "@/components/explorer/ModelFreshnessDeadline"

vi.mock("@/lib/api/auth-fetch", () => ({ authFetch: vi.fn() }))
vi.mock("sonner", () => ({ toast: { success: vi.fn(), error: vi.fn() } }))

const mockFetch = authFetch as unknown as Mock
const ID = "6f1c0a52-3c52-4a0e-9d7c-2d2f0f6a1b11"
const SAVED = `/api/v1/explorer/saved/${ID}`
const PUT_URL = `${SAVED}/freshness`

function res(body: unknown, status = 200) {
  return { ok: status < 400, status, json: async () => body } as unknown as Response
}

/**
 * GET answers with the stored deadline and the rebuild interval; PUT stores what it is
 * sent, as the gateway does. The returned setter moves the interval, as a schedule edit would.
 */
function gateway(
  initial: number | null,
  opts: { put?: { status: number; body: unknown }; interval?: number | null } = {},
) {
  let stored = initial
  let interval = opts.interval ?? null
  mockFetch.mockImplementation(async (url: string, init?: RequestInit) => {
    if (url === PUT_URL && init?.method === "PUT") {
      if (opts.put) return res(opts.put.body, opts.put.status)
      stored = JSON.parse(String(init.body)).deadline_seconds
      return res({ saved_query_id: ID, deadline_seconds: stored })
    }
    if (url === PUT_URL) {
      return res({ saved_query_id: ID, deadline_seconds: stored, rebuild_interval_seconds: interval })
    }
    return res({}, 404)
  })
  return (next: number | null) => {
    interval = next
  }
}

const LOAN_NOTE = "Its schedule can leave 1d between rebuilds, so it shows Overdue between them."

function putBodies() {
  return mockFetch.mock.calls
    .filter(([url, init]) => url === PUT_URL && (init as RequestInit)?.method === "PUT")
    .map(([, init]) => JSON.parse(String((init as RequestInit).body)))
}

beforeEach(() => {
  mockFetch.mockReset()
})
afterEach(() => cleanup())

function renderRow(props: Partial<Parameters<typeof ModelFreshnessDeadline>[0]> = {}) {
  const onSaved = vi.fn()
  const ui = (id: string) => (
    <ModelFreshnessDeadline savedQueryId={id} materialization="table" canEdit onSaved={onSaved} {...props} />
  )
  const { rerender } = render(ui(ID))
  rerenderWith = (id: string, more = {}) =>
    rerender(
      <ModelFreshnessDeadline savedQueryId={id} materialization="table" canEdit onSaved={onSaved} {...props} {...more} />,
    )
  return { onSaved, row: screen.getByTestId("model-freshness-deadline") }
}
let rerenderWith: (id: string, more?: Partial<Parameters<typeof ModelFreshnessDeadline>[0]>) => void = () => {}

describe("ModelFreshnessDeadline row", () => {
  it("shows the stored deadline", async () => {
    gateway(21600)
    const { row } = renderRow()
    expect(await within(row).findByText("Flag Overdue after 6h")).toBeInTheDocument()
    expect(within(row).getByRole("button", { name: "Edit freshness deadline" })).toBeEnabled()
  })

  it("says there is none, and offers to set one", async () => {
    gateway(null)
    const { row } = renderRow()
    expect(await within(row).findByText("No deadline")).toBeInTheDocument()
    expect(within(row).getByRole("button", { name: "Set freshness deadline" })).toBeInTheDocument()
  })

  it("marks a deadline the sweep will not check, on a model that rebuilds no table", async () => {
    gateway(3600)
    const { row } = renderRow({ materialization: "statement" })
    await within(row).findByText("Flag Overdue after 1h")
    expect(row).toHaveTextContent("not checked")
  })

  it("does not show the previous model's deadline while the next one loads", async () => {
    // The model page stays mounted when a graph link moves it to another model.
    gateway(21600)
    const { row } = renderRow()
    await within(row).findByText("Flag Overdue after 6h")

    const OTHER = "0b0c3d1e-4f5a-4b6c-8d7e-9f0a1b2c3d4e"
    mockFetch.mockImplementation(() => new Promise(() => {}))
    rerenderWith(OTHER)
    expect(within(row).queryByText("Flag Overdue after 6h")).toBeNull()
    expect(row).toHaveTextContent("Loading…")
  })

  it("locks the control for a role below the model-run bar, and says why", async () => {
    gateway(3600)
    const reason = "Setting a freshness deadline needs the admin role or higher. Your role is member."
    const { row } = renderRow({ canEdit: false, disabledReason: reason })
    const edit = await within(row).findByRole("button", { name: "Edit freshness deadline" })
    expect(edit).toBeDisabled()
    expect(edit).toHaveAttribute("title", reason)
  })

  it("warns when the deadline is shorter than the gap its schedule leaves", async () => {
    // Loan portfolio: 6h, rebuilt once a day after the Nightly.
    gateway(21600, { interval: 86400 })
    const { row } = renderRow()
    expect(await within(row).findByRole("note")).toHaveTextContent(LOAN_NOTE)
  })

  it("warns at a deadline equal to the gap: the rebuild lands after it", async () => {
    gateway(86400, { interval: 86400 })
    const { row } = renderRow()
    expect(await within(row).findByRole("note")).toHaveTextContent(LOAN_NOTE)
  })

  it.each([
    ["a deadline over the gap", 97200, 86400, "table"],
    ["a gap the gateway cannot predict", 21600, null, "table"],
    ["no deadline", null, 86400, "table"],
    ["a model the sweep does not check", 21600, 86400, "statement"],
  ])("says nothing for %s", async (_name, deadline, interval, materialization) => {
    gateway(deadline, { interval })
    const { row } = renderRow({ materialization })
    await within(row).findByRole("button", { name: /freshness deadline/ })
    expect(within(row).queryByRole("note")).toBeNull()
  })

  it("reads the interval again when the model's schedule changes", async () => {
    const setInterval = gateway(21600, { interval: 3600 })
    const { row } = renderRow({ scheduleKey: "active|2026-10-23T12:00:00Z" })
    await within(row).findByText("Flag Overdue after 6h")
    expect(within(row).queryByRole("note")).toBeNull()

    setInterval(86400)
    rerenderWith(ID, { scheduleKey: "active|2026-10-23T12:05:00Z" })
    expect(await within(row).findByRole("note")).toHaveTextContent(LOAN_NOTE)
  })
})

describe("ModelFreshnessDeadline dialog", () => {
  it("saves a preset as seconds and shows the new value", async () => {
    gateway(null)
    const user = userEvent.setup()
    const { row, onSaved } = renderRow()
    await user.click(await within(row).findByRole("button", { name: "Set freshness deadline" }))

    const dialog = await screen.findByRole("dialog")
    await user.click(within(dialog).getByRole("button", { name: "1 day" }))
    await user.click(within(dialog).getByRole("button", { name: "Save" }))

    await waitFor(() => expect(screen.queryByRole("dialog")).toBeNull())
    expect(putBodies()).toEqual([{ deadline_seconds: 86400 }])
    expect(within(row).getByText("Flag Overdue after 1d")).toBeInTheDocument()
    expect(onSaved).toHaveBeenCalledTimes(1)
  })

  it("saves a typed amount in the chosen unit", async () => {
    gateway(21600)
    const user = userEvent.setup()
    const { row } = renderRow()
    await user.click(await within(row).findByRole("button", { name: "Edit freshness deadline" }))

    const dialog = await screen.findByRole("dialog")
    const amount = within(dialog).getByLabelText("Flag Overdue after")
    expect(amount).toHaveValue(6)
    await user.clear(amount)
    await user.type(amount, "90")
    await user.selectOptions(within(dialog).getByRole("combobox", { name: "Unit" }), "minutes")
    await user.click(within(dialog).getByRole("button", { name: "Save" }))

    await waitFor(() => expect(putBodies()).toEqual([{ deadline_seconds: 5400 }]))
    expect(await within(row).findByText("Flag Overdue after 1h 30m")).toBeInTheDocument()
  })

  it("removes the deadline with null", async () => {
    gateway(3600)
    const user = userEvent.setup()
    const { row } = renderRow()
    await user.click(await within(row).findByRole("button", { name: "Edit freshness deadline" }))
    await user.click(within(await screen.findByRole("dialog")).getByRole("button", { name: "Remove deadline" }))

    await waitFor(() => expect(putBodies()).toEqual([{ deadline_seconds: null }]))
    expect(await within(row).findByText("No deadline")).toBeInTheDocument()
  })

  it("refuses an out-of-range deadline before asking the server", async () => {
    gateway(null)
    const user = userEvent.setup()
    const { row } = renderRow()
    await user.click(await within(row).findByRole("button", { name: "Set freshness deadline" }))

    const dialog = await screen.findByRole("dialog")
    const amount = within(dialog).getByLabelText("Flag Overdue after")
    await user.clear(amount)
    await user.type(amount, "400")
    await user.selectOptions(within(dialog).getByRole("combobox", { name: "Unit" }), "days")

    expect(dialog).toHaveTextContent("A deadline must be between 1 minute and 365 days.")
    expect(within(dialog).getByRole("button", { name: "Save" })).toBeDisabled()
    expect(putBodies()).toEqual([])
  })

  it("keeps the dialog open and shows the server's reason when the save is refused", async () => {
    const reason = "freshness deadline must be between 60 seconds and one year, in SECONDS"
    gateway(null, { put: { status: 400, body: { error: reason } } })
    const user = userEvent.setup()
    const { row, onSaved } = renderRow()
    await user.click(await within(row).findByRole("button", { name: "Set freshness deadline" }))
    const dialog = await screen.findByRole("dialog")
    await user.click(within(dialog).getByRole("button", { name: "Save" }))

    expect(await within(dialog).findByRole("alert")).toHaveTextContent(reason)
    expect(within(row).getByText("No deadline")).toBeInTheDocument()
    expect(onSaved).not.toHaveBeenCalled()
  })

  it("reopens on what is stored, not on an edit that was cancelled", async () => {
    gateway(21600)
    const user = userEvent.setup()
    const { row } = renderRow()
    const edit = await within(row).findByRole("button", { name: "Edit freshness deadline" })
    await user.click(edit)
    let dialog = await screen.findByRole("dialog")
    await user.click(within(dialog).getByRole("button", { name: "7 days" }))
    await user.click(within(dialog).getByRole("button", { name: "Cancel" }))
    await waitFor(() => expect(screen.queryByRole("dialog")).toBeNull())

    await user.click(edit)
    dialog = await screen.findByRole("dialog")
    expect(within(dialog).getByLabelText("Flag Overdue after")).toHaveValue(6)
    expect(within(dialog).getByRole("combobox", { name: "Unit" })).toHaveValue("hours")
    expect(putBodies()).toEqual([])
  })

  it("warns that widening clears an Overdue flag as widened, not fixed", async () => {
    gateway(3600)
    const user = userEvent.setup()
    const { row } = renderRow()
    await user.click(await within(row).findByRole("button", { name: "Edit freshness deadline" }))
    const dialog = await screen.findByRole("dialog")
    await user.click(within(dialog).getByRole("button", { name: "12 hours" }))
    expect(dialog).toHaveTextContent("clears an Overdue flag as “deadline widened”, not as fixed")
  })

  it("offers a deadline the schedule leaves room for, and the note goes once it fits", async () => {
    gateway(21600, { interval: 86400 })
    const user = userEvent.setup()
    const { row } = renderRow()
    await user.click(await within(row).findByRole("button", { name: "Edit freshness deadline" }))
    const dialog = await screen.findByRole("dialog")
    expect(within(dialog).getByRole("note")).toHaveTextContent(
      "Its schedule can leave 1d between rebuilds, so this deadline flags it Overdue between them even when every run succeeds.",
    )

    await user.click(within(dialog).getByRole("button", { name: "Use 1d 3h" }))
    expect(within(dialog).getByLabelText("Flag Overdue after")).toHaveValue(27)
    expect(within(dialog).getByRole("combobox", { name: "Unit" })).toHaveValue("hours")
    expect(within(dialog).queryByRole("note")).toBeNull()

    await user.click(within(dialog).getByRole("button", { name: "Save" }))
    await waitFor(() => expect(putBodies()).toEqual([{ deadline_seconds: 97200 }]))
    await within(row).findByText("Flag Overdue after 1d 3h")
    expect(within(row).queryByRole("note")).toBeNull()
  })

  it("only warns: a deadline under the gap still saves, and the row keeps the warning", async () => {
    gateway(null, { interval: 86400 })
    const user = userEvent.setup()
    const { row } = renderRow()
    await user.click(await within(row).findByRole("button", { name: "Set freshness deadline" }))
    const dialog = await screen.findByRole("dialog")
    await user.click(within(dialog).getByRole("button", { name: "12 hours" }))
    expect(within(dialog).getByRole("note")).toBeInTheDocument()

    const save = within(dialog).getByRole("button", { name: "Save" })
    expect(save).toBeEnabled()
    await user.click(save)
    await waitFor(() => expect(putBodies()).toEqual([{ deadline_seconds: 43200 }]))
    expect(await within(row).findByRole("note")).toHaveTextContent(LOAN_NOTE)
  })

  it("does not warn about the gap on a model the sweep does not check", async () => {
    gateway(21600, { interval: 86400 })
    const user = userEvent.setup()
    const { row } = renderRow({ materialization: "statement" })
    await user.click(await within(row).findByRole("button", { name: "Edit freshness deadline" }))
    const dialog = await screen.findByRole("dialog")
    const notes = within(dialog).getAllByRole("note")
    expect(notes).toHaveLength(1)
    expect(notes[0]).toHaveTextContent("does not rebuild a table")
  })

  it("warns without an offer when no deadline under the one-year cap fits the gap", async () => {
    // A yearly cron: 365 days between rebuilds.
    gateway(7 * 86400, { interval: 365 * 86400 })
    const user = userEvent.setup()
    const { row } = renderRow()
    await user.click(await within(row).findByRole("button", { name: "Edit freshness deadline" }))
    const dialog = await screen.findByRole("dialog")
    expect(within(dialog).getByRole("note")).toHaveTextContent("Its schedule can leave 365d between rebuilds")
    expect(within(dialog).queryByRole("button", { name: /^Use / })).toBeNull()
  })
})

describe("deadline arithmetic", () => {
  it("states a stored deadline in the largest exact unit", () => {
    expect(splitDeadline(21600)).toEqual({ amount: "6", unit: "hours" })
    expect(splitDeadline(172800)).toEqual({ amount: "2", unit: "days" })
    expect(splitDeadline(5400)).toEqual({ amount: "90", unit: "minutes" })
  })

  it("holds the gateway's range: 60 seconds to one year", () => {
    expect(deadlineFrom("1", "minutes")).toEqual({ seconds: 60 })
    expect(deadlineFrom("365", "days")).toEqual({ seconds: 31536000 })
    expect(deadlineFrom("0.5", "minutes")).toHaveProperty("error")
    expect(deadlineFrom("366", "days")).toHaveProperty("error")
    expect(deadlineFrom("", "hours")).toHaveProperty("error")
  })

  it("suggests the gap plus a tenth, in a unit the row prints exactly", () => {
    expect(suggestDeadline(86400)).toBe(27 * 3600) // daily → 1d 3h
    expect(suggestDeadline(47 * 3600)).toBe(52 * 3600) // a DST-skipped day → 2d 4h
    expect(suggestDeadline(3600)).toBe(75 * 60) // hourly → 1h 15m
    expect(suggestDeadline(60)).toBe(120) // every minute: at least a minute's margin
    expect(suggestDeadline(30 * 86400)).toBe(33 * 86400)
    expect(suggestDeadline(365 * 86400)).toBeNull()
  })
})
