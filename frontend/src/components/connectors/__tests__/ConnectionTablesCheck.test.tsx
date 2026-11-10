/**
 * The tables an API connection could read when it was last checked
 * (ConnectionTablesCheck.tsx, generator-trust plan phase 3b). The gateway reads
 * one page of every table on save and on "Check tables", and stores the answer
 * on the connection (last_table_check). A failing table warns, never blocks.
 */

import { afterEach, beforeEach, describe, expect, it, vi } from "vitest"
import { cleanup, render, screen } from "@testing-library/react"
import userEvent from "@testing-library/user-event"
import "@testing-library/jest-dom"

vi.mock("@/lib/api/auth-fetch", () => ({ authFetch: vi.fn() }))
vi.mock("sonner", () => ({ toast: { success: vi.fn(), error: vi.fn(), info: vi.fn(), warning: vi.fn() } }))

import { toast } from "sonner"
import { authFetch } from "@/lib/api/auth-fetch"
import {
  ConnectionTablesCard,
  notifyTableCheck,
  tableCheckHasProblems,
  tableCheckSaveNotice,
  tableCheckSummary,
  type TableCheckRecord,
} from "@/components/connectors/ConnectionTablesCheck"

const minutesAgo = (m: number) => new Date(Date.now() - m * 60_000).toISOString()

const mixed: TableCheckRecord = {
  checked_at: minutesAgo(3),
  tables: [
    { name: "tasks", status: "ok" },
    { name: "lists", status: "ok" },
    { name: "team_task", status: "failed", error: "HTTP 400: team_id is not valid" },
    { name: "space_tags", status: "needs_setting", missing_settings: ["space_id"] },
    { name: "goals", status: "not_checked" },
  ],
  ok: 2,
  failed: 1,
  needs_setting: 1,
  not_checked: 1,
}

const allRead: TableCheckRecord = {
  checked_at: minutesAgo(1),
  tables: [
    { name: "tasks", status: "ok" },
    { name: "lists", status: "ok" },
  ],
  ok: 2,
  failed: 0,
  needs_setting: 0,
  not_checked: 0,
}

const unlisted: TableCheckRecord = {
  checked_at: minutesAgo(1),
  error: "connector image failed to build. End of the build log:\nNo module named 'acme'",
}

const jsonRes = (status: number, data: unknown) => ({ ok: status < 300, status, json: async () => data })

beforeEach(() => vi.clearAllMocks())
afterEach(() => cleanup())

describe("tableCheckSummary", () => {
  it("counts each outcome", () => {
    expect(tableCheckSummary(mixed)).toBe("2 of 5 tables readable · 1 failed · 1 needs a setting · 1 not checked")
    expect(tableCheckSummary(allRead)).toBe("2 of 2 tables readable")
    expect(tableCheckSummary({ ok: 0, needs_setting: 2, tables: [] })).toBe("The connector lists no tables")
    expect(tableCheckSummary({ ok: 1, needs_setting: 2 })).toBe("1 of 3 tables readable · 2 need a setting")
  })

  it("says when the tables could not be listed", () => {
    expect(tableCheckSummary(unlisted)).toBe("The tables could not be listed")
  })
})

describe("tableCheckHasProblems", () => {
  it("is true for a failed, unset or unchecked table, or a listing error", () => {
    expect(tableCheckHasProblems(mixed)).toBe(true)
    expect(tableCheckHasProblems(unlisted)).toBe(true)
    expect(tableCheckHasProblems({ ok: 3, not_checked: 1 })).toBe(true)
    expect(tableCheckHasProblems(allRead)).toBe(false)
  })
})

describe("tableCheckSaveNotice", () => {
  it("warns with the tables that could not be read", () => {
    expect(tableCheckSaveNotice({ last_table_check: mixed })).toEqual({
      level: "warning",
      title: "Saved — 2 of 5 tables readable · 1 failed · 1 needs a setting · 1 not checked",
      description: "Could not read: team_task. See Tables on the connection's page.",
    })
  })

  it("names three failed tables and counts the rest", () => {
    const tables = ["a", "b", "c", "d", "e"].map((name) => ({ name, status: "failed" }))
    const notice = tableCheckSaveNotice({ last_table_check: { tables, ok: 0, failed: 5 } })
    expect(notice?.description).toBe("Could not read: a, b, c and 2 more. See Tables on the connection's page.")
  })

  it("warns with the listing error", () => {
    expect(tableCheckSaveNotice({ last_table_check: unlisted })).toEqual({
      level: "warning",
      title: "Saved, but its tables could not be listed",
      description: unlisted.error,
    })
  })

  it("is a note while the check still runs", () => {
    expect(tableCheckSaveNotice({ table_check_pending: true })?.level).toBe("info")
  })

  it("is nothing when every table was read, or no check was run", () => {
    expect(tableCheckSaveNotice({ last_table_check: allRead })).toBeNull()
    expect(tableCheckSaveNotice({})).toBeNull()
    expect(tableCheckSaveNotice(undefined)).toBeNull()
  })

  it("notifyTableCheck shows it", () => {
    notifyTableCheck({ last_table_check: mixed })
    expect(toast.warning).toHaveBeenCalledTimes(1)
    notifyTableCheck({ table_check_pending: true })
    expect(toast.info).toHaveBeenCalledTimes(1)
    notifyTableCheck({ last_table_check: allRead })
    expect(toast.warning).toHaveBeenCalledTimes(1)
    expect(toast.info).toHaveBeenCalledTimes(1)
  })
})

describe("ConnectionTablesCard", () => {
  it("says when the connection was never checked", () => {
    render(<ConnectionTablesCard connectionId="c1" record={null} onChecked={vi.fn()} />)
    expect(screen.getByTestId("table-check-never")).toHaveTextContent("Not checked yet.")
    expect(screen.getByRole("button", { name: /check tables/i })).toBeEnabled()
  })

  it("lists each table that could not be read, and why", () => {
    render(<ConnectionTablesCard connectionId="c1" record={mixed} onChecked={vi.fn()} />)
    expect(screen.getByTestId("table-check-summary")).toHaveTextContent("2 of 5 tables readable")
    expect(screen.getByTestId("table-check-failed")).toHaveTextContent("team_task")
    expect(screen.getByTestId("table-check-failed")).toHaveTextContent("HTTP 400: team_id is not valid")
    expect(screen.getByTestId("table-check-needs_setting")).toHaveTextContent("Needs a setting: space_id")
    expect(screen.getByTestId("table-check-not_checked")).toHaveTextContent("ran out of time")
    expect(screen.getByText("tasks, lists")).toBeInTheDocument()
  })

  it("shows the listing error", () => {
    render(<ConnectionTablesCard connectionId="c1" record={unlisted} onChecked={vi.fn()} />)
    expect(screen.getByTestId("table-check-summary")).toHaveTextContent("The tables could not be listed")
    expect(screen.getByText(/No module named 'acme'/)).toBeInTheDocument()
  })

  it("Check tables stores a finished check", async () => {
    vi.mocked(authFetch).mockResolvedValueOnce(jsonRes(200, { last_table_check: allRead }) as unknown as Response)
    const onChecked = vi.fn()
    render(<ConnectionTablesCard connectionId="c1" record={mixed} onChecked={onChecked} />)
    await userEvent.click(screen.getByRole("button", { name: /check tables/i }))
    expect(authFetch).toHaveBeenCalledWith(expect.stringContaining("/api/v1/connections/c1/check-tables"), {
      method: "POST",
    })
    expect(onChecked).toHaveBeenCalledWith(allRead)
    expect(toast.success).toHaveBeenCalledWith("Every table was read", { description: "2 of 2 tables readable" })
  })

  it("Check tables stores a check with problems without a success toast", async () => {
    vi.mocked(authFetch).mockResolvedValueOnce(jsonRes(200, { last_table_check: mixed }) as unknown as Response)
    const onChecked = vi.fn()
    render(<ConnectionTablesCard connectionId="c1" record={null} onChecked={onChecked} />)
    await userEvent.click(screen.getByRole("button", { name: /check tables/i }))
    expect(onChecked).toHaveBeenCalledWith(mixed)
    expect(toast.success).not.toHaveBeenCalled()
  })

  it("Check tables says when the check is still running", async () => {
    vi.mocked(authFetch).mockResolvedValueOnce(jsonRes(202, { table_check_pending: true }) as unknown as Response)
    const onChecked = vi.fn()
    render(<ConnectionTablesCard connectionId="c1" record={null} onChecked={onChecked} />)
    await userEvent.click(screen.getByRole("button", { name: /check tables/i }))
    expect(onChecked).not.toHaveBeenCalled()
    expect(toast.info).toHaveBeenCalledWith("Still checking the tables", expect.anything())
  })

  it("Check tables shows a refusal", async () => {
    vi.mocked(authFetch).mockResolvedValueOnce(
      jsonRes(400, { error: "table_check_not_supported", message: "Only API connections have a table check." }) as unknown as Response,
    )
    const onChecked = vi.fn()
    render(<ConnectionTablesCard connectionId="c1" record={null} onChecked={onChecked} />)
    await userEvent.click(screen.getByRole("button", { name: /check tables/i }))
    expect(onChecked).not.toHaveBeenCalled()
    expect(toast.error).toHaveBeenCalledWith("Could not check the tables", {
      description: "Only API connections have a table check.",
    })
    expect(screen.getByRole("button", { name: /check tables/i })).toBeEnabled()
  })
})
