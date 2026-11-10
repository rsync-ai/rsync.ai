/**
 * The name an export downloads as (dashboards-design.md § 8, D3): the dashboard's or
 * chart's name and the day, in UTC. `utf8` is the name as typed, less the characters a
 * file system refuses; `ascii` is a slug of it, for a Content-Disposition's plain
 * filename; a name with nothing left in either is `fallback`. No DOM: this file is safe anywhere.
 */

export function exportFilename(
  name: string,
  ext: string,
  fallback: string,
  at: Date = new Date(),
): { ascii: string; utf8: string } {
  const day = at.toISOString().slice(0, 10)
  const slug = name
    .normalize("NFKD")
    .replace(/[^\w\s-]/g, "")
    .trim()
    .replace(/[\s_]+/g, "-")
    .toLowerCase()
    .slice(0, 80)
  const clean = name
    .replace(/[\\/:*?"<>|\u0000-\u001f]/g, " ")
    .replace(/\s+/g, " ")
    .trim()
    .slice(0, 120)
  return { ascii: `${slug || fallback}-${day}.${ext}`, utf8: `${clean || fallback} ${day}.${ext}` }
}
