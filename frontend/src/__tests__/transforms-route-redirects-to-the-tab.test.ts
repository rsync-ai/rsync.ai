/**
 * /transforms was the Transform Builder. It is now a redirect, because a
 * pipeline's transforms are added and edited in that pipeline's Transforms tab;
 * old links (`/transforms?pipeline=<id>` was how the app linked to it) must
 * still land on the rules they meant.
 */
import { beforeEach, describe, expect, it, vi } from "vitest"

// vi.mock is hoisted above the imports, so what it closes over must be too.
const { redirect } = vi.hoisted(() => ({
  redirect: vi.fn((url: string) => {
    // next/navigation's redirect throws to stop rendering; so does this.
    throw new Error(`NEXT_REDIRECT ${url}`)
  }),
}))
vi.mock("next/navigation", () => ({ redirect }))

import TransformsRedirectPage from "@/app/(dashboard)/transforms/page"

async function landsOn(searchParams?: Record<string, string | string[] | undefined>) {
  await expect(
    TransformsRedirectPage({ searchParams: searchParams ? Promise.resolve(searchParams) : undefined })
  ).rejects.toThrow(/NEXT_REDIRECT/)
  expect(redirect).toHaveBeenCalledTimes(1)
  return redirect.mock.calls[0][0]
}

beforeEach(() => {
  redirect.mockClear()
})

describe("/transforms", () => {
  it("sends a pipeline link to that pipeline's Transforms tab", async () => {
    expect(await landsOn({ pipeline: "3b4f7eaf-0000-4000-8000-000000000001" })).toBe(
      "/pipelines/3b4f7eaf-0000-4000-8000-000000000001?tab=transforms"
    )
  })

  it("encodes the id, so a crafted one cannot change the path", async () => {
    expect(await landsOn({ pipeline: "../settings?x=1" })).toBe("/pipelines/..%2Fsettings%3Fx%3D1?tab=transforms")
  })

  it.each([undefined, {}, { pipeline: "  " }, { pipeline: ["a", "b"] }])(
    "sends %j to the pipeline list",
    async (sp) => {
      expect(await landsOn(sp)).toBe("/pipelines")
    }
  )
})
