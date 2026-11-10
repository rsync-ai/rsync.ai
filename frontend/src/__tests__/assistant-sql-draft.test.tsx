/**
 * The card under an Ask rsync.ai answer for the SQL generate_sql wrote
 * (dashboards-design § 7.5): a refused call shows none, the same SQL shows
 * once, Copy copies the SQL, and Open in Explorer hands it to a new query tab.
 */

import type React from "react"
import { afterEach, describe, expect, it, vi } from "vitest"
import { cleanup, fireEvent, render, screen, within } from "@testing-library/react"
import "@testing-library/jest-dom"

import { AssistantThread, lookedAtLabel, sqlDrafts } from "@/components/assistant/AssistantThread"
import { EXPLORER_OPEN_SQL_EVENT, takeExplorerSql } from "@/lib/explorer/queryTabs"
import type { AssistantAnswer, LookedAt } from "@/lib/api/assistant"

vi.mock("next/link", () => ({
  default: ({ href, children, ...rest }: { href: string; children: React.ReactNode }) => (
    <a
      href={href}
      {...rest}
      onClick={(e) => {
        e.preventDefault()
        ;(rest as { onClick?: (e: unknown) => void }).onClick?.(e)
      }}
    >
      {children}
    </a>
  ),
}))

const CONN = "6f1c0a52-3c52-4a0e-9d7c-2d2f0f6a1b11"
const SQL = "SELECT status, COUNT(*) AS orders\nFROM public.orders\nGROUP BY status"

function answer(looked_at: LookedAt[]): AssistantAnswer {
  return {
    conversation_id: "conv-1",
    answer: "Here is a query that counts orders by status.",
    model: { entry: "claude", label: "Claude", model: "claude-sonnet-5-5" },
    looked_at,
    cost_cents: 1,
  }
}

const written = (sql: string, ok = true): LookedAt => ({
  tool: "generate_sql",
  arguments: { connection_id: CONN, question: "orders by status" },
  ok,
  sql: ok ? { sql, connection_id: CONN, tables: ["public.orders"] } : undefined,
})

function renderAnswer(a: AssistantAnswer) {
  render(
    <AssistantThread
      chat={{ id: "c1", workspaceId: null, title: "sql", items: [{ kind: "answer", answer: a }], updatedAt: Date.now() }}
      wide={false}
      onRetry={() => {}}
    />,
  )
}

afterEach(() => {
  cleanup()
  sessionStorage.clear()
  vi.restoreAllMocks()
})

describe("sqlDrafts", () => {
  it("takes each SQL once, and none from a refused call or another tool", () => {
    const got = sqlDrafts(
      answer([
        written("", false),
        { ...written("SELECT 2"), ok: false },
        { tool: "describe_schema", arguments: { connection_id: CONN }, ok: true },
        written(SQL),
        written(SQL),
        { ...written("SELECT 1"), sql: { sql: "   ", connection_id: CONN, tables: [] } },
      ]),
    )
    expect(got).toEqual([{ sql: SQL, connection_id: CONN, tables: ["public.orders"] }])
  })

  it("says in Looked at whether the SQL was written", () => {
    expect(lookedAtLabel(written(SQL))).toBe("a SQL draft")
    expect(lookedAtLabel(written("", false))).toBe("a SQL draft (not written)")
    expect(lookedAtLabel({ ...written(SQL), ok: false })).toBe("a SQL draft (not written)")
  })
})

describe("the card", () => {
  it("shows the SQL as not run, with the tables it reads", () => {
    renderAnswer(answer([written(SQL)]))
    const card = screen.getByRole("region", { name: "Written SQL" })
    expect(card).toHaveTextContent("Not run")
    expect(card).toHaveTextContent("From public.orders")
    expect(card.querySelector("pre")?.textContent).toBe(SQL)
  })

  it("shows no card when the call was refused", () => {
    renderAnswer(answer([written("", false)]))
    expect(screen.queryByRole("region", { name: "Written SQL" })).toBeNull()
    expect(screen.getByText(/Looked at a SQL draft \(not written\)/)).toBeInTheDocument()
  })

  it("copies the SQL", async () => {
    const writeText = vi.fn().mockResolvedValue(undefined)
    Object.defineProperty(navigator, "clipboard", { value: { writeText }, configurable: true })
    renderAnswer(answer([written(SQL)]))
    fireEvent.click(screen.getByRole("button", { name: "Copy SQL" }))
    expect(writeText).toHaveBeenCalledWith(SQL)
    expect(await screen.findByRole("button", { name: "Copied" })).toBeInTheDocument()
  })

  it("opens the SQL in a new Explorer tab on its connection", () => {
    const opened = vi.fn()
    window.addEventListener(EXPLORER_OPEN_SQL_EVENT, opened)
    renderAnswer(answer([written(SQL)]))
    const open = within(screen.getByRole("region", { name: "Written SQL" })).getByRole("link", {
      name: "Open in Explorer",
    })
    expect(open).toHaveAttribute("href", "/explorer")
    expect(takeExplorerSql()).toBeNull()
    fireEvent.click(open)
    window.removeEventListener(EXPLORER_OPEN_SQL_EVENT, opened)
    expect(opened).toHaveBeenCalledTimes(1)
    expect(takeExplorerSql()).toEqual({ sql: SQL, connectionId: CONN, name: undefined })
  })
})
