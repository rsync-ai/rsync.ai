import { describe, expect, it } from "vitest"
import { dependencyStatusLabel, dependencyStreak } from "../dependencyStatus"

describe("dependencyStatusLabel", () => {
  it("reads 'Checking…' for a dependency the prober has not checked yet", () => {
    expect(dependencyStatusLabel({ status: "unknown" })).toBe("Checking…")
    expect(dependencyStatusLabel({ status: "unknown", last_checked_at: null })).toBe("Checking…")
  })

  it("keeps 'unknown' once a probe ran and could not decide", () => {
    expect(dependencyStatusLabel({ status: "unknown", last_checked_at: "2026-09-16T10:00:00Z" })).toBe("unknown")
  })

  // The prober skips a pipeline in these statuses (dependency_probe.go:102), so
  // a row it never reached stays unchecked: "Checking…" there never ends.
  it("reads 'Not checked' when the pipeline has finished, stopped or paused", () => {
    for (const s of ["completed", "failed", "cancelled", "stopped", "paused", "archived", "COMPLETED"]) {
      expect(dependencyStatusLabel({ status: "unknown" }, s)).toBe("Not checked")
    }
  })

  it("control: keeps 'Checking…' while the pipeline runs, or when its status is not known", () => {
    for (const s of ["running", "processing", "waiting_for_user", "idle", "", null, undefined]) {
      expect(dependencyStatusLabel({ status: "unknown" }, s)).toBe("Checking…")
    }
  })

  it("control: a finished pipeline does not relabel a row the prober did check", () => {
    expect(dependencyStatusLabel({ status: "unknown", last_checked_at: "2026-09-16T10:00:00Z" }, "completed")).toBe(
      "unknown",
    )
    expect(dependencyStatusLabel({ status: "healthy" }, "completed")).toBe("healthy")
  })

  // The gateway withdraws a paused or stopped pipeline's frozen "healthy" row
  // (pipeline_runtime.go markUnprobedDeps): it was checked once, before the
  // stop, and nothing has checked it since. It must not read like a probe that
  // ran and could not decide.
  it("reads 'Not checked' for a row the gateway marked as not checked while stopped or paused", () => {
    for (const s of ["stopped", "paused"]) {
      const dep = {
        status: "unknown",
        last_checked_at: "2026-10-22T10:00:00Z",
        details: { not_checked_while: s, last_result: "healthy" },
      }
      expect(dependencyStatusLabel(dep, s)).toBe("Not checked")
      expect(dependencyStatusLabel(dep)).toBe("Not checked")
    }
  })

  it("control: details without the marker do not relabel a checked row", () => {
    expect(
      dependencyStatusLabel({ status: "unknown", last_checked_at: "2026-10-22T10:00:00Z", details: { error: "x" } }, "stopped"),
    ).toBe("unknown")
  })

  it("passes checked statuses through", () => {
    expect(dependencyStatusLabel({ status: "healthy" })).toBe("healthy")
    expect(dependencyStatusLabel({ status: "unhealthy", last_checked_at: "2026-09-16T10:00:00Z" })).toBe("unhealthy")
  })
})

// consecutive_failures and last_healthy_at come from the dependency probe
// (dependency_probe.go writeHealth) and were served but never shown.
describe("dependencyStreak", () => {
  const age = (iso: string) => `<${iso}>`

  it("says nothing for a healthy dependency, whatever the counters hold", () => {
    expect(dependencyStreak({ status: "healthy", consecutive_failures: 0, last_healthy_at: "t1" }, age)).toBeNull()
  })

  it("names the failed-check streak and when it was last healthy", () => {
    expect(dependencyStreak({ status: "unhealthy", consecutive_failures: 12, last_healthy_at: "t1" }, age)).toBe(
      "not healthy for 12 checks in a row · last healthy <t1>",
    )
    expect(dependencyStreak({ status: "degraded", consecutive_failures: 1 }, age)).toBe(
      "not healthy for 1 check in a row",
    )
  })

  it("does not read a missing last_healthy_at as 'never healthy'", () => {
    expect(dependencyStreak({ status: "unhealthy", consecutive_failures: 3, last_healthy_at: null }, age)).not.toMatch(
      /never/,
    )
  })

  it("renders nothing rather than an empty clause when neither field is served", () => {
    expect(dependencyStreak({ status: "unknown" }, age)).toBeNull()
    expect(dependencyStreak({ status: "unhealthy", consecutive_failures: 0 }, age)).toBeNull()
  })
})
