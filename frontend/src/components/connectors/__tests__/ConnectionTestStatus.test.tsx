/**
 * The stored last-test result on /connections and /connections/:id
 * (ConnectionTestStatus.tsx). The gateway persists last_test_status /
 * last_test_error on every test of a stored connection (connections.go
 * TestConnection); before this, a failed test rendered as "Not tested" and the
 * reason was only ever in a toast.
 */

import { afterEach, describe, expect, it, vi } from "vitest"
import { cleanup, render, screen } from "@testing-library/react"
import userEvent from "@testing-library/user-event"
import "@testing-library/jest-dom"

import {
  ConnectionTestBadge,
  LastTestErrorLine,
  LastTestFailedAlert,
  connectionTestState,
  lastTestSummary,
} from "@/components/connectors/ConnectionTestStatus"

afterEach(() => cleanup())

const minutesAgo = (m: number) => new Date(Date.now() - m * 60_000).toISOString()

const failed = {
  is_connected: false,
  last_tested_at: minutesAgo(12),
  last_test_status: "failed",
  last_test_error: "password authentication failed for user \"etl\"",
}

describe("connectionTestState", () => {
  it("reads the stored verdict", () => {
    expect(connectionTestState({ is_connected: true, last_test_status: "success" })).toBe("connected")
    expect(connectionTestState(failed)).toBe("failed")
    expect(connectionTestState({})).toBe("untested")
  })

  it("an expired token beats a failed test", () => {
    expect(connectionTestState({ ...failed, is_expired: true })).toBe("expired")
  })

  // KI-CONNECTION-BADGE-OUTRANKS-EXPIRED-TOKEN: a stored success only proves
  // the token worked when the test ran. A token that expired after that test
  // is the newer fact, so it wins.
  it("a token that expired after the last passing test beats it", () => {
    expect(
      connectionTestState({
        is_connected: true,
        last_test_status: "success",
        last_tested_at: minutesAgo(3 * 24 * 60),
        is_expired: true,
        token_expires_at: minutesAgo(60),
      })
    ).toBe("expired")
  })

  it("an expired token with no test time to compare beats a stored success", () => {
    expect(connectionTestState({ is_connected: true, last_test_status: "success", is_expired: true })).toBe("expired")
  })

  it("a test that passed after the token's expiry still reads connected", () => {
    expect(
      connectionTestState({
        is_connected: true,
        last_test_status: "success",
        last_tested_at: minutesAgo(5),
        is_expired: true,
        token_expires_at: minutesAgo(60),
      })
    ).toBe("connected")
  })
})

describe("ConnectionTestBadge", () => {
  it("says Test failed, not Not tested, after a failed test", () => {
    render(<ConnectionTestBadge connection={failed} />)
    expect(screen.getByText("Test failed")).toBeInTheDocument()
    expect(screen.queryByText("Not tested")).toBeNull()
  })

  it("says how old a passing test is", () => {
    const at = minutesAgo(3 * 24 * 60)
    render(<ConnectionTestBadge connection={{ is_connected: true, last_test_status: "success", last_tested_at: at }} />)
    const badge = screen.getByTestId("connection-test-badge")
    expect(badge).toHaveTextContent("Connected · 3d ago")
    expect(badge.getAttribute("title")).toMatch(/^Last tested /)
  })

  it("says Token expired, not Connected, once the token outlives the last passing test", () => {
    render(
      <ConnectionTestBadge
        connection={{
          is_connected: true,
          last_test_status: "success",
          last_tested_at: minutesAgo(3 * 24 * 60),
          is_expired: true,
          token_expires_at: minutesAgo(60),
        }}
      />
    )
    expect(screen.getByTestId("connection-test-badge")).toHaveTextContent("Token expired")
    expect(screen.queryByText(/Connected/)).toBeNull()
  })
})

describe("LastTestErrorLine", () => {
  it("shows why and when the last test failed", () => {
    render(<LastTestErrorLine connection={failed} />)
    const line = screen.getByTestId("connection-last-test-error")
    expect(line).toHaveTextContent('Last test failed 12m ago: password authentication failed for user "etl"')
    expect(line).toHaveAttribute("title", failed.last_test_error)
  })

  it("renders nothing unless the last test failed", () => {
    render(<LastTestErrorLine connection={{ is_connected: true, last_test_status: "success", last_test_error: "" }} />)
    render(<LastTestErrorLine connection={{}} />)
    expect(screen.queryByTestId("connection-last-test-error")).toBeNull()
  })

  it("says so when the connector gave no message", () => {
    render(<LastTestErrorLine connection={{ ...failed, last_test_error: "  " }} />)
    expect(screen.getByTestId("connection-last-test-error")).toHaveTextContent("The connector returned no error message.")
  })
})

describe("LastTestFailedAlert", () => {
  it("shows the stored error and re-runs the test", async () => {
    const onRetest = vi.fn()
    render(<LastTestFailedAlert connection={failed} onRetest={onRetest} testing={false} />)
    const alert = screen.getByTestId("connection-last-test-failed")
    expect(alert).toHaveTextContent("The last connection test failed · 12m ago")
    expect(alert).toHaveTextContent('password authentication failed for user "etl"')

    await userEvent.click(screen.getByRole("button", { name: "Test again" }))
    expect(onRetest).toHaveBeenCalledTimes(1)
  })

  it("locks the button while a test runs", () => {
    render(<LastTestFailedAlert connection={failed} onRetest={() => {}} testing />)
    expect(screen.getByRole("button", { name: "Testing..." })).toBeDisabled()
  })

  it("is absent once the connection passes", () => {
    render(
      <LastTestFailedAlert
        connection={{ is_connected: true, last_test_status: "success", last_tested_at: minutesAgo(1) }}
        onRetest={() => {}}
        testing={false}
      />
    )
    expect(screen.queryByTestId("connection-last-test-failed")).toBeNull()
  })
})

describe("lastTestSummary", () => {
  it("names the verdict and when", () => {
    expect(lastTestSummary(failed)).toBe("Failed · 12m ago")
    expect(lastTestSummary({ last_test_status: "success", last_tested_at: minutesAgo(0) })).toBe("Succeeded · just now")
    expect(lastTestSummary({})).toBe("Never")
  })
})
