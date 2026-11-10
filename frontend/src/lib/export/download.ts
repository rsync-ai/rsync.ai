/**
 * Saving an export in the browser (dashboards-design.md § 8, D3). Browser only: call
 * these from an event handler.
 */

/** Saves the blob as a file named `filename`, through a link the page clicks. */
export function downloadBlob(blob: Blob, filename: string): void {
  const url = URL.createObjectURL(blob)
  const a = document.createElement("a")
  a.href = url
  a.download = filename
  a.rel = "noopener"
  a.style.display = "none"
  document.body.appendChild(a)
  a.click()
  a.remove()
  // Kept a moment after the click: a browser may read the URL after click() returns.
  setTimeout(() => URL.revokeObjectURL(url), 1_000)
}

/** The file name a Content-Disposition gives: its UTF-8 filename* first, then filename, else `fallback`. */
export function filenameFromDisposition(header: string | null, fallback: string): string {
  if (!header) return fallback
  const star = /filename\*\s*=\s*UTF-8''([^;]+)/i.exec(header)
  if (star) {
    try {
      const name = decodeURIComponent(star[1].trim())
      if (name) return name
    } catch {
      // A malformed escape: the plain filename is next.
    }
  }
  const plain = /filename\s*=\s*(?:"([^"]*)"|([^;]+))/i.exec(header)
  const name = (plain?.[1] ?? plain?.[2] ?? "").trim()
  return name || fallback
}
