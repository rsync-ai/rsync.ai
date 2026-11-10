import { useSyncExternalStore } from "react"

type Nav = Navigator & { userAgentData?: { platform?: string } }

// The platform never changes while the page is open.
const never = () => () => {}

/** ⌘ on a Mac, iPhone or iPad; Ctrl everywhere else. */
function modKeyOfThisBrowser(): ModKey {
  const nav = navigator as Nav
  return /mac|iphone|ipad|ipod/i.test(nav.userAgentData?.platform || nav.platform || nav.userAgent) ? "⌘" : "Ctrl"
}

export type ModKey = "⌘" | "Ctrl"

/**
 * The shortcut key's name as this person's keyboard says it (launch review bug
 * 15): ⌘ on Apple devices, Ctrl elsewhere. The handlers take either key; this is
 * only what a hint shows. The server, which cannot know, renders Ctrl.
 */
export function useModKey(): ModKey {
  return useSyncExternalStore(never, modKeyOfThisBrowser, () => "Ctrl")
}
