import { createLucideIcon } from "lucide-react"

/**
 * The Workflows icon: a trigger, a query, and two delivery channels fanning out
 * from it, the shape the workflow canvas draws. lucide's own Workflow icon is two
 * boxes, which reads as one step to the next rather than a workflow of several.
 * Drawn on lucide's 24×24 grid so it takes the same size, stroke and colour props
 * as every other sidebar icon.
 */
export const WorkflowNodes = createLucideIcon("workflow-nodes", [
  ["rect", { x: "2", y: "9.5", width: "5", height: "5", rx: "1.5", key: "trigger" }],
  ["rect", { x: "9.5", y: "9.5", width: "5", height: "5", rx: "1.5", key: "query" }],
  ["rect", { x: "17", y: "3", width: "5", height: "5", rx: "1.5", key: "channel-a" }],
  ["rect", { x: "17", y: "16", width: "5", height: "5", rx: "1.5", key: "channel-b" }],
  ["path", { d: "M7 12h2.5", key: "edge-query" }],
  ["path", { d: "M14.5 12h1.25v-6.5H17", key: "edge-channel-a" }],
  ["path", { d: "M15.75 12v6.5H17", key: "edge-channel-b" }],
])
