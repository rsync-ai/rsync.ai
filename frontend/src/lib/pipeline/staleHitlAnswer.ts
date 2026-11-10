import { toast } from "sonner"
import type { HITLAnswerRefusedError } from "@/lib/api/pipelines"

/**
 * Says why a HITL dialog closed without resuming the run: its step was answered
 * in another tab, another answer is on its way, or the run already finished.
 *
 * One toast per run, however many dialogs or auto-retries report the same
 * refusal (sonner replaces a toast with the same id).
 */
export function notifyStaleHITLAnswer(err: HITLAnswerRefusedError): void {
  toast.info(err.message, { id: `hitl-answer-refused:${err.executionId ?? ""}`, duration: 6000 })
}
