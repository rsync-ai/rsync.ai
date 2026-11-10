/**
 * The card under an Ask rsync.ai answer for the data checks plan_model_checks drafted:
 * a refused call shows none, a later draft for a model replaces an earlier one, and
 * "Open on the Checks tab" hands the draft to that model's page.
 */

import type React from "react"
import { afterEach, describe, expect, it, vi } from "vitest"
import { cleanup, fireEvent, render, screen, within } from "@testing-library/react"
import "@testing-library/jest-dom"

import { AssistantThread, checksDrafts } from "@/components/assistant/AssistantThread"
import { takeAssistantDraft } from "@/components/explorer/modelChecks"
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

const M1 = "6f1c0a52-3c52-4a0e-9d7c-2d2f0f6a1b11"
const M2 = "0b0c3d1e-4f5a-4b6c-8d7e-9f0a1b2c3d4e"
const M3 = "9a8b7c6d-5e4f-4a3b-8c2d-1e0f9a8b7c6d"

function answer(looked_at: LookedAt[]): AssistantAnswer {
  return {
    conversation_id: "conv-1",
    answer: "I drafted two checks.",
    model: { entry: "claude", label: "Claude", model: "claude-sonnet-5-5" },
    looked_at,
    cost_cents: 1,
  }
}

const plan = (args: Record<string, unknown>, ok = true): LookedAt => ({ tool: "plan_model_checks", arguments: args, ok })

afterEach(() => {
  cleanup()
  sessionStorage.clear()
})

describe("checksDrafts", () => {
  it("takes a model's last accepted draft, and none from a refused call or another tool", () => {
    const got = checksDrafts(
      answer([
        plan({ model_id: M3, checks: [{ name: "refused", type: "not_null", columns: "id" }] }, false),
        plan({ model_id: M1, checks: [{ name: "first", type: "row_count" }] }),
        { tool: "list_model_checks", arguments: { model_id: M2 }, ok: true },
        plan({ model_id: M2, checks: [{ name: "other model", type: "row_count" }] }),
        plan({ model_id: M1, checks: [{ name: "second", type: "unique", columns: "id" }] }),
      ]),
    )
    expect(got.map((d) => [d.modelId, d.drafts.map((x) => x.name)])).toEqual([
      [M2, ["other model"]],
      [M1, ["second"]],
    ])
  })
})

describe("the card", () => {
  it("lists the drafted checks and opens them on the model's Checks tab", () => {
    render(
      <AssistantThread
        chat={{
          id: "c1",
          workspaceId: null,
          title: "checks",
          items: [
            {
              kind: "answer",
              answer: answer([
                plan({
                  model_id: M1,
                  checks: [
                    { name: "has rows", type: "count", min: "1" },
                    { name: "one row per order", type: "unique", columns: "order_id, region" },
                  ],
                }),
              ]),
            },
          ],
          updatedAt: Date.now(),
        }}
        wide={false}
        onRetry={() => {}}
      />,
    )
    const card = screen.getByRole("region", { name: "Drafted data checks" })
    expect(card).toHaveTextContent("2 data checks drafted")
    expect(card).toHaveTextContent("has rows · Row count")
    expect(card).toHaveTextContent("one row per order · Unique · order_id, region")
    expect(card).toHaveTextContent("Not saved")

    const open = within(card).getByRole("link", { name: "Open on the Checks tab" })
    expect(open).toHaveAttribute("href", `/models/${M1}?tab=checks`)
    expect(takeAssistantDraft(M1)).toBeNull()
    fireEvent.click(open)
    expect(takeAssistantDraft(M1)?.map((d) => d.name)).toEqual(["has rows", "one row per order"])
  })
})
