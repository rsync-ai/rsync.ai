import { beforeEach, describe, expect, it, vi } from "vitest"

import {
  EXPLORER_OPEN_SQL_EVENT,
  MAX_QUERY_TABS,
  addTab,
  appendHistory,
  closeTab,
  initialTabsState,
  loadTabs,
  newTabName,
  openSqlInExplorer,
  queryTabsKey,
  readHistory,
  renameTab,
  saveTabs,
  takeExplorerSql,
  updateTab,
  type QueryTabsState,
} from "../queryTabs"

// dashboards-design.md § 7.5: up to 10 query tabs per person, each with its own
// connection and SQL, kept in the browser per user and workspace — a tab's name,
// connection and SQL, never its results.

beforeEach(() => {
  localStorage.clear()
  sessionStorage.clear()
})

function openTabs(n: number): QueryTabsState {
  let s = initialTabsState("c1")
  while (s.tabs.length < n) s = addTab(s)!
  return s
}

describe("query tabs", () => {
  it("opens up to 10 and refuses the 11th", () => {
    const ten = openTabs(MAX_QUERY_TABS)
    expect(MAX_QUERY_TABS).toBe(10)
    expect(ten.tabs).toHaveLength(10)
    expect(addTab(ten)).toBeNull()
    // Control: one fewer still opens.
    expect(addTab(openTabs(9))?.tabs).toHaveLength(10)
  })

  it("names a new tab with the lowest free number and makes it active", () => {
    const s = addTab(initialTabsState())!
    expect(s.tabs.map((t) => t.name)).toEqual(["Query 1", "Query 2"])
    expect(s.active).toBe(s.tabs[1].id)
    const gap = closeTab(addTab(s)!, s.tabs[0].id)
    expect(newTabName(gap.tabs)).toBe("Query 1")
  })

  it("moves focus left when the active tab closes, and never leaves zero tabs", () => {
    const s = openTabs(3)
    const closed = closeTab(s, s.tabs[2].id)
    expect(closed.active).toBe(s.tabs[1].id)
    const one = initialTabsState("c9")
    const after = closeTab(one, one.tabs[0].id)
    expect(after.tabs).toHaveLength(1)
    expect(after.tabs[0].id).not.toBe(one.tabs[0].id)
    expect(after.tabs[0]).toMatchObject({ connectionId: "c9", sql: "" })
  })

  it("keeps the same object when an update or rename changes nothing", () => {
    const s = initialTabsState("c1")
    const id = s.tabs[0].id
    expect(updateTab(s, id, { sql: "" })).toBe(s)
    expect(renameTab(s, id, "   ")).toBe(s)
    expect(renameTab(s, id, " Revenue ").tabs[0].name).toBe("Revenue")
  })
})

describe("query tab storage", () => {
  it("keys by user and workspace, and has no key until both are known", () => {
    expect(queryTabsKey("u1", "w1")).toBe("rsync_explorer_tabs_v1:u1:w1")
    expect(queryTabsKey("u1", "w2")).not.toBe(queryTabsKey("u1", "w1"))
    expect(queryTabsKey("u2", "w1")).not.toBe(queryTabsKey("u1", "w1"))
    expect(queryTabsKey(undefined, "w1")).toBeNull()
    expect(queryTabsKey("u1", "")).toBeNull()
  })

  it("stores name, connection and SQL, never a result", () => {
    const s = initialTabsState("c1")
    const tab = { ...s.tabs[0], sql: "select 1", result: { rows: [{ email: "a@b.c" }] } }
    saveTabs("k", { active: tab.id, tabs: [tab] })
    const raw = localStorage.getItem("k")!
    expect(raw).toContain("select 1")
    expect(raw).not.toContain("a@b.c")
    expect(Object.keys(JSON.parse(raw).tabs[0]).sort()).toEqual(["connectionId", "id", "name", "sql"])
    expect(loadTabs("k")).toEqual({ active: tab.id, tabs: [{ id: tab.id, name: "Query 1", connectionId: "c1", sql: "select 1" }] })
  })

  it("reads back nothing for a missing, broken or empty entry, and caps at 10", () => {
    expect(loadTabs("none")).toBeNull()
    localStorage.setItem("bad", "{not json")
    expect(loadTabs("bad")).toBeNull()
    localStorage.setItem("empty", JSON.stringify({ active: "x", tabs: [] }))
    expect(loadTabs("empty")).toBeNull()
    const many = Array.from({ length: 14 }, (_, i) => ({ id: `t${i}`, name: `Q${i}`, connectionId: "c", sql: "" }))
    localStorage.setItem("many", JSON.stringify({ active: "t13", tabs: many }))
    const loaded = loadTabs("many")!
    expect(loaded.tabs).toHaveLength(10)
    expect(loaded.active).toBe("t0")
  })
})

describe("Open in Explorer hand-off", () => {
  it("is taken once, and announces itself to an Explorer already on screen", () => {
    const heard = vi.fn()
    window.addEventListener(EXPLORER_OPEN_SQL_EVENT, heard)
    openSqlInExplorer({ sql: "select 2", connectionId: "c1", name: "Orders" })
    window.removeEventListener(EXPLORER_OPEN_SQL_EVENT, heard)
    expect(heard).toHaveBeenCalledTimes(1)
    expect(takeExplorerSql()).toEqual({ sql: "select 2", connectionId: "c1", name: "Orders" })
    expect(takeExplorerSql()).toBeNull()
  })

  it("drops a draft with no SQL", () => {
    openSqlInExplorer({ sql: "  " })
    expect(takeExplorerSql()).toBeNull()
  })
})

describe("History", () => {
  it("keeps the newest 20 runs per connection, in the key History has always used", () => {
    for (let i = 0; i < 25; i++) {
      appendHistory("c1", { id: `h${i}`, sql: `select ${i}`, timestamp: new Date(i * 1000), rowCount: i })
    }
    const list = readHistory("c1")
    expect(list).toHaveLength(20)
    expect(list[0]).toMatchObject({ id: "h24", sql: "select 24", rowCount: 24 })
    expect(list[0].timestamp).toBeInstanceOf(Date)
    expect(readHistory("c2")).toEqual([])
    expect(localStorage.getItem("rsync_explorer_history_v1:c1")).not.toBeNull()
  })
})
