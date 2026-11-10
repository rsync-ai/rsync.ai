// Used where too few zones come back from the engine: one without Intl.supportedValuesOf.
const FALLBACK = [
  "UTC",
  "America/New_York",
  "America/Chicago",
  "America/Denver",
  "America/Los_Angeles",
  "Europe/London",
  "Europe/Paris",
  "Asia/Tokyo",
  "Asia/Shanghai",
]

/**
 * The IANA zones a timezone picker offers, sorted: always "UTC", and each zone in `inUse`
 * (the stored or chosen value).
 *
 * Chrome's Intl.supportedValuesOf("timeZone") lists no "UTC" — nor does Node's — yet a
 * browser set to UTC reports "UTC" as its own zone, every schedule the chat creates is
 * stored in "UTC", and a browser may report an alias the list spells differently
 * (Asia/Calcutta for Asia/Kolkata). A Select whose value has no item renders blank, so a
 * picker built from that list alone opened empty on those, and could not offer UTC at all.
 * Every timezone picker takes its list from here (src/__tests__/timezone-choices.test.ts).
 */
export function timeZoneChoices(...inUse: Array<string | null | undefined>): string[] {
  let listed: string[] = []
  try {
    listed = (Intl as unknown as { supportedValuesOf?: (k: string) => string[] }).supportedValuesOf?.("timeZone") ?? []
  } catch {
    // An engine without the API: the fallback below.
  }
  const zones = new Set(listed.length > 0 ? listed : FALLBACK)
  zones.add("UTC")
  for (const tz of inUse) if (tz) zones.add(tz)
  return [...zones].sort((a, b) => a.localeCompare(b))
}
