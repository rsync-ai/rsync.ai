import { afterEach, beforeAll, beforeEach, describe, expect, it, vi } from "vitest"
import type { Mock } from "vitest"
import { act, cleanup, render, screen, waitFor } from "@testing-library/react"

import SettingsPage from "@/app/(dashboard)/settings/page"
import { authFetch } from "@/lib/api/auth-fetch"
import { API_ENDPOINTS } from "@/lib/config/api"

// ---------------------------------------------------------------------------
// Launch review, PR5: your account settings.
//
// The Account tab was a max-w-5xl column that left a third of the page blank,
// with a ragged two-column form, and the avatar's violet glowed in dark mode.
// These render the real page and read what a user sees: full width, the same
// label-left, control-right rows as workspace settings, each password field
// named once by its row, and an avatar with a dark pair.
// ---------------------------------------------------------------------------

vi.mock("@/lib/api/auth-fetch", () => ({ authFetch: vi.fn() }))
vi.mock("sonner", () => ({ toast: { success: vi.fn(), error: vi.fn() } }))
const router = vi.hoisted(() => ({ push: vi.fn(), replace: vi.fn(), refresh: vi.fn() }))
vi.mock("next/navigation", () => ({
  useRouter: () => router,
  useSearchParams: () => new URLSearchParams(),
  usePathname: () => "/settings",
}))
vi.mock("next-themes", () => ({ useTheme: () => ({ theme: "light", resolvedTheme: "light", setTheme: vi.fn() }) }))

const mockFetch = authFetch as unknown as Mock

function res(body: unknown, status = 200) {
  return { ok: status < 400, status, json: async () => body } as unknown as Response
}

async function open() {
  mockFetch.mockImplementation(async (url: string) =>
    url === API_ENDPOINTS.AUTH.ME ? res({ name: "Your Name", email: "you@company.com", role: "member" }) : res({}),
  )
  await act(async () => {
    render(<SettingsPage />)
  })
  await waitFor(() => expect((screen.getByLabelText("Full name") as HTMLInputElement).value).toBe("Your Name"))
}

const classes = (el: Element) => el.className.split(/\s+/)

/** The row a control sits in, and its left (label) and right (control) halves. */
function row(control: HTMLElement) {
  const grid = control.closest(".grid") as HTMLElement
  const [left, right] = Array.from(grid.children)
  return { grid, left, right }
}

beforeAll(() => {
  window.ResizeObserver ||= class {
    observe() {}
    unobserve() {}
    disconnect() {}
  }
})
beforeEach(() => mockFetch.mockReset())
afterEach(() => cleanup())

describe("account settings — layout", () => {
  it("uses the full width, the profile card above the password card", async () => {
    await open()
    expect(document.querySelector(".max-w-5xl")).toBeNull()
    const profile = screen.getByText("Your personal information").closest(".rounded-xl") as HTMLElement
    const password = screen.getByText("Update your account password").closest(".rounded-xl") as HTMLElement
    // Siblings in the tab, not halves of a two-column grid.
    expect(profile.parentElement).toBe(password.parentElement)
    expect(classes(profile.parentElement as HTMLElement)).not.toContain("@3xl:grid-cols-2")
    expect(profile.compareDocumentPosition(password) & Node.DOCUMENT_POSITION_FOLLOWING).toBeTruthy()
  })

  it("the profile fields are label-left, control-right rows", async () => {
    await open()
    const name = screen.getByLabelText("Full name")
    const { grid, left, right } = row(name)
    expect(classes(grid)).toContain("@2xl:grid-cols-[minmax(0,15rem)_minmax(0,1fr)]")
    expect(left.textContent).toBe("Full name")
    expect(right.contains(name)).toBe(true)

    const email = screen.getByLabelText("Email")
    const hint = screen.getByText("You sign in with it. It can't be changed here.")
    expect(row(email).left.contains(hint)).toBe(true)
    expect(email.getAttribute("aria-describedby")).toBe(hint.id)
  })

  it("each password field is a row, named once by its row's label", async () => {
    await open()
    for (const label of ["Current password", "New password", "Confirm new password"]) {
      const input = screen.getByLabelText(label, { selector: "input" })
      expect(document.querySelectorAll(`label[for="${input.id}"]`)).toHaveLength(1)
      const { left, right } = row(input)
      expect(left.querySelector("label")?.textContent).toBe(label)
      expect(right.contains(input)).toBe(true)
    }
    const next = screen.getByLabelText("New password", { selector: "input" })
    expect(next.getAttribute("aria-describedby")).toBe(screen.getByText("At least 8 characters.").id)
    expect(screen.getByRole("button", { name: "Update password" })).toBeTruthy()
  })
})

describe("account settings — avatar", () => {
  it("the initials' violet has a dark-mode pair", async () => {
    await open()
    const initials = screen.getByText("YN")
    expect(classes(initials)).toEqual(
      expect.arrayContaining(["bg-violet-100", "text-violet-700", "dark:bg-violet-400/10", "dark:text-violet-300"]),
    )
  })
})
