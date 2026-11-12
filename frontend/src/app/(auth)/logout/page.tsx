"use client"

import { Suspense, useEffect } from "react"
import { useRouter, useSearchParams } from "next/navigation"
import { clearSignedInState } from "@/lib/auth/signed-in-state"
import { API_GATEWAY_URL } from "@/lib/config/api"

// Self-correcting base URL (see @/lib/config/api): a mis-baked localhost value
// is ignored in favor of the current origin on a real deployment.
const API_URL = API_GATEWAY_URL

function LogoutContent() {
  const router = useRouter()
  const searchParams = useSearchParams()

  useEffect(() => {
    let cancelled = false
    ;(async () => {
      try {
        // Clear server-side session + cookies.
        await fetch(`${API_URL}/api/v1/auth/logout`, {
          method: "POST",
          credentials: "include",
          headers: { "Content-Type": "application/json", Accept: "application/json" },
        })
      } catch {
        // Ignore network failures; still clear client state and proceed.
      } finally {
        // The same list lib/auth.ts logout() clears, in every workspace.
        clearSignedInState()
        if (!cancelled) {
          const next = searchParams.get("next")
          router.replace(next ? `/login?next=${encodeURIComponent(next)}` : "/login")
        }
      }
    })()
    return () => {
      cancelled = true
    }
  }, [router, searchParams])

  return <div className="min-h-screen flex items-center justify-center text-sm text-muted-foreground">Signing out…</div>
}

export default function LogoutPage() {
  return (
    <Suspense>
      <LogoutContent />
    </Suspense>
  )
}
