"use client"

import { useEffect, useState, useCallback } from "react"
import { PageHeader } from "@/components/layout/PageHeader"
import { AdminNav } from "@/components/admin/AdminNav"
import { Card, CardContent, CardDescription, CardHeader, CardTitle } from "@/components/ui/card"
import { Button } from "@/components/ui/button"
import { Label } from "@/components/ui/label"
import { RadioGroup, RadioGroupItem } from "@/components/ui/radio-group"
import { AdminReadFailureState, LoadingState } from "@/components/admin/AdminStates"
import { toast } from "sonner"
import { AlertTriangle, Save, UserPlus } from "lucide-react"
import { adminGetSettings, adminUpdateSettings, type AdminReadFailure } from "@/lib/api/admin"
import { AIModelsCard } from "@/components/admin/AIModelsCard"
import { useFeatureFlags } from "@/config/features"

export default function AdminSettingsPage() {
  const [loading, setLoading] = useState(true)
  const [failure, setFailure] = useState<AdminReadFailure | null>(null)
  const [registrationMode, setRegistrationMode] = useState("open")
  const [loadFailed, setLoadFailed] = useState(false)
  const [saving, setSaving] = useState(false)
  // The AI models routes exist only while the assistant is on.
  const { assistant } = useFeatureFlags()

  const load = useCallback(async () => {
    setLoading(true)
    setFailure(null)
    setLoadFailed(false)
    const r = await adminGetSettings()
    if (r.ok) {
      setRegistrationMode(r.data.settings?.registration_mode || "open")
    } else if (r.status === 401 || r.status === 403 || r.status === 429) {
      setFailure(r)
    } else {
      // Any other failure must not fall through to the form: `|| "open"` would
      // render Open Registration over a read that never came back, with Save
      // live to write that invention back to an invite-only instance.
      setLoadFailed(true)
    }
    setLoading(false)
  }, [])

  useEffect(() => { load() }, [load])

  const handleSave = async () => {
    setSaving(true)
    try {
      await adminUpdateSettings({ registration_mode: registrationMode })
      toast.success("Settings saved")
    } catch (err) {
      toast.error(err instanceof Error ? err.message : "Failed to save settings")
    }
    setSaving(false)
  }

  return (
    <div className="space-y-5">
      <PageHeader heading="Admin" description="Instance settings" />
      <AdminNav />

      {loading ? (
        <LoadingState />
      ) : failure ? (
        <AdminReadFailureState failure={failure} onRetry={load} />
      ) : loadFailed ? (
        // No form here on purpose. Showing the controls would mean showing a
        // registration mode we never read, and leaving Save reachable would let
        // that guess be written back.
        <Card className="max-w-2xl">
          <CardHeader>
            <CardTitle className="flex items-center gap-2">
              <AlertTriangle className="h-5 w-5 text-amber-500" />
              Could not load instance settings
            </CardTitle>
            <CardDescription>
              The current registration mode is unknown, so it is not shown and cannot be
              changed from here. Existing settings are unaffected.
            </CardDescription>
          </CardHeader>
          <CardContent>
            <Button variant="outline" onClick={load}>Retry</Button>
          </CardContent>
        </Card>
      ) : (
        <div className="max-w-2xl space-y-6">
          <Card>
            <CardHeader>
              <CardTitle className="flex items-center gap-2">
                <UserPlus className="h-5 w-5 text-zinc-500 dark:text-zinc-400" />
                Registration
              </CardTitle>
              <CardDescription>
                Control how new users can sign up for this instance.
              </CardDescription>
            </CardHeader>
            <CardContent className="space-y-4">
              <RadioGroup value={registrationMode} onValueChange={setRegistrationMode}>
                <div className="flex items-start gap-3">
                  <RadioGroupItem value="open" id="reg-open" className="mt-1" />
                  <div>
                    <Label htmlFor="reg-open" className="font-medium cursor-pointer">Open Registration</Label>
                    <p className="text-sm text-zinc-500 dark:text-zinc-400">Anyone can create an account.</p>
                  </div>
                </div>
                <div className="flex items-start gap-3">
                  <RadioGroupItem value="invite_only" id="reg-invite" className="mt-1" />
                  <div>
                    <Label htmlFor="reg-invite" className="font-medium cursor-pointer">Invite Only</Label>
                    <p className="text-sm text-zinc-500 dark:text-zinc-400">
                      New users must have a valid invite link to register. Admins can create invite links from the Invitations tab.
                    </p>
                  </div>
                </div>
              </RadioGroup>

              <div className="flex justify-end pt-2">
                <Button onClick={handleSave} disabled={saving}>
                  <Save className="h-4 w-4 mr-1.5" />
                  {saving ? "Saving..." : "Save Changes"}
                </Button>
              </div>
            </CardContent>
          </Card>
          {assistant && <AIModelsCard />}
        </div>
      )}
    </div>
  )
}
