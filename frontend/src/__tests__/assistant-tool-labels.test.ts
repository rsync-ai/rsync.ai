import { readFileSync } from "node:fs"
import { resolve } from "node:path"
import { describe, expect, it } from "vitest"
import { TOOL_LABELS, WELCOME } from "@/components/assistant/AssistantThread"

// The tool reference is generated from the agent gateway's registry
// (TestReferenceDocIsCurrent keeps it current), so a tool added there without
// a label here fails this test instead of showing "Looked at list_runs".
describe("assistant tool labels", () => {
  it("label every tool the agent gateway has", () => {
    const doc = readFileSync(resolve(process.cwd(), "../docs/api/agent-gateway.md"), "utf8")
    const tools = Array.from(doc.matchAll(/^### `([a-z_]+)`/gm), (m) => m[1])
    expect(tools.length).toBeGreaterThan(5)
    expect(tools.filter((tool) => !TOOL_LABELS[tool])).toEqual([])
  })

  // The welcome said "changes nothing" after generate_connector shipped. Any
  // tool the reference marks **Writes.** makes a read-only claim false.
  it("does not call itself read-only while a tool writes", () => {
    const doc = readFileSync(resolve(process.cwd(), "../docs/api/agent-gateway.md"), "utf8")
    const writers = doc.split(/^### /m).filter((section) => section.includes("**Writes.**"))
    expect(writers.length).toBeGreaterThan(0)
    expect(WELCOME).not.toMatch(/changes nothing|read[- ]only|only reads|never changes/i)
  })

  // Each kind of write the gateway offers is something an admin can only find
  // out about from the welcome, so it names both.
  it("names each kind of write a tool offers", () => {
    const doc = readFileSync(resolve(process.cwd(), "../docs/api/agent-gateway.md"), "utf8")
    const writers = doc
      .split(/^### /m)
      .filter((section) => section.includes("**Writes.**"))
      .map((section) => section.slice(1, section.indexOf("`", 1)))
    expect(writers).toEqual(
      expect.arrayContaining([
        "generate_connector",
        "create_model",
        "schedule_model",
        "plan_pipeline",
        "plan_workflow",
        "plan_workflow_change",
        "plan_dashboard",
        "plan_dashboard_change",
      ]),
    )
    expect(WELCOME).toMatch(/propose a pipeline/i)
    expect(WELCOME).toMatch(/generate a connector/i)
    expect(WELCOME).toMatch(/build, schedule and run silver and gold models/i)
    expect(WELCOME).toMatch(/draft a workflow/i)
    expect(WELCOME).toMatch(/propose a change to a workflow/i)
    expect(WELCOME).toMatch(/draft a dashboard/i)
    expect(WELCOME).toMatch(/propose a change to one tile of a dashboard/i)
  })

  // plan_dashboard saves nothing either: a person saves its draft (dashboards-design.md § 7.3).
  it("says a person saves a dashboard draft", () => {
    expect(WELCOME).toMatch(/draft a dashboard of charts on your models and saved queries, which you preview and save yourself/i)
  })

  // plan_dashboard_change changes nothing until an owner or admin applies it
  // (dashboards-design.md § 7.6).
  it("says a person applies a dashboard change", () => {
    expect(WELCOME).toMatch(/propose a change to one tile of a dashboard, which you apply yourself/i)
  })

  // plan_model_checks saves nothing, so the welcome says who saves its draft.
  it("says a check draft is saved on the Checks tab", () => {
    expect(WELCOME).toMatch(/draft data checks for a model, which you save on the model's Checks tab/i)
  })
})
