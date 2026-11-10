"use client"

import { useCallback, useEffect, useRef, useState } from "react"

import {
  activateWorkflow,
  deleteWorkflow,
  getWorkflow,
  pauseWorkflow,
  runWorkflowNow,
  testWorkflow,
  type Workflow,
  type WorkflowTestResult,
} from "@/lib/api/workflows"
import { usePollWhileRunning } from "@/components/workflows/usePollWhileRunning"

interface Loaded {
  workflow: Workflow | null
  canManage: boolean
  error: Error | null
}

/**
 * Loads one workflow, and reads it again while its last run is going so the
 * canvas follows the run step by step. A response that lands after the
 * component unmounted, or after the id changed, is dropped instead of setting
 * state on a stale view.
 */
export function useWorkflow(workflowId: string) {
  const [loaded, setLoaded] = useState<Loaded>({ workflow: null, canManage: false, error: null })
  const [version, setVersion] = useState(0)
  // Every read and every replace takes the next ticket; a read's answer is
  // applied only while its ticket is the latest, so an answer that a later
  // read or a saved change overtook never puts the older workflow back.
  const ticket = useRef(0)

  useEffect(() => {
    let alive = true
    const mine = ++ticket.current
    getWorkflow(workflowId).then(
      ({ workflow, can_manage }) => {
        if (alive && mine === ticket.current) setLoaded({ workflow, canManage: can_manage, error: null })
      },
      (err: unknown) => {
        if (alive && mine === ticket.current) {
          setLoaded((s) => ({ ...s, error: err instanceof Error ? err : new Error("Could not load the workflow") }))
        }
      },
    )
    return () => {
      alive = false
    }
  }, [workflowId, version])

  // A failed re-read keeps the workflow shown (the error view would replace
  // the page); the next tick tries again.
  usePollWhileRunning(loaded.workflow?.last_run?.status === "running", async () => {
    const mine = ++ticket.current
    const got = await getWorkflow(workflowId).catch(() => null)
    if (got && mine === ticket.current) setLoaded({ workflow: got.workflow, canManage: got.can_manage, error: null })
  })

  const reload = useCallback(() => {
    setLoaded((s) => ({ ...s, error: null }))
    setVersion((v) => v + 1)
  }, [])
  const replace = useCallback((workflow: Workflow) => {
    ticket.current++
    setLoaded((s) => ({ ...s, workflow }))
  }, [])

  return { ...loaded, reload, replace }
}

export type WorkflowAction = "" | "test" | "activate" | "pause" | "run" | "delete"

/**
 * Test, Turn on, Pause, Run now and Delete for one workflow, with one action in
 * flight at a time. The server checks every one of them again. onStart lets a
 * page drop its own status line as an action starts, and clearStatus drops this
 * one's, so the page says only what the last thing done said.
 */
export function useWorkflowActions(
  workflow: Workflow | null,
  opts: {
    replace: (w: Workflow) => void
    reload: () => void
    onChanged?: () => void
    onDeleted?: () => void
    onStart?: () => void
  },
) {
  const [busy, setBusy] = useState<WorkflowAction>("")
  const [error, setError] = useState<string | null>(null)
  const [notice, setNotice] = useState<string | null>(null)
  const [test, setTest] = useState<WorkflowTestResult | null>(null)
  const { replace, reload, onChanged, onDeleted, onStart } = opts

  const run = useCallback(
    async (kind: WorkflowAction, fn: () => Promise<void>) => {
      if (busy || !workflow) return
      setBusy(kind)
      setError(null)
      setNotice(null)
      onStart?.()
      try {
        await fn()
        onChanged?.()
      } catch (err) {
        setError(err instanceof Error ? err.message : "That did not work")
      } finally {
        setBusy("")
      }
    },
    [busy, workflow, onChanged, onStart],
  )

  const id = workflow?.id ?? ""
  const manual = workflow?.definition.trigger.type === "manual"
  return {
    busy,
    error,
    notice,
    test,
    clearTest: () => setTest(null),
    clearStatus: () => {
      setNotice(null)
      setError(null)
    },
    onTest: () =>
      run("test", async () => {
        setTest(await testWorkflow(id))
      }),
    onActivate: () =>
      run("activate", async () => {
        replace(await activateWorkflow(id))
        setNotice(manual ? "Turned on. It runs when you click Run now." : "Turned on. It runs as you, on its trigger.")
      }),
    onPause: () =>
      run("pause", async () => {
        await pauseWorkflow(id)
        reload()
      }),
    onRun: () =>
      run("run", async () => {
        await runWorkflowNow(id)
        setNotice("Run started. It sends the message only if the condition is true.")
      }),
    onDelete: () =>
      run("delete", async () => {
        await deleteWorkflow(id)
        onDeleted?.()
      }),
  }
}
