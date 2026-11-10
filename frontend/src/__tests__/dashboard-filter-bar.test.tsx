import { beforeEach, describe, expect, it, vi } from "vitest"
import type { Mock } from "vitest"
import { fireEvent, render, screen, waitFor, within } from "@testing-library/react"

import { FilterBar } from "@/components/dashboards/FilterBar"
import { authFetch } from "@/lib/api/auth-fetch"
import type { DashboardFilter } from "@/lib/api/dashboards"
import { API_ENDPOINTS } from "@/lib/config/api"

// A dashboard's filters (dashboards-design.md § 4.2). The server binds each pick
// as a parameter and refuses a bad one; what is pinned here is what the bar
// sends: a date range only once a day is picked, a value list read live only
// when it opens and applied when it closes, and Escape keeping the last pick.

vi.mock("@/lib/api/auth-fetch", () => ({ authFetch: vi.fn() }))

const mockFetch = authFetch as unknown as Mock
const DASH = "dddddddd-0000-4000-8000-0000000000a1"

function json(status: number, body: unknown) {
  return { ok: status >= 200 && status < 300, status, json: () => Promise.resolve(body) } as Response
}

const filters: DashboardFilter[] = [
  { id: "day", kind: "date_range", label: "Day", column: "order_date" },
  { id: "region", kind: "values", label: "Region", column: "region" },
]

function serveValues(body: unknown = { values: ["APAC", "EU", "US"], truncated: false }) {
  mockFetch.mockImplementation((url: string) =>
    Promise.resolve(
      url === API_ENDPOINTS.DASHBOARDS.FILTER_VALUES(DASH, "region") ? json(200, body) : json(500, { error: url }),
    ),
  )
}

beforeEach(() => {
  mockFetch.mockReset()
})

describe("FilterBar", () => {
  it("draws nothing for a dashboard with no filters", () => {
    const { container } = render(<FilterBar dashboardId={DASH} filters={[]} picks={{}} onChange={vi.fn()} />)
    expect(container).toBeEmptyDOMElement()
  })

  it("reads no values until a list is opened", () => {
    render(<FilterBar dashboardId={DASH} filters={filters} picks={{}} onChange={vi.fn()} />)
    expect(screen.getByRole("group", { name: "Filters" })).toBeInTheDocument()
    expect(mockFetch).not.toHaveBeenCalled()
  })

  it("sends a date range as its days, and clears it once both are empty", () => {
    const onChange = vi.fn()
    const { rerender } = render(<FilterBar dashboardId={DASH} filters={filters} picks={{}} onChange={onChange} />)
    expect(screen.queryByLabelText("Day from")).toBeNull()
    fireEvent.click(screen.getByRole("button", { name: "Custom" }))
    fireEvent.change(screen.getByLabelText("Day from"), { target: { value: "2026-10-01" } })
    expect(onChange).toHaveBeenLastCalledWith("day", { from: "2026-10-01", to: "" })

    rerender(
      <FilterBar dashboardId={DASH} filters={filters} picks={{ day: { from: "2026-10-01" } }} onChange={onChange} />,
    )
    expect(screen.getByText("Pick both days to filter.")).toBeInTheDocument()

    rerender(
      <FilterBar
        dashboardId={DASH}
        filters={filters}
        picks={{ day: { from: "2026-10-05", to: "2026-10-01" } }}
        onChange={onChange}
      />,
    )
    expect(screen.getByText("The range ends before it starts.")).toBeInTheDocument()

    rerender(
      <FilterBar dashboardId={DASH} filters={filters} picks={{ day: { from: "2026-10-01", to: "" } }} onChange={onChange} />,
    )
    fireEvent.change(screen.getByLabelText("Day from"), { target: { value: "" } })
    expect(onChange).toHaveBeenLastCalledWith("day", null)
  })

  it("sets the last 7, 30 or 90 days, today included, from one chip, and All clears the range", () => {
    vi.useFakeTimers({ toFake: ["Date"] })
    vi.setSystemTime(new Date(2026, 9, 7, 15, 0))
    try {
      const onChange = vi.fn()
      const { rerender } = render(<FilterBar dashboardId={DASH} filters={filters} picks={{}} onChange={onChange} />)
      const chip = (name: string) => within(screen.getByRole("group", { name: "Day range" })).getByRole("button", { name })
      expect(chip("All")).toHaveAttribute("aria-pressed", "true")

      fireEvent.click(chip("7d"))
      expect(onChange).toHaveBeenLastCalledWith("day", { from: "2026-10-01", to: "2026-10-07" })
      fireEvent.click(chip("30d"))
      expect(onChange).toHaveBeenLastCalledWith("day", { from: "2026-09-08", to: "2026-10-07" })
      fireEvent.click(chip("90d"))
      expect(onChange).toHaveBeenLastCalledWith("day", { from: "2026-07-10", to: "2026-10-07" })

      // The chip pressed is read from the range itself, so a range set elsewhere shows as its chip.
      rerender(
        <FilterBar dashboardId={DASH} filters={filters} picks={{ day: { from: "2026-09-08", to: "2026-10-07" } }} onChange={onChange} />,
      )
      expect(chip("30d")).toHaveAttribute("aria-pressed", "true")
      expect(screen.queryByLabelText("Day from")).toBeNull()
      rerender(
        <FilterBar dashboardId={DASH} filters={filters} picks={{ day: { from: "2026-09-01", to: "2026-10-07" } }} onChange={onChange} />,
      )
      expect(chip("Custom")).toHaveAttribute("aria-pressed", "true")
      expect(screen.getByLabelText("Day from")).toHaveValue("2026-09-01")

      fireEvent.click(chip("All"))
      expect(onChange).toHaveBeenLastCalledWith("day", null)
    } finally {
      vi.useRealTimers()
    }
  })

  it("goes back to All when a custom range is cleared from outside, as Clear filters does", () => {
    const onChange = vi.fn()
    const { rerender } = render(<FilterBar dashboardId={DASH} filters={filters} picks={{}} onChange={onChange} />)
    const chip = (name: string) => within(screen.getByRole("group", { name: "Day range" })).getByRole("button", { name })
    fireEvent.click(chip("Custom"))
    rerender(
      <FilterBar dashboardId={DASH} filters={filters} picks={{ day: { from: "2026-09-01", to: "2026-09-03" } }} onChange={onChange} />,
    )
    expect(chip("Custom")).toHaveAttribute("aria-pressed", "true")

    rerender(<FilterBar dashboardId={DASH} filters={filters} picks={{}} onChange={onChange} />)
    expect(chip("All")).toHaveAttribute("aria-pressed", "true")
    expect(screen.queryByLabelText("Day from")).toBeNull()
  })

  it("reads a list's values when it opens and applies the ticked ones when it closes", async () => {
    serveValues()
    const onChange = vi.fn()
    render(<FilterBar dashboardId={DASH} filters={filters} picks={{}} onChange={onChange} />)
    fireEvent.click(screen.getByRole("button", { name: /Region/ }))
    const list = await screen.findByRole("dialog", { name: "Region values" })
    expect(mockFetch).toHaveBeenCalledTimes(1)
    expect(mockFetch.mock.calls[0][0]).toBe(API_ENDPOINTS.DASHBOARDS.FILTER_VALUES(DASH, "region"))

    fireEvent.click(await screen.findByRole("checkbox", { name: "EU" }))
    fireEvent.click(screen.getByRole("checkbox", { name: "US" }))
    // Nothing is sent while the list is open.
    expect(onChange).not.toHaveBeenCalled()
    fireEvent.click(screen.getByRole("button", { name: "Apply" }))
    expect(onChange).toHaveBeenCalledWith("region", { values: ["EU", "US"] })
    expect(list).not.toBeInTheDocument()
  })

  it("keeps the last pick when the list is closed with Escape", async () => {
    serveValues()
    const onChange = vi.fn()
    render(<FilterBar dashboardId={DASH} filters={filters} picks={{ region: { values: ["EU"] } }} onChange={onChange} />)
    // The button names what is picked.
    fireEvent.click(screen.getByRole("button", { name: "Region EU" }))
    fireEvent.click(await screen.findByRole("checkbox", { name: "US" }))
    fireEvent.keyDown(document, { key: "Escape" })
    await waitFor(() => expect(screen.queryByRole("dialog")).toBeNull())
    expect(onChange).not.toHaveBeenCalled()
  })

  it("takes a typed value for a column whose values are not listed, and says why the list is short", async () => {
    serveValues({ values: [], truncated: false, error: "This column is masked: type a value." })
    const onChange = vi.fn()
    render(<FilterBar dashboardId={DASH} filters={filters} picks={{}} onChange={onChange} />)
    fireEvent.click(screen.getByRole("button", { name: /Region/ }))
    expect(await screen.findByText("This column is masked: type a value.")).toBeInTheDocument()
    const typed = screen.getByLabelText("Type a Region value")
    fireEvent.change(typed, { target: { value: "  LATAM  " } })
    fireEvent.keyDown(typed, { key: "Enter" })
    expect(screen.getByRole("checkbox", { name: "LATAM" })).toBeChecked()
    fireEvent.click(screen.getByRole("button", { name: "Apply" }))
    expect(onChange).toHaveBeenCalledWith("region", { values: ["LATAM"] })
  })

  it("clears every filter at once", () => {
    const onChange = vi.fn()
    render(
      <FilterBar
        dashboardId={DASH}
        filters={filters}
        picks={{ day: { from: "2026-10-01", to: "2026-10-05" }, region: { values: ["EU"] } }}
        onChange={onChange}
      />,
    )
    fireEvent.click(screen.getByRole("button", { name: "Clear filters" }))
    expect(onChange).toHaveBeenCalledWith("day", null)
    expect(onChange).toHaveBeenCalledWith("region", null)
  })
})
