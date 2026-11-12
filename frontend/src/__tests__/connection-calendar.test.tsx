import { afterEach, describe, expect, it, vi } from "vitest"
import type { Mock } from "vitest"
import { cleanup, render, screen, waitFor } from "@testing-library/react"
import userEvent from "@testing-library/user-event"
import "@testing-library/jest-dom"
import { toast } from "sonner"

import { ConnectionCalendar } from "@/components/connectors/ConnectionCalendar"
import { authFetch } from "@/lib/api/auth-fetch"
import { API_ENDPOINTS } from "@/lib/config/api"

// A connection's calendar for charts (dashboards-design.md § 14.3, decision 14): the day its
// weeks start and the month its fiscal year starts, ISO Monday and January until changed.
// Each pick saves on its own, sending the gateway the one field it changed, which
// UpdateConnection takes without a connection test (connection_calendar_test.go).

vi.mock("@/lib/api/auth-fetch", () => ({ authFetch: vi.fn() }))
vi.mock("sonner", () => ({ toast: { success: vi.fn(), error: vi.fn() } }))

const mockFetch = authFetch as unknown as Mock
const answer = (status: number, body: unknown) =>
  ({ ok: status < 400, status, json: async () => body }) as unknown as Response

afterEach(() => {
  cleanup()
  vi.clearAllMocks()
})

describe("a connection's calendar", () => {
  it("reads Monday and January from a gateway that sends neither", () => {
    render(<ConnectionCalendar connectionId="conn-1" value={{}} onSaved={() => {}} />)
    expect(screen.getByLabelText("Weeks start on")).toHaveDisplayValue("Monday")
    expect(screen.getByLabelText("Fiscal year starts in")).toHaveDisplayValue("January")
  })

  it("shows what the connection holds", () => {
    render(<ConnectionCalendar connectionId="conn-1" value={{ week_start: 7, fiscal_year_start_month: 4 }} onSaved={() => {}} />)
    expect(screen.getByLabelText("Weeks start on")).toHaveDisplayValue("Sunday")
    expect(screen.getByLabelText("Fiscal year starts in")).toHaveDisplayValue("April")
  })

  it("saves the week start alone, then hands back the whole calendar", async () => {
    mockFetch.mockResolvedValue(answer(200, { id: "conn-1" }))
    const onSaved = vi.fn()
    render(<ConnectionCalendar connectionId="conn-1" value={{ fiscal_year_start_month: 4 }} onSaved={onSaved} />)

    await userEvent.selectOptions(screen.getByLabelText("Weeks start on"), "Sunday")

    await waitFor(() => expect(onSaved).toHaveBeenCalledWith({ week_start: 7, fiscal_year_start_month: 4 }))
    expect(mockFetch).toHaveBeenCalledTimes(1)
    const [url, init] = mockFetch.mock.calls[0]
    expect(url).toBe(API_ENDPOINTS.CONNECTIONS.UPDATE("conn-1"))
    expect(init.method).toBe("PUT")
    expect(JSON.parse(init.body)).toEqual({ week_start: 7 })
    expect(toast.success).toHaveBeenCalledWith("Calendar saved", expect.anything())
  })

  it("saves the fiscal year start alone", async () => {
    mockFetch.mockResolvedValue(answer(200, { id: "conn-1" }))
    const onSaved = vi.fn()
    render(<ConnectionCalendar connectionId="conn-1" value={{}} onSaved={onSaved} />)

    await userEvent.selectOptions(screen.getByLabelText("Fiscal year starts in"), "October")

    await waitFor(() => expect(onSaved).toHaveBeenCalledWith({ week_start: 1, fiscal_year_start_month: 10 }))
    expect(JSON.parse(mockFetch.mock.calls[0][1].body)).toEqual({ fiscal_year_start_month: 10 })
  })

  it("says the gateway's reason when it refuses, and keeps what was saved", async () => {
    mockFetch.mockResolvedValue(answer(403, { error: "insufficient workspace role" }))
    const onSaved = vi.fn()
    render(<ConnectionCalendar connectionId="conn-1" value={{}} onSaved={onSaved} />)

    await userEvent.selectOptions(screen.getByLabelText("Weeks start on"), "Sunday")

    await waitFor(() =>
      expect(toast.error).toHaveBeenCalledWith("The calendar was not saved", {
        description: "insufficient workspace role",
      }),
    )
    expect(onSaved).not.toHaveBeenCalled()
    expect(screen.getByLabelText("Weeks start on")).toHaveDisplayValue("Monday")
    expect(screen.getByLabelText("Weeks start on")).toBeEnabled()
  })

  it("says so when the request never reaches the gateway", async () => {
    mockFetch.mockRejectedValue(new Error("Failed to fetch"))
    const onSaved = vi.fn()
    render(<ConnectionCalendar connectionId="conn-1" value={{}} onSaved={onSaved} />)

    await userEvent.selectOptions(screen.getByLabelText("Fiscal year starts in"), "July")

    await waitFor(() =>
      expect(toast.error).toHaveBeenCalledWith("The calendar was not saved", { description: "Failed to fetch" }),
    )
    expect(onSaved).not.toHaveBeenCalled()
  })
})
