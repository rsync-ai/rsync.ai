import { useCallback, type KeyboardEvent } from "react"

type TextBox = HTMLTextAreaElement | HTMLInputElement

/**
 * Tab writes the box's suggestion into an EMPTY AI prompt box, with the cursor at
 * the end. Once the box has text, Tab moves focus as usual, so a second Tab
 * always leaves the box and the keyboard is never trapped.
 *
 * Never taken over: Shift+Tab, Tab with any other modifier, and Tab pressed while
 * an input method (IME) is composing. A keydown something else already handled
 * is left alone too: the dialog's focus trap (ui/dialog.tsx) listens on the
 * document in the CAPTURE phase, so it runs first, and when the box is the
 * dialog's last stop it has already moved focus by the time this sees the key.
 *
 * Only for the AI prompt boxes, opted in one by one: a name, token or search
 * field keeps plain Tab. Not for the SQL editor either, where Tab indents.
 *
 * `suggestion` must be text that reads right once written into the box, not a
 * placeholder with "e.g." or "(optional)" in it.
 */
export function useSuggestionTab({
  value,
  suggestion,
  onAccept,
}: {
  value: string
  suggestion: string | null | undefined
  onAccept: (text: string) => void
}) {
  const text = suggestion?.trim() ?? ""
  const canAccept = text !== "" && value.trim() === ""

  const onKeyDown = useCallback(
    (e: KeyboardEvent<TextBox>): boolean => {
      if (!canAccept || !isPlainTab(e)) return false
      e.preventDefault()
      onAccept(text)
      const box = e.currentTarget
      // After React has written the value: put the cursor at the end.
      requestAnimationFrame(() => {
        if (document.activeElement === box) box.setSelectionRange(text.length, text.length)
      })
      return true
    },
    [canAccept, onAccept, text],
  )

  return { onKeyDown, showHint: canAccept }
}

function isPlainTab(e: KeyboardEvent<TextBox>): boolean {
  return (
    e.key === "Tab" &&
    !e.shiftKey &&
    !e.altKey &&
    !e.ctrlKey &&
    !e.metaKey &&
    !e.defaultPrevented &&
    !e.nativeEvent.isComposing
  )
}
