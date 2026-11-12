/**
 * Staggered entrances stop growing after the sixth item (launch review, kit).
 * Connectors staggered every card by 0.05s, so the 60th card of a long category
 * waited three seconds to appear.
 */
import { describe, expect, it } from "vitest"
import { MAX_STAGGERED, staggerDelay } from "@/components/kit/motion"

describe("staggerDelay", () => {
  it("steps up to the sixth item, then holds", () => {
    const want = [0, 0.05, 0.25, 0.3, 0.3, 0.3]
    ;[0, 1, 5, 6, 7, 60].forEach((i, k) => expect(staggerDelay(i)).toBeCloseTo(want[k]))
    expect(staggerDelay(100)).toBe(MAX_STAGGERED * 0.05)
  })

  it("takes a step and never goes negative", () => {
    expect(staggerDelay(3, 0.1)).toBeCloseTo(0.3)
    expect(staggerDelay(-1)).toBe(0)
  })
})
