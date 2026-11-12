import { describe, expect, it } from "vitest"
import { render, screen } from "@testing-library/react"

import { Badge } from "@/components/ui/badge"

// A badge is a one-line pill. Left to wrap, a label with a space ("Batch
// (manual)", "Backfill+CDC" beside it) broke into two lines in the narrow Type
// column on /pipelines. jsdom lays nothing out, so the class is what can be
// checked here.
describe("Badge", () => {
  it("keeps its label on one line by default", () => {
    render(<Badge variant="outline">Batch (manual)</Badge>)
    expect(screen.getByText("Batch (manual)")).toHaveClass("whitespace-nowrap")
  })

  it("lets a caller opt back into wrapping", () => {
    render(<Badge className="whitespace-normal">a long reason that may wrap</Badge>)
    const badge = screen.getByText("a long reason that may wrap")
    expect(badge).toHaveClass("whitespace-normal")
    expect(badge).not.toHaveClass("whitespace-nowrap")
  })
})
