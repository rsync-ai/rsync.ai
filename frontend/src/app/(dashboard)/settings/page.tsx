"use client"

import { useCallback, useEffect, useState } from "react"
import { useTheme } from "next-themes"
import { useRouter, useSearchParams } from "next/navigation"
import { PageHeader } from "@/components/layout/PageHeader"
import { Card, CardContent, CardDescription, CardHeader, CardTitle } from "@/components/ui/card"
import { Button } from "@/components/ui/button"
import { Input } from "@/components/ui/input"
import { SettingRow, SettingRows } from "@/components/kit/SettingRow"
import { Avatar, AvatarFallback } from "@/components/ui/avatar"
import { Tabs, TabsContent, TabsList, TabsTrigger } from "@/components/ui/tabs"
import { useHydrated } from "@/components/ui/local-date-time"
import { NotificationPreferencesCard } from "@/components/settings/NotificationPreferencesCard"
import { AgentAccessCard } from "@/components/settings/AgentAccessCard"
import { PasswordField } from "@/components/settings/PasswordField"
import { useFeatureFlags } from "@/config/features"
import { AlertTriangle, Bell, Bot, Loader2, Lock, Monitor, Moon, Palette, Save, Sun, User } from "lucide-react"
import { authFetch } from "@/lib/api/auth-fetch"
import { API_ENDPOINTS } from "@/lib/config/api"
import { toast } from "sonner"
import { cn } from "@/lib/utils"

const TABS = ["account", "alerts", "ai-apps", "appearance"] as const
type SettingsTab = (typeof TABS)[number]

function parseTab(value: string | null): SettingsTab {
  return (TABS as readonly string[]).includes(value ?? "") ? (value as SettingsTab) : "account"
}

type ProfileRead =
  | { kind: "signed-out" }
  | { kind: "failed" }
  | { kind: "ready"; name: string; email: string; isAdmin: boolean }

// authFetch leaves /auth/* 401s to the caller, so a 401 here is the session
// being gone. Anything else (a 500, no network) is not, and the page says so.
async function readProfile(): Promise<ProfileRead> {
  try {
    const res = await authFetch(API_ENDPOINTS.AUTH.ME)
    if (res.status === 401) return { kind: "signed-out" }
    if (!res.ok) return { kind: "failed" }
    const data = (await res.json()) as { name?: string; email: string; role?: string }
    return { kind: "ready", name: data.name ?? "", email: data.email, isAdmin: data.role === "admin" }
  } catch {
    return { kind: "failed" }
  }
}

/**
 * Your own settings, one tab each: Account (profile, password), Email alerts,
 * AI apps (only when the agent gateway is on) and Appearance. The open tab is
 * kept in `?tab=`, so a link can open it and a reload keeps it.
 */
export default function SettingsPage() {
  const router = useRouter()
  const searchParams = useSearchParams()
  const { agentGateway } = useFeatureFlags()

  const requested = parseTab(searchParams.get("tab"))
  const [tab, setTab] = useState<SettingsTab>(requested)
  const [lastRequested, setLastRequested] = useState<SettingsTab>(requested)
  if (requested !== lastRequested) {
    // The URL moved while the page is open (the header's Settings link, back).
    setLastRequested(requested)
    setTab(requested)
  }
  // The AI apps tab exists only once the feature flags say so; until then a
  // link to it opens Account, then moves over when the flag arrives.
  const active: SettingsTab = tab === "ai-apps" && !agentGateway ? "account" : tab

  const changeTab = (value: string) => {
    const next = parseTab(value)
    setTab(next)
    window.history.replaceState(null, "", next === "account" ? "/settings" : `/settings?tab=${next}`)
  }

  const [name, setName] = useState("")
  const [email, setEmail] = useState("")
  const [isAdmin, setIsAdmin] = useState(false)
  const [profile, setProfile] = useState<"loading" | "failed" | "ready">("loading")

  const applyProfile = useCallback(
    (read: ProfileRead) => {
      if (read.kind === "signed-out") {
        router.push(`/logout?next=${encodeURIComponent(`${window.location.pathname}${window.location.search}`)}`)
        return
      }
      if (read.kind === "failed") {
        setProfile("failed")
        return
      }
      setName(read.name)
      setEmail(read.email)
      setIsAdmin(read.isAdmin)
      setProfile("ready")
    },
    [router],
  )

  useEffect(() => {
    let live = true
    void readProfile().then((read) => {
      if (live) applyProfile(read)
    })
    return () => {
      live = false
    }
  }, [applyProfile])

  const retryProfile = () => {
    setProfile("loading")
    void readProfile().then(applyProfile)
  }

  return (
    <div className="space-y-6">
      <PageHeader heading="Settings" description="Manage your account settings and preferences" />

      <Tabs value={active} onValueChange={changeTab} className="space-y-6">
        <TabsList aria-label="Settings" className="h-auto flex-wrap justify-start">
          <TabsTrigger value="account" className="gap-2">
            <User className="h-4 w-4" aria-hidden />
            Account
          </TabsTrigger>
          <TabsTrigger value="alerts" className="gap-2">
            <Bell className="h-4 w-4" aria-hidden />
            Email alerts
          </TabsTrigger>
          {agentGateway && (
            <TabsTrigger value="ai-apps" className="gap-2">
              <Bot className="h-4 w-4" aria-hidden />
              AI apps
            </TabsTrigger>
          )}
          <TabsTrigger value="appearance" className="gap-2">
            <Palette className="h-4 w-4" aria-hidden />
            Appearance
          </TabsTrigger>
        </TabsList>

        {/* Full width, one card under the other: each setting is a label-left,
            control-right row, so a side-by-side pair of cards only squeezed them. */}
        <TabsContent value="account" className="mt-0 space-y-6">
          <ProfileCard
            status={profile}
            name={name}
            email={email}
            onNameChange={setName}
            onRetry={retryProfile}
          />
          <PasswordCard email={email} />
        </TabsContent>

        <TabsContent value="alerts" className="mt-0">
          <NotificationPreferencesCard email={profile === "ready" ? email : undefined} isAdmin={isAdmin} />
        </TabsContent>

        {agentGateway && (
          <TabsContent value="ai-apps" className="mt-0 space-y-6">
            <AgentAccessCard />
          </TabsContent>
        )}

        <TabsContent value="appearance" className="mt-0">
          <AppearanceCard />
        </TabsContent>
      </Tabs>
    </div>
  )
}

function ProfileCard({
  status,
  name,
  email,
  onNameChange,
  onRetry,
}: {
  status: "loading" | "failed" | "ready"
  name: string
  email: string
  onNameChange: (name: string) => void
  onRetry: () => void
}) {
  const [saving, setSaving] = useState(false)
  const initials = status !== "ready"
    ? ""
    : name
      ? name.split(" ").map((n) => n[0]).join("").toUpperCase()
      : email[0]?.toUpperCase() ?? "?"

  const save = async () => {
    if (saving) return
    setSaving(true)
    try {
      const res = await authFetch(API_ENDPOINTS.AUTH.UPDATE_ME, {
        method: "PATCH",
        body: JSON.stringify({ name }),
      })
      if (!res.ok) {
        // A rejected name ("name is required", "workspace is read-only") says
        // why; pass it on rather than a bare "Failed to save profile".
        const data = (await res.json().catch(() => null)) as { error?: string; message?: string } | null
        const reason = (data?.error ?? data?.message ?? "").trim()
        toast.error("Failed to save profile", {
          description: reason || `The server answered ${res.status}.`,
        })
        return
      }
      toast.success("Profile updated")
    } catch (error) {
      toast.error("Failed to save profile", {
        description: error instanceof Error ? error.message : "The request never reached the server.",
      })
    } finally {
      setSaving(false)
    }
  }

  return (
    <Card>
      <CardHeader>
        <CardTitle className="flex items-center gap-2">
          <User className="h-5 w-5 text-zinc-500 dark:text-zinc-400" />
          Profile
        </CardTitle>
        <CardDescription>Your personal information</CardDescription>
      </CardHeader>
      <CardContent>
        {status === "failed" ? (
          <div className="flex items-center justify-between gap-3">
            <p className="flex items-center gap-2 text-sm text-zinc-600 dark:text-zinc-300">
              <AlertTriangle className="h-4 w-4 text-amber-500" />
              Could not load your profile.
            </p>
            <Button variant="outline" size="sm" onClick={onRetry}>
              Retry
            </Button>
          </div>
        ) : (
          <form
            className="space-y-5"
            onSubmit={(e) => {
              e.preventDefault()
              void save()
            }}
          >
            <div className="flex items-center gap-4">
              <Avatar className="h-14 w-14">
                <AvatarFallback className="bg-violet-100 text-lg text-violet-700 dark:bg-violet-400/10 dark:text-violet-300">
                  {initials}
                </AvatarFallback>
              </Avatar>
              {/* There is no photo upload, so a "Profile photo" label pointed at a
                  control that doesn't exist (#49). Say what the avatar is instead. */}
              <div className="min-w-0">
                <p className="truncate font-medium text-zinc-900 dark:text-white">
                  {status === "loading" ? "Loading…" : name || email}
                </p>
                <p className="text-xs text-zinc-500 dark:text-zinc-400">Your avatar shows the initials of your name.</p>
              </div>
            </div>

            <SettingRows>
              <SettingRow label="Full name" htmlFor="settings-name">
                <Input
                  id="settings-name"
                  value={name}
                  onChange={(e) => onNameChange(e.target.value)}
                  placeholder="Enter your name"
                  autoComplete="name"
                  disabled={status !== "ready"}
                  className="max-w-md"
                />
              </SettingRow>
              {/* htmlFor/id, not proximity: this input has no placeholder, so before the
                  association it had NO accessible name at all (axe `label`, critical). */}
              <SettingRow label="Email" htmlFor="settings-email" hint="You sign in with it. It can't be changed here.">
                <Input id="settings-email" value={email} disabled aria-describedby="settings-email-hint" className="max-w-md" />
              </SettingRow>
            </SettingRows>

            <div className="flex justify-end">
              <Button type="submit" disabled={saving || status !== "ready"}>
                {saving ? <Loader2 className="mr-2 h-4 w-4 animate-spin" /> : <Save className="mr-2 h-4 w-4" />}
                {saving ? "Saving…" : "Save changes"}
              </Button>
            </div>
          </form>
        )}
      </CardContent>
    </Card>
  )
}

function PasswordCard({ email }: { email: string }) {
  const [current, setCurrent] = useState("")
  const [next, setNext] = useState("")
  const [confirm, setConfirm] = useState("")
  const [changing, setChanging] = useState(false)

  const change = async () => {
    if (changing) return
    if (next !== confirm) {
      toast.error("New passwords do not match")
      return
    }
    if (next.length < 8) {
      toast.error("New password must be at least 8 characters")
      return
    }
    setChanging(true)
    try {
      const res = await authFetch(API_ENDPOINTS.AUTH.CHANGE_PASSWORD, {
        method: "PATCH",
        body: JSON.stringify({ current_password: current, new_password: next }),
      })
      if (!res.ok) {
        const data = (await res.json().catch(() => null)) as { error?: string } | null
        toast.error(data?.error ?? "Failed to change password")
        return
      }
      toast.success("Password changed successfully")
      setCurrent("")
      setNext("")
      setConfirm("")
    } catch {
      toast.error("Failed to change password")
    } finally {
      setChanging(false)
    }
  }

  return (
    <Card>
      <CardHeader>
        <CardTitle className="flex items-center gap-2">
          <Lock className="h-5 w-5 text-zinc-500 dark:text-zinc-400" />
          Change password
        </CardTitle>
        <CardDescription>Update your account password</CardDescription>
      </CardHeader>
      <CardContent>
        <form
          className="space-y-4"
          onSubmit={(e) => {
            e.preventDefault()
            void change()
          }}
        >
          {/* Lets a password manager tell whose password this is. */}
          <input type="text" name="username" autoComplete="username" value={email} readOnly hidden />
          <SettingRows>
            <SettingRow label="Current password" htmlFor="settings-current-password">
              <div className="max-w-md">
                <PasswordField
                  id="settings-current-password"
                  toggleNoun="current password"
                  value={current}
                  onChange={setCurrent}
                  placeholder="Enter current password"
                  autoComplete="current-password"
                />
              </div>
            </SettingRow>
            <SettingRow label="New password" htmlFor="settings-new-password" hint="At least 8 characters.">
              <div className="max-w-md">
                <PasswordField
                  id="settings-new-password"
                  describedBy="settings-new-password-hint"
                  toggleNoun="new password"
                  value={next}
                  onChange={setNext}
                  placeholder="At least 8 characters"
                  autoComplete="new-password"
                />
              </div>
            </SettingRow>
            <SettingRow label="Confirm new password" htmlFor="settings-confirm-password">
              <div className="max-w-md">
                <PasswordField
                  id="settings-confirm-password"
                  toggleNoun="password confirmation"
                  value={confirm}
                  onChange={setConfirm}
                  placeholder="Repeat new password"
                  autoComplete="new-password"
                />
              </div>
            </SettingRow>
          </SettingRows>
          <div className="flex justify-end">
            <Button type="submit" disabled={changing || !current || !next || !confirm}>
              {changing ? <Loader2 className="mr-2 h-4 w-4 animate-spin" /> : <Lock className="mr-2 h-4 w-4" />}
              {changing ? "Updating…" : "Update password"}
            </Button>
          </div>
        </form>
      </CardContent>
    </Card>
  )
}

const THEMES = [
  { value: "light", label: "Light", Icon: Sun },
  { value: "dark", label: "Dark", Icon: Moon },
  { value: "system", label: "System", Icon: Monitor },
] as const

function AppearanceCard() {
  // `theme` is the choice ("system" included); `resolvedTheme` is only what it
  // came out as, which is never "system".
  const { theme, resolvedTheme, setTheme } = useTheme()
  const hydrated = useHydrated()
  const chosen = hydrated ? theme : undefined

  return (
    <Card>
      <CardHeader>
        <CardTitle className="flex items-center gap-2">
          <Palette className="h-5 w-5 text-zinc-500 dark:text-zinc-400" />
          Appearance
        </CardTitle>
        <CardDescription>Choose how rsync.ai looks on this device</CardDescription>
      </CardHeader>
      <CardContent className="space-y-3">
        <div role="group" aria-label="Theme" className="grid max-w-md grid-cols-3 gap-3">
          {THEMES.map(({ value, label, Icon }) => (
            <button
              key={value}
              type="button"
              aria-pressed={chosen === value}
              onClick={() => setTheme(value)}
              className={cn(
                "flex flex-col items-center gap-2 rounded-lg border p-4 text-sm font-medium transition-colors focus-visible:outline-none focus-visible:ring-2 focus-visible:ring-violet-500",
                chosen === value
                  ? "border-violet-500 bg-violet-50 text-violet-700 dark:bg-violet-950 dark:text-violet-300"
                  : "border-zinc-200 text-zinc-700 hover:bg-zinc-50 dark:border-zinc-800 dark:text-zinc-300 dark:hover:bg-zinc-800/60",
              )}
            >
              <Icon className="h-5 w-5" aria-hidden />
              {label}
            </button>
          ))}
        </div>
        {chosen === "system" && resolvedTheme && (
          <p className="text-xs text-zinc-500 dark:text-zinc-400">Following your system, which is {resolvedTheme} now.</p>
        )}
      </CardContent>
    </Card>
  )
}
