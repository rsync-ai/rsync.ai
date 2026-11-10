import { useState } from "react"
import { describe, it, expect, vi } from "vitest"
import { render, screen, within } from "@testing-library/react"
import userEvent from "@testing-library/user-event"
import { UpstreamPicker, upstreamKey, type UpstreamOption } from "@/components/explorer/UpstreamPicker"

// The picker's own rules, apart from the dialog that feeds it. The ones pinned here are
// the ones whose failure is a wrong choice made quietly: two rows nobody can tell apart,
// a suggestion that looks like a selection, a cap that strands what is chosen, and a
// read-only viewer offered controls that do nothing.

const ORDERS_A: UpstreamOption = {
  kind: "pipeline",
  id: "11111111-aaaa-4000-8000-000000000000",
  name: "Orders load",
  detail: "Shop DB → Warehouse · CDC",
}
const ORDERS_B: UpstreamOption = {
  kind: "pipeline",
  id: "22222222-bbbb-4000-8000-000000000000",
  name: "orders load",
  detail: "Shop DB → Warehouse · CDC",
}
const DIM: UpstreamOption = {
  kind: "model",
  id: "33333333-cccc-4000-8000-000000000000",
  name: "Orders load",
  detail: "Builds analytics.orders_dim",
}

function renderPicker(props: Partial<React.ComponentProps<typeof UpstreamPicker>> = {}) {
  const onToggle = vi.fn()
  render(
    <>
      <span id="lbl">Rebuild after</span>
      <UpstreamPicker
        labelId="lbl"
        options={[ORDERS_A, ORDERS_B, DIM]}
        selected={[]}
        suggestedKeys={new Set()}
        readOnly={false}
        atCap={false}
        onToggle={onToggle}
        {...props}
      />
    </>
  )
  return { onToggle }
}

// The dialog owns `selected`; this holds it the same way, so a removal re-renders the
// chips the way it does in the app.
function StatefulPicker({ initial }: { initial: UpstreamOption[] }) {
  const [selected, setSelected] = useState(initial)
  return (
    <UpstreamPicker
      labelId="lbl"
      options={initial}
      selected={selected}
      suggestedKeys={new Set()}
      readOnly={false}
      atCap={false}
      onToggle={(o) =>
        setSelected((cur) =>
          cur.some((c) => upstreamKey(c) === upstreamKey(o)) ? cur.filter((c) => upstreamKey(c) !== upstreamKey(o)) : [...cur, o]
        )
      }
    />
  )
}

describe("UpstreamPicker", () => {
  it("starts open when nothing is chosen, without taking focus", () => {
    renderPicker()
    expect(screen.getByRole("group", { name: "Rebuild after" })).toBeInTheDocument()
    expect(screen.getByText("Nothing chosen yet.")).toBeInTheDocument()
    expect(screen.getByRole("button", { name: /choose a pipeline or model/i })).toHaveAttribute(
      "aria-expanded",
      "true"
    )
    // The user is still on the control before this one; the panel opening is not a
    // reason to move them.
    expect(screen.getByRole("searchbox")).not.toHaveFocus()
  })

  // Same name, same kind: the id is the only thing left that tells them apart. Counted
  // case-blind, and per kind — a model may share a pipeline's name, and its section
  // already says which it is.
  it("tells two pipelines with one name apart by their id", () => {
    renderPicker()
    const pipelines = screen.getByRole("group", { name: "Pipelines" })
    const rows = within(pipelines).getAllByRole("checkbox")
    expect(rows[0]).toHaveAccessibleDescription("Shop DB → Warehouse · CDC · #11111111")
    expect(rows[1]).toHaveAccessibleDescription("Shop DB → Warehouse · CDC · #22222222")
    const models = screen.getByRole("group", { name: "Models" })
    expect(within(models).getByRole("checkbox", { name: "Orders load" })).toHaveAccessibleDescription(
      "Builds analytics.orders_dim"
    )
  })

  it("marks a suggestion and lists it first, without choosing it", () => {
    const { onToggle } = renderPicker({ suggestedKeys: new Set([upstreamKey(ORDERS_B)]) })
    const rows = within(screen.getByRole("group", { name: "Pipelines" })).getAllByRole("checkbox")
    expect(rows[0]).toHaveAccessibleDescription(/^Suggested · /)
    expect(rows[0]).toHaveAccessibleName("orders load")
    expect(rows[0]).not.toBeChecked()
    expect(onToggle).not.toHaveBeenCalled()
  })

  it("chooses and removes through the same callback", async () => {
    const user = userEvent.setup()
    const { onToggle } = renderPicker({ selected: [DIM] })
    await user.click(screen.getByRole("button", { name: "Remove Orders load" }))
    expect(onToggle).toHaveBeenCalledWith(DIM)

    await user.click(screen.getByRole("button", { name: /add a pipeline or model/i }))
    expect(screen.getByRole("searchbox")).toHaveFocus()
    await user.click(within(screen.getByRole("group", { name: "Pipelines" })).getAllByRole("checkbox")[0])
    expect(onToggle).toHaveBeenLastCalledWith(ORDERS_A)
  })

  // A trigger can point at a producer that has since been deleted. It must still show,
  // and still be removable, or the only way to drop it is to delete the schedule.
  it("shows a chosen upstream no list returns, and lets it go", async () => {
    const gone: UpstreamOption = { kind: "pipeline", id: "dead0000-0000-4000-8000-000000000000", name: "Retired loader" }
    const user = userEvent.setup()
    const { onToggle } = renderPicker({ selected: [gone] })
    const chosen = screen.getByRole("list", { name: "Chosen" })
    expect(within(chosen).getByText("Retired loader")).toBeInTheDocument()
    await user.click(screen.getByRole("button", { name: "Remove Retired loader" }))
    expect(onToggle).toHaveBeenCalledWith(gone)
  })

  it("at the cap, blocks new rows and keeps chosen ones removable", async () => {
    const user = userEvent.setup()
    renderPicker({ selected: [ORDERS_A], atCap: true })
    await user.click(screen.getByRole("button", { name: /add a pipeline or model/i }))
    const rows = within(screen.getByRole("group", { name: "Pipelines" })).getAllByRole("checkbox")
    expect(rows[0]).toBeChecked()
    expect(rows[0]).toBeEnabled()
    expect(rows[1]).toBeDisabled()
    expect(screen.getByRole("button", { name: "Remove Orders load" })).toBeEnabled()
  })

  // Without a known connection, or with nothing on it, "this connection" would head an
  // empty list and hide everything else behind a toggle.
  it("does not group by a connection nothing loads", () => {
    renderPicker({ connectionId: "no-such-connection" })
    expect(screen.getByRole("group", { name: "Pipelines" })).toBeInTheDocument()
    expect(screen.queryByRole("group", { name: /this connection/i })).not.toBeInTheDocument()
    expect(screen.queryByRole("button", { name: /from other connections/i })).not.toBeInTheDocument()
  })

  it("gives a read-only viewer the chosen list and nothing to change it with", () => {
    renderPicker({ selected: [ORDERS_A, DIM], readOnly: true })
    const chosen = screen.getByRole("list", { name: "Chosen" })
    expect(within(chosen).getAllByRole("listitem")).toHaveLength(2)
    expect(screen.queryByRole("button")).not.toBeInTheDocument()
    expect(screen.queryByRole("checkbox")).not.toBeInTheDocument()
  })

  // A removed chip takes its button with it. Focus must land on a control that is
  // still there, not fall to the page and send a keyboard user back to the top.
  it("moves focus to the next chip when one is removed, and to the Add button after the last", async () => {
    const user = userEvent.setup()
    const a = { ...ORDERS_A, name: "First" }
    const b = { ...ORDERS_B, name: "Second" }
    const c = { ...DIM, name: "Third" }
    render(<StatefulPicker initial={[a, b, c]} />)

    await user.click(screen.getByRole("button", { name: "Remove Second" }))
    expect(screen.getByRole("button", { name: "Remove Third" })).toHaveFocus()
    // The last chip in the list has no next one: the one before it.
    await user.click(screen.getByRole("button", { name: "Remove Third" }))
    expect(screen.getByRole("button", { name: "Remove First" })).toHaveFocus()
    await user.keyboard("{Enter}")
    expect(screen.queryByRole("list", { name: "Chosen" })).not.toBeInTheDocument()
    expect(screen.getByRole("button", { name: /a pipeline or model/i })).toHaveFocus()
  })

  it("points aria-controls at the panel only while it is open", async () => {
    const user = userEvent.setup()
    renderPicker({ selected: [ORDERS_A] })
    const toggle = screen.getByRole("button", { name: /add a pipeline or model/i })
    expect(toggle).not.toHaveAttribute("aria-controls")
    await user.click(toggle)
    const panel = document.getElementById(toggle.getAttribute("aria-controls") ?? "")
    expect(panel).not.toBeNull()
    expect(panel).toContainElement(screen.getByRole("searchbox"))
  })

  it("closes on Done and gives focus back to the button that opened it", async () => {
    const user = userEvent.setup()
    renderPicker({ selected: [ORDERS_A] })
    const toggle = screen.getByRole("button", { name: /add a pipeline or model/i })
    expect(toggle).toHaveAttribute("aria-expanded", "false")
    await user.click(toggle)
    await user.type(screen.getByRole("searchbox"), "dim")
    await user.click(screen.getByRole("button", { name: "Done" }))
    expect(screen.queryByRole("searchbox")).not.toBeInTheDocument()
    expect(toggle).toHaveFocus()
    // The search is not remembered: the next open starts from the whole list.
    await user.click(toggle)
    expect(screen.getByRole("searchbox")).toHaveValue("")
  })

  // The dialog closes on any Escape that reaches the document unhandled (ui/dialog.tsx),
  // so an open panel has to claim it from wherever focus is in the picker, the Add
  // button included: otherwise one key throws away the whole unsaved model.
  describe("Escape", () => {
    // Stands in for the dialog: what it would see at the document.
    function dialogSees() {
      const seen: boolean[] = []
      const listener = (e: KeyboardEvent) => {
        if (e.key === "Escape") seen.push(e.defaultPrevented)
      }
      document.addEventListener("keydown", listener)
      return { seen, stop: () => document.removeEventListener("keydown", listener) }
    }

    it("closes the panel, not the dialog, from the search box", async () => {
      const user = userEvent.setup()
      renderPicker({ selected: [ORDERS_A] })
      const toggle = screen.getByRole("button", { name: /add a pipeline or model/i })
      await user.click(toggle)
      const dialog = dialogSees()
      await user.keyboard("{Escape}")
      dialog.stop()
      expect(screen.queryByRole("searchbox")).not.toBeInTheDocument()
      expect(toggle).toHaveFocus()
      expect(dialog.seen).toEqual([true])
    })

    it("closes the panel, not the dialog, from the Add button while the panel is open", async () => {
      const user = userEvent.setup()
      // Nothing chosen: the panel starts open and focus has not moved into it.
      renderPicker()
      const toggle = screen.getByRole("button", { name: /choose a pipeline or model/i })
      toggle.focus()
      const dialog = dialogSees()
      await user.keyboard("{Escape}")
      dialog.stop()
      expect(screen.queryByRole("searchbox")).not.toBeInTheDocument()
      expect(toggle).toHaveAttribute("aria-expanded", "false")
      expect(toggle).toHaveFocus()
      expect(dialog.seen).toEqual([true])
    })

    it("leaves Escape to the dialog once the panel is closed", async () => {
      const user = userEvent.setup()
      renderPicker({ selected: [ORDERS_A] })
      screen.getByRole("button", { name: "Remove Orders load" }).focus()
      const dialog = dialogSees()
      await user.keyboard("{Escape}")
      dialog.stop()
      expect(dialog.seen).toEqual([false])
    })
  })
})
