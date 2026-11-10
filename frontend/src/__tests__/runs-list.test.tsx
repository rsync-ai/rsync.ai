import { afterEach, beforeEach, describe, expect, it, vi } from "vitest"
import { act, fireEvent, render, screen, waitFor, within } from "@testing-library/react"
import userEvent from "@testing-library/user-event"
import type { Mock } from "vitest"

import { RUNS_PAGE_SIZE, RunsList, runDuration } from "@/components/runs/RunsList"
import { authFetch } from "@/lib/api/auth-fetch"
import { listActivity, type ActivityPage, type ActivityRun } from "@/lib/api/activity"
import { ACTIVE_WORKSPACE_EVENT, ACTIVE_WORKSPACE_KEY } from "@/lib/workspace/active-workspace"

// Runs: one list of pipeline, scheduled-query and workflow runs. These pin where
// each kind of run opens, that the filters reach both the request and the URL,
// that "Load more" appends without repeating a row, and that a page asked for
// under an older filter or workspace never lands on the current list.

const { replace } = vi.hoisted(() => ({ replace: vi.fn() }))
vi.mock("next/navigation", () => ({
  useRouter: () => ({ push: vi.fn(), replace, refresh: vi.fn() }),
  usePathname: () => "/runs",
}))
vi.mock("@/lib/api/auth-fetch", () => ({ authFetch: vi.fn() }))
vi.mock("@/lib/api/activity", async (importOriginal) => ({
  ...(await importOriginal<typeof import("@/lib/api/activity")>()),
  listActivity: vi.fn(),
}))

const list = listActivity as Mock

function run(over: Partial<ActivityRun>): ActivityRun {
  return {
    kind: "pipeline",
    id: "r",
    subject_id: "s",
    subject_name: "Subject",
    status: "succeeded",
    source_status: "completed",
    trigger: "manual",
    started_at: "2026-10-02T10:00:00Z",
    finished_at: "2026-10-02T10:04:12Z",
    ...over,
  }
}

/** A promise the test resolves when it chooses. */
function held<T>() {
  let resolve!: (v: T) => void
  const promise = new Promise<T>((r) => (resolve = r))
  return { promise, resolve }
}

/** Picks a status in the shared Select, the way a person does: open it, click the option. */
async function pickStatus(label: string) {
  const user = userEvent.setup()
  await user.click(screen.getByRole("combobox", { name: "Status" }))
  await user.click(await screen.findByRole("option", { name: label }))
}

const WS_A = "0df2f291-0000-0000-0000-000000000001"
const WS_B = "f71b4b51-0000-0000-0000-000000000002"

beforeEach(() => {
  vi.clearAllMocks()
  window.localStorage.setItem(ACTIVE_WORKSPACE_KEY, WS_A)
})

afterEach(() => {
  window.localStorage.clear()
})

describe("RunsList — rows", () => {
  it("opens each kind of run where its runs are shown", async () => {
    list.mockResolvedValue({
      runs: [
        run({ kind: "pipeline", id: "exec-1", subject_id: "pipe-1", subject_name: "Orders to warehouse", trigger: "cdc" }),
        run({
          kind: "scheduled_query",
          id: "sqr-1",
          subject_id: "query-1",
          subject_name: "Daily revenue",
          status: "failed",
          trigger: "triggered",
          error: "relation \"orders\" does not exist",
        }),
        run({
          kind: "workflow",
          id: "wr-1",
          subject_id: "wf-1",
          subject_name: "Weekly report",
          status: "running",
          trigger: "after_pipeline",
          finished_at: undefined,
        }),
        run({ kind: "pipeline", id: "exec-2", subject_name: "", trigger: "" }),
        // A model a workflow step rebuilt: stored as "triggered", sent as "workflow".
        run({ kind: "scheduled_query", id: "sqr-2", subject_id: "query-2", subject_name: "Transactions", trigger: "workflow" }),
      ],
    } satisfies ActivityPage)
    render(<RunsList />)

    const rows = (await screen.findAllByRole("row")).slice(1)
    const cells = rows.map((r) => within(r).getAllByRole("cell").map((c) => c.textContent))
    expect(cells.map((c) => [c[0], c[1], c[3], c[5]])).toEqual([
      ["Orders to warehouse", "Pipeline", "CDC", "4m 12s"],
      ['Daily revenuerelation "orders" does not exist', "Model", "After its upstream", "4m 12s"],
      ["Weekly report", "Workflow", "After a pipeline", "Running"],
      ["Unnamed", "Pipeline", "—", "4m 12s"],
      ["Transactions", "Model", "By a workflow", "4m 12s"],
    ])
    expect(screen.getByRole("link", { name: "Orders to warehouse" }).getAttribute("href")).toBe("/executions/exec-1")
    expect(screen.getByRole("link", { name: "Daily revenue" }).getAttribute("href")).toBe("/models/query-1")
    expect(screen.getByRole("link", { name: "Weekly report" }).getAttribute("href")).toBe("/workflows/wf-1")
    // One page: nothing more to load.
    expect(screen.queryByRole("button", { name: "Load more" })).toBeNull()
  })

  // A skip's reason is why it waited, not an error: only a failed run's reason is red.
  it("shows a skipped run's reason in grey and a failed run's in red", async () => {
    list.mockResolvedValue({
      runs: [
        run({ kind: "scheduled_query", id: "s1", subject_name: "Loans", status: "skipped", error: "waiting on 1 upstream" }),
        run({ kind: "scheduled_query", id: "s2", subject_name: "Cards", status: "failed", error: "relation does not exist" }),
      ],
    } satisfies ActivityPage)
    render(<RunsList />)
    expect((await screen.findByText("waiting on 1 upstream")).className).not.toMatch(/text-red/)
    expect(screen.getByText("relation does not exist").className).toMatch(/text-red-700/)
  })

  it("links a scheduled pipeline run's trigger to the schedule that started it", async () => {
    list.mockResolvedValue({
      runs: [
        run({ id: "exec-1", subject_id: "pipe 1", subject_name: "Orders", trigger: "scheduled", schedule_id: "sch/1" }),
        run({ id: "exec-2", subject_name: "Refunds", trigger: "manual" }),
        // Only a pipeline run carries a schedule id; one on any other kind is not a link.
        run({ kind: "scheduled_query", id: "sqr-1", subject_id: "q-1", subject_name: "Daily", trigger: "scheduled", schedule_id: "sch-x" }),
      ],
    } satisfies ActivityPage)
    render(<RunsList />)

    const links = await screen.findAllByRole("link", { name: "Schedule" })
    expect(links).toHaveLength(1)
    expect(links[0].getAttribute("href")).toBe("/pipelines/pipe%201?scheduleId=sch%2F1#schedules")
    expect(links[0].closest("tr")).toHaveTextContent("Orders")
  })

  it("says why the list is empty: nothing has run, or the filters match nothing", async () => {
    list.mockResolvedValue({ runs: [] })
    const { unmount } = render(<RunsList />)
    await screen.findByText("No runs yet")
    expect(screen.getByRole("link", { name: "Create pipeline" }).getAttribute("href")).toBe("/chat")
    unmount()

    render(<RunsList initialStatus="failed" />)
    await screen.findByText("No runs match these filters.")
    expect(screen.queryByRole("link", { name: "Create pipeline" })).toBeNull()
  })
})

describe("RunsList — filters", () => {
  it("names each kind filter in plain English", async () => {
    list.mockResolvedValue({ runs: [] })
    render(<RunsList />)
    const group = await screen.findByRole("group", { name: "Kind" })
    const names = Array.from(group.querySelectorAll("button")).map((b) => b.textContent)
    expect(names).toEqual(["All", "Pipelines", "Models", "Workflows"])
  })

  it("asks for the filters it was opened with, so /runs?status=failed lists failed runs", async () => {
    list.mockResolvedValue({ runs: [] })
    render(<RunsList initialKind="workflow" initialStatus="failed" />)
    await waitFor(() => expect(list).toHaveBeenCalledWith({ kind: "workflow", status: "failed", limit: RUNS_PAGE_SIZE }))
    expect(screen.getByRole("button", { name: "Workflows" }).getAttribute("aria-pressed")).toBe("true")
    expect(screen.getByRole("combobox", { name: "Status" })).toHaveTextContent("Failed")
  })

  it("re-reads and writes the URL on each change, and does nothing for the filter already set", async () => {
    list.mockResolvedValue({ runs: [] })
    render(<RunsList />)
    await waitFor(() => expect(list).toHaveBeenCalledTimes(1))

    fireEvent.click(screen.getByRole("button", { name: "Pipelines" }))
    await waitFor(() => expect(list).toHaveBeenLastCalledWith({ kind: "pipeline", status: undefined, limit: RUNS_PAGE_SIZE }))
    expect(replace).toHaveBeenLastCalledWith("/runs?kind=pipeline", { scroll: false })

    await pickStatus("Failed")
    await waitFor(() => expect(list).toHaveBeenLastCalledWith({ kind: "pipeline", status: "failed", limit: RUNS_PAGE_SIZE }))
    expect(replace).toHaveBeenLastCalledWith("/runs?kind=pipeline&status=failed", { scroll: false })

    fireEvent.click(screen.getByRole("button", { name: "All" }))
    await waitFor(() => expect(replace).toHaveBeenLastCalledWith("/runs?status=failed", { scroll: false }))

    await pickStatus("Any status")
    await waitFor(() => expect(replace).toHaveBeenLastCalledWith("/runs", { scroll: false }))

    const calls = list.mock.calls.length
    fireEvent.click(screen.getByRole("button", { name: "All" }))
    expect(list.mock.calls.length).toBe(calls)
    expect(replace).toHaveBeenCalledTimes(4)
  })
})

describe("RunsList — paging", () => {
  it("appends the next page from the cursor, without repeating a row, until there is no cursor", async () => {
    list
      .mockResolvedValueOnce({ runs: [run({ id: "a", subject_name: "A" }), run({ id: "b", subject_name: "B" })], next_cursor: "c1" })
      .mockResolvedValueOnce({
        // "b" again: a run that started between the two reads shifts the page.
        runs: [run({ id: "b", subject_name: "B" }), run({ id: "c", subject_name: "C" })],
      })
    render(<RunsList initialStatus="succeeded" />)
    await screen.findByText("A")

    fireEvent.click(screen.getByRole("button", { name: "Load more" }))
    await screen.findByText("C")
    expect(list).toHaveBeenLastCalledWith({ kind: undefined, status: "succeeded", limit: RUNS_PAGE_SIZE, cursor: "c1" })
    expect(screen.getAllByRole("link").map((a) => a.textContent)).toEqual(["A", "B", "C"])
    expect(screen.queryByRole("button", { name: "Load more" })).toBeNull()
  })

  it("drops a Load more page that a filter change overtook", async () => {
    const more = held<ActivityPage>()
    list
      .mockResolvedValueOnce({ runs: [run({ id: "old", subject_name: "Old page" })], next_cursor: "c1" })
      .mockReturnValueOnce(more.promise)
      .mockResolvedValueOnce({ runs: [run({ kind: "workflow", id: "w", subject_name: "Workflow run" })] })
    render(<RunsList />)
    await screen.findByText("Old page")

    fireEvent.click(screen.getByRole("button", { name: "Load more" }))
    fireEvent.click(screen.getByRole("button", { name: "Workflows" }))
    await screen.findByText("Workflow run")

    await act(async () => {
      more.resolve({ runs: [run({ id: "late", subject_name: "Late page" })] })
      await more.promise
    })
    expect(screen.queryByText("Late page")).toBeNull()
    expect(screen.getAllByRole("link").map((a) => a.textContent)).toEqual(["Workflow run"])
  })
})

describe("RunsList — workspace and errors", () => {
  it("re-reads on a workspace switch and drops the previous workspace's answer", async () => {
    const first = held<ActivityPage>()
    list.mockReturnValueOnce(first.promise).mockResolvedValueOnce({ runs: [run({ id: "b", subject_name: "B's run" })] })
    render(<RunsList />)

    await act(async () => {
      window.localStorage.setItem(ACTIVE_WORKSPACE_KEY, WS_B)
      window.dispatchEvent(new Event(ACTIVE_WORKSPACE_EVENT))
    })
    await screen.findByText("B's run")

    await act(async () => {
      first.resolve({ runs: [run({ id: "a", subject_name: "A's run" })] })
      await first.promise
    })
    expect(screen.queryByText("A's run")).toBeNull()
  })

  it("shows the server's error with a Retry that reads again", async () => {
    list.mockRejectedValueOnce(new Error("kind must be one of pipeline, scheduled_query, workflow")).mockResolvedValueOnce({
      runs: [run({ subject_name: "Back" })],
    })
    render(<RunsList />)
    const alert = await screen.findByRole("alert")
    expect(alert.textContent).toContain("kind must be one of pipeline, scheduled_query, workflow")

    fireEvent.click(within(alert).getByRole("button", { name: "Retry" }))
    await screen.findByText("Back")
    expect(screen.queryByRole("alert")).toBeNull()
  })
})

describe("runDuration", () => {
  it.each([
    [{ started_at: "2026-10-02T10:00:00Z", finished_at: "2026-10-02T10:00:00Z" }, "0ms"],
    [{ started_at: "2026-10-02T10:00:00Z", finished_at: "2026-10-02T10:00:59Z" }, "59s"],
    [{ started_at: "2026-10-02T10:00:00Z", finished_at: "2026-10-02T11:02:30Z" }, "1h 2m"],
    [{ started_at: "2026-10-02T10:00:00Z" }, ""],
    // A clock skew between services must not print a negative time.
    [{ started_at: "2026-10-02T10:00:05Z", finished_at: "2026-10-02T10:00:00Z" }, ""],
    [{ started_at: "not a date", finished_at: "2026-10-02T10:00:00Z" }, ""],
  ])("%j → %s", (r, want) => {
    expect(runDuration(r)).toBe(want)
  })
})

describe("listActivity", () => {
  const actual = () => vi.importActual<typeof import("@/lib/api/activity")>("@/lib/api/activity")
  const reply = (status: number, body: unknown) =>
    (authFetch as Mock).mockResolvedValueOnce({ ok: status < 400, status, json: async () => body })

  it("sends only the filters that are set, and returns no cursor on the last page", async () => {
    const { listActivity: real } = await actual()
    reply(200, { runs: [run({ schedule_id: "sch-1" })], next_cursor: "" })
    const page = await real({ kind: "workflow", limit: 50, cursor: "abc" })

    const url = String((authFetch as Mock).mock.calls[0][0])
    expect(url.replace(/^https?:\/\/[^/]+/, "")).toBe("/api/v1/activity?kind=workflow&limit=50&cursor=abc")
    expect(page.next_cursor).toBeUndefined()
    expect(page.runs).toHaveLength(1)
    expect(page.runs[0].schedule_id).toBe("sch-1")
  })

  it("throws the server's message, or says which status it got", async () => {
    const { listActivity: real, ActivityError } = await actual()
    reply(400, { error: "bad cursor" })
    await expect(real({ cursor: "x" })).rejects.toMatchObject({ message: "bad cursor", status: 400 })

    ;(authFetch as Mock).mockResolvedValueOnce({ ok: false, status: 502, json: async () => Promise.reject(new Error("html")) })
    const err = await real().catch((e: unknown) => e)
    expect(err).toBeInstanceOf(ActivityError)
    expect((err as Error).message).toBe("Could not load runs (HTTP 502).")
  })

  it("reads a body without a runs array as an empty page", async () => {
    const { listActivity: real } = await actual()
    reply(200, null)
    expect(await real()).toEqual({ runs: [], next_cursor: undefined })
  })
})
