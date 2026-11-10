"use client"

import { MotionConfig } from "framer-motion"

/**
 * Honours the OS "reduce motion" setting for every framer-motion animation:
 * movement is dropped, fades stay. Lives at the root so no page has to opt in.
 */
export function MotionPreferences({ children }: { children: React.ReactNode }) {
  return <MotionConfig reducedMotion="user">{children}</MotionConfig>
}
