"use client"

/**
 * Delivery channels: where this workspace's workflows send. A Slack channel is
 * one incoming webhook (Slack makes one per channel: Apps > Incoming Webhooks
 * > Add to Slack, pick the channel, copy the URL). An email channel is a list
 * of up to 20 addresses. Only owners and admins add, change, test or delete
 * one; everyone sees the names, so Ask rsync.ai can be told "send it to
 * #finance". The server never returns a webhook URL (handlers/delivery_channels.go).
 */

import { useCallback, useEffect, useState } from "react"
import { Hash, Loader2, Mail, Plus, Send, Trash2 } from "lucide-react"

import { Button } from "@/components/ui/button"
import { Card, CardContent, CardDescription, CardHeader, CardTitle } from "@/components/ui/card"
import { Input } from "@/components/ui/input"
import { Label } from "@/components/ui/label"
import { Textarea } from "@/components/ui/textarea"
import {
  createDeliveryChannel,
  deleteDeliveryChannel,
  listDeliveryChannels,
  parseRecipients,
  testDeliveryChannel,
  type DeliveryChannel,
} from "@/lib/api/workflows"

export function DeliveryChannelsCard() {
  const [channels, setChannels] = useState<DeliveryChannel[] | null>(null)
  const [canManage, setCanManage] = useState(false)
  const [loadError, setLoadError] = useState<string | null>(null)
  const [adding, setAdding] = useState<"" | "slack" | "email">("")
  const [name, setName] = useState("")
  const [webhook, setWebhook] = useState("")
  const [recipients, setRecipients] = useState("")
  const [saving, setSaving] = useState(false)
  const [formError, setFormError] = useState<string | null>(null)
  const [rowBusy, setRowBusy] = useState<string>("")
  const [rowMessage, setRowMessage] = useState<{ id: string; ok: boolean; text: string } | null>(null)

  const load = useCallback(async () => {
    try {
      const data = await listDeliveryChannels()
      setChannels(data.channels)
      setCanManage(data.can_manage)
      setLoadError(null)
    } catch (err) {
      setLoadError(err instanceof Error ? err.message : "Could not load delivery channels")
    }
  }, [])

  // First load; a response that lands after unmount is dropped. Later reloads
  // (after an add or a delete) come from event handlers through load().
  useEffect(() => {
    let alive = true
    listDeliveryChannels().then(
      (data) => {
        if (!alive) return
        setChannels(data.channels)
        setCanManage(data.can_manage)
      },
      (err: unknown) => {
        if (alive) setLoadError(err instanceof Error ? err.message : "Could not load delivery channels")
      },
    )
    return () => {
      alive = false
    }
  }, [])

  const reset = () => {
    setAdding("")
    setName("")
    setWebhook("")
    setRecipients("")
    setFormError(null)
  }

  const save = async () => {
    if (!adding || saving) return
    setSaving(true)
    setFormError(null)
    try {
      await createDeliveryChannel(
        adding === "slack"
          ? { name, kind: "slack", webhook_url: webhook.trim() }
          : { name, kind: "email", recipients: parseRecipients(recipients) },
      )
      reset()
      await load()
    } catch (err) {
      setFormError(err instanceof Error ? err.message : "Could not add the channel")
    } finally {
      setSaving(false)
    }
  }

  const onTest = async (ch: DeliveryChannel) => {
    setRowBusy(ch.id)
    setRowMessage(null)
    try {
      await testDeliveryChannel(ch.id)
      setRowMessage({ id: ch.id, ok: true, text: ch.kind === "slack" ? "Test message posted." : "Test email sent." })
    } catch (err) {
      setRowMessage({ id: ch.id, ok: false, text: err instanceof Error ? err.message : "The test failed" })
    } finally {
      setRowBusy("")
    }
  }

  const onDelete = async (ch: DeliveryChannel) => {
    setRowBusy(ch.id)
    setRowMessage(null)
    try {
      await deleteDeliveryChannel(ch.id)
      await load()
    } catch (err) {
      setRowMessage({ id: ch.id, ok: false, text: err instanceof Error ? err.message : "Could not delete the channel" })
    } finally {
      setRowBusy("")
    }
  }

  return (
    <Card>
      <CardHeader>
        <CardTitle>Delivery channels</CardTitle>
        <CardDescription>
          Where workflows send their reports and alerts. Ask rsync.ai picks them by name, such as &ldquo;send it to
          #finance&rdquo;. Workflow messages can contain query results, so only owners and admins add channels.
        </CardDescription>
      </CardHeader>
      <CardContent className="space-y-4">
        {loadError && (
          <p role="alert" className="text-sm text-red-700 dark:text-red-300">
            {loadError}
          </p>
        )}
        {channels === null && !loadError && (
          <div role="status" className="flex items-center gap-2 text-sm text-zinc-500 dark:text-zinc-400">
            <Loader2 className="h-4 w-4 animate-spin" /> Loading…
          </div>
        )}
        {channels !== null && channels.length === 0 && (
          <p className="text-sm text-zinc-500 dark:text-zinc-400">No delivery channels yet.</p>
        )}
        {channels !== null && channels.length > 0 && (
          <ul aria-label="Delivery channels" className="divide-y divide-zinc-100 rounded-lg border border-zinc-200 dark:divide-zinc-800 dark:border-zinc-800">
            {channels.map((ch) => (
              <li key={ch.id} className="space-y-1 px-3 py-2">
                <div className="flex flex-wrap items-center gap-2">
                  {ch.kind === "slack" ? (
                    <Hash className="h-4 w-4 text-zinc-500 dark:text-zinc-400" aria-label="Slack" />
                  ) : (
                    <Mail className="h-4 w-4 text-zinc-500 dark:text-zinc-400" aria-label="Email" />
                  )}
                  <span className="font-medium text-zinc-900 dark:text-white">{ch.name}</span>
                  <span className="text-xs text-zinc-500 dark:text-zinc-400">
                    {ch.kind === "slack"
                      ? "Slack webhook"
                      : ch.recipients?.length
                        ? ch.recipients.join(", ")
                        : `${ch.recipient_count} address${ch.recipient_count === 1 ? "" : "es"}`}
                  </span>
                  {ch.used_by.length > 0 && (
                    <span className="text-xs text-zinc-500 dark:text-zinc-400">· used by {ch.used_by.join(", ")}</span>
                  )}
                  {canManage && (
                    <span className="ml-auto flex gap-1">
                      <Button size="sm" variant="ghost" disabled={rowBusy === ch.id} onClick={() => void onTest(ch)}>
                        {rowBusy === ch.id ? <Loader2 className="mr-1 h-3.5 w-3.5 animate-spin" /> : <Send className="mr-1 h-3.5 w-3.5" />}
                        Send test
                      </Button>
                      <Button
                        size="sm"
                        variant="ghost"
                        disabled={rowBusy === ch.id || ch.used_by.length > 0}
                        title={ch.used_by.length > 0 ? "A workflow sends here" : "Delete"}
                        onClick={() => void onDelete(ch)}
                      >
                        <Trash2 className="h-3.5 w-3.5" />
                        <span className="sr-only">Delete {ch.name}</span>
                      </Button>
                    </span>
                  )}
                </div>
                {rowMessage?.id === ch.id && (
                  <p
                    role={rowMessage.ok ? "status" : "alert"}
                    className={rowMessage.ok ? "text-xs text-emerald-700 dark:text-emerald-300" : "text-xs text-red-700 dark:text-red-300"}
                  >
                    {rowMessage.text}
                  </p>
                )}
              </li>
            ))}
          </ul>
        )}

        {canManage && !adding && (
          <div className="flex flex-wrap gap-2">
            <Button size="sm" variant="outline" onClick={() => setAdding("slack")}>
              <Plus className="mr-1 h-3.5 w-3.5" /> Slack channel
            </Button>
            <Button size="sm" variant="outline" onClick={() => setAdding("email")}>
              <Plus className="mr-1 h-3.5 w-3.5" /> Email list
            </Button>
          </div>
        )}
        {!canManage && channels !== null && (
          <p className="text-xs text-zinc-500 dark:text-zinc-400">Only workspace owners and admins add delivery channels.</p>
        )}

        {adding && (
          <form
            aria-label={adding === "slack" ? "Add a Slack channel" : "Add an email list"}
            className="space-y-3 rounded-lg border border-zinc-200 p-3 dark:border-zinc-800"
            onSubmit={(e) => {
              e.preventDefault()
              void save()
            }}
          >
            <div className="space-y-1">
              <Label htmlFor="dc-name">Name</Label>
              <Input
                id="dc-name"
                value={name}
                placeholder={adding === "slack" ? "#finance" : "Finance team"}
                onChange={(e) => setName(e.target.value)}
                maxLength={100}
              />
            </div>
            {adding === "slack" ? (
              <div className="space-y-1">
                <Label htmlFor="dc-webhook">Incoming webhook URL</Label>
                <Input
                  id="dc-webhook"
                  type="password"
                  autoComplete="off"
                  value={webhook}
                  placeholder="https://hooks.slack.com/services/…"
                  onChange={(e) => setWebhook(e.target.value)}
                />
                <p className="text-xs text-zinc-500 dark:text-zinc-400">
                  In Slack: Apps &gt; Incoming Webhooks &gt; Add to Slack, pick the channel, copy the URL. It is stored
                  encrypted and never shown again.
                </p>
              </div>
            ) : (
              <div className="space-y-1">
                <Label htmlFor="dc-recipients">Email addresses</Label>
                <Textarea
                  id="dc-recipients"
                  value={recipients}
                  rows={3}
                  placeholder="cfo@example.com, finance@example.com"
                  onChange={(e) => setRecipients(e.target.value)}
                />
                <p className="text-xs text-zinc-500 dark:text-zinc-400">Up to 20, separated by commas or new lines.</p>
              </div>
            )}
            {formError && (
              <p role="alert" className="text-xs text-red-700 dark:text-red-300">
                {formError}
              </p>
            )}
            <div className="flex gap-2">
              <Button size="sm" type="submit" disabled={saving}>
                {saving && <Loader2 className="mr-1 h-3.5 w-3.5 animate-spin" />}
                Add channel
              </Button>
              <Button size="sm" type="button" variant="ghost" onClick={reset}>
                Cancel
              </Button>
            </div>
          </form>
        )}
      </CardContent>
    </Card>
  )
}
