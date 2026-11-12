import { afterEach, describe, expect, it, vi } from "vitest"

import { exportDashboardPdf } from "@/lib/api/dashboards"
import { filenameFromDisposition } from "@/lib/export/download"
import { exportFilename } from "@/lib/export/filename"

// What an export downloads as (dashboards-design.md § 8, D3), and the browser's call
// for a dashboard's PDF: to this site's own /proxy/ route, never the gateway, with
// the CSRF header the route checks and the filters picked on the page.

const AT = new Date("2026-10-06T21:30:00Z")

describe("exportFilename", () => {
  it("keeps the name as typed, less what a file system refuses, with the day in UTC", () => {
    expect(exportFilename("Ops review: Q4 (EU)", "pdf", "dashboard", AT)).toEqual({
      ascii: "ops-review-q4-eu-2026-10-06.pdf",
      utf8: "Ops review Q4 (EU) 2026-10-06.pdf",
    })
    expect(exportFilename('a/b\\c*d?"e"<f>|g', "csv", "chart", AT).utf8).toBe('a b c d e f g 2026-10-06.csv')
    expect(exportFilename("Umsätze München", "png", "chart", AT)).toEqual({
      ascii: "umsatze-munchen-2026-10-06.png",
      utf8: "Umsätze München 2026-10-06.png",
    })
  })

  it("falls back when nothing of the name is left", () => {
    expect(exportFilename("日本", "svg", "chart", AT)).toEqual({ ascii: "chart-2026-10-06.svg", utf8: "日本 2026-10-06.svg" })
    expect(exportFilename("???", "pdf", "dashboard", AT)).toEqual({ ascii: "dashboard-2026-10-06.pdf", utf8: "dashboard 2026-10-06.pdf" })
  })
})

describe("filenameFromDisposition", () => {
  it("prefers the UTF-8 filename*, then the plain filename, else the fallback", () => {
    expect(filenameFromDisposition(`attachment; filename="ops.pdf"; filename*=UTF-8''Ops%20review%20%28EU%29.pdf`, "x.pdf")).toBe("Ops review (EU).pdf")
    expect(filenameFromDisposition('attachment; filename="ops.pdf"', "x.pdf")).toBe("ops.pdf")
    expect(filenameFromDisposition("attachment; filename=ops.pdf", "x.pdf")).toBe("ops.pdf")
    expect(filenameFromDisposition(`attachment; filename="ops.pdf"; filename*=UTF-8''%E0%A4`, "x.pdf")).toBe("ops.pdf")
    expect(filenameFromDisposition("attachment", "x.pdf")).toBe("x.pdf")
    expect(filenameFromDisposition(null, "x.pdf")).toBe("x.pdf")
  })
})

describe("exportDashboardPdf", () => {
  afterEach(() => {
    vi.unstubAllGlobals()
    document.cookie = "csrf_token=; expires=Thu, 01 Jan 1970 00:00:00 GMT; path=/"
  })

  it("posts the filters to this site's PDF route with the CSRF header, and names the file as the route does", async () => {
    document.cookie = "csrf_token=csrf-1; path=/"
    const fetch = vi.fn(
      async () =>
        new Response(new Uint8Array([37, 80, 68, 70]), {
          status: 200,
          headers: { "Content-Type": "application/pdf", "Content-Disposition": `attachment; filename="swipes-2026-10-06.pdf"; filename*=UTF-8''Swipes%202026-10-06.pdf` },
        }),
    )
    vi.stubGlobal("fetch", fetch)

    const got = await exportDashboardPdf("d 1", { f2: { values: ["EU"] } })

    expect(got.filename).toBe("Swipes 2026-10-06.pdf")
    expect(got.blob.size).toBe(4)
    const [url, init] = fetch.mock.calls[0] as unknown as [string, RequestInit]
    expect(url).toBe(`${window.location.origin}/proxy/dashboards/d%201/pdf`)
    expect(init.method).toBe("POST")
    expect((init.headers as Record<string, string>)["X-CSRF-Token"]).toBe("csrf-1")
    expect(JSON.parse(String(init.body))).toEqual({ filters: { f2: { values: ["EU"] } } })
  })

  it("says why the route refused", async () => {
    vi.stubGlobal("fetch", vi.fn(async () => Response.json({ error: "Missing or invalid CSRF token" }, { status: 403 })))
    await expect(exportDashboardPdf("d1", {})).rejects.toThrow("Missing or invalid CSRF token")
  })
})
