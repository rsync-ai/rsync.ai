import { describe, expect, it } from "vitest"
import { existsSync, readFileSync } from "node:fs"
import { join, relative } from "node:path"
import ts from "typescript"
import { SRC, sourceFiles } from "./class-literals"

/**
 * Page content responds to the width of the page, not the window's.
 *
 * DashboardShell makes the page frame a `@container`. The sidebar and the docked
 * assistant both take width from that frame, and a viewport breakpoint sees neither:
 * with the assistant docked on a 1440px screen, `lg:grid-cols-3` still laid out three
 * columns in a 744px page and crushed every card in it. So page code uses container
 * variants (`@3xl:grid-cols-3`) and never `sm:`/`md:`/`lg:`/`xl:`/`2xl:`.
 *
 * Two places keep viewport variants, and rightly:
 * - the shell itself (header, sidebar, assistant), which is sized against the window;
 * - anything a portal renders outside the page (a dialog's or popover's content),
 *   where no `@container` is an ancestor, so a container variant would never match.
 */

// `lg`, `max-md`, `min-[480px]`, `[@media(...)]` — every variant that asks the window.
// A container variant starts with `@` (`@3xl`, `@max-md`, `@min-[480px]`), so none match.
const VIEWPORT = /^(?:(?:min|max)-(?:sm|md|lg|xl|2xl|\[[^\]]*\])|sm|md|lg|xl|2xl|\[@media.*\])$/

// Content rendered in a portal, outside the page frame: a primitive's *Content, or a
// component that is itself a dialog. A Trigger renders inline, in the page.
const OVERLAY =
  /^(Dialog|AlertDialog|Sheet|Drawer|Popover|DropdownMenu|ContextMenu|Tooltip|HoverCard|Select|Command|Menubar)(Sub)?(Content|Portal)$|^(?!AlertDialog$).+(Dialog|Modal)$/

/** Sized against the window, outside the page frame — viewport variants are right here. */
const SHELL: Record<string, string> = {
  "components/layout/DashboardShell.tsx": "the frame itself: makes room for the sidebar and the dock",
  "components/layout/Header.tsx": "fixed to the window's top edge",
  "components/layout/Sidebar.tsx": "fixed to the window's left edge",
  "components/assistant/AssistantPanel.tsx": "fixed beside the page; docks only on a wide window",
  "components/assistant/AssistantResizeHandle.tsx": "the docked panel's edge; shown only on a wide window",
  "components/assistant/AssistantLauncher.tsx": "fixed to the window's corner",
}

function inPageScope(rel: string): boolean {
  if (rel in SHELL) return false
  if (rel.startsWith("components/ui/")) return false // primitives: portals and overlays
  return rel.startsWith("app/(dashboard)/") || rel.startsWith("components/")
}

/** `@3xl:hover:bg-x` → ["@3xl", "hover", "bg-x"]; a `:` inside `[…]` or `(…)` does not split. */
function variantsOf(token: string): string[] {
  const parts: string[] = []
  let depth = 0
  let current = ""
  for (const ch of token) {
    if (ch === "[" || ch === "(") depth++
    else if (ch === "]" || ch === ")") depth--
    if (ch === ":" && depth === 0) {
      parts.push(current)
      current = ""
    } else current += ch
  }
  return parts // the trailing utility is dropped
}

function tagOf(node: ts.JsxOpeningLikeElement): string {
  return node.tagName.getText().replace(/^.*\./, "")
}

function insideOverlay(node: ts.Node): boolean {
  for (let p = node.parent; p; p = p.parent) {
    if (ts.isJsxElement(p) && OVERLAY.test(tagOf(p.openingElement))) return true
    if ((ts.isJsxSelfClosingElement(p) || ts.isJsxOpeningElement(p)) && OVERLAY.test(tagOf(p))) return true
  }
  return false
}

function viewportVariantsIn(fileName: string, source: string): string[] {
  // A `.ts` file parsed as TSX misreads a `<T>value` cast as an element.
  const kind = fileName.endsWith(".tsx") ? ts.ScriptKind.TSX : ts.ScriptKind.TS
  const sf = ts.createSourceFile(fileName, source, ts.ScriptTarget.Latest, true, kind)
  const found: string[] = []
  const visit = (node: ts.Node) => {
    const text =
      ts.isStringLiteral(node) ||
      ts.isNoSubstitutionTemplateLiteral(node) ||
      ts.isTemplateHead(node) ||
      ts.isTemplateMiddle(node) ||
      ts.isTemplateTail(node)
        ? node.text
        : null
    if (text && !ts.isImportDeclaration(node.parent) && !ts.isExportDeclaration(node.parent)) {
      const tokens = text.split(/\s+/).filter((t) => variantsOf(t).some((v) => VIEWPORT.test(v)))
      if (tokens.length && !insideOverlay(node)) {
        const line = sf.getLineAndCharacterOfPosition(node.getStart(sf)).line + 1
        found.push(`${fileName}:${line} ${tokens.join(" ")}`)
      }
    }
    ts.forEachChild(node, visit)
  }
  visit(sf)
  return found
}

describe("page layout breakpoints", () => {
  const files = sourceFiles(SRC)
    .map((file) => relative(SRC, file))
    .filter(inPageScope)

  it("scans the dashboard pages and the components they render", () => {
    // A walker that silently matched nothing would pass every assertion below.
    expect(files.filter((f) => f.startsWith("app/(dashboard)/")).length).toBeGreaterThan(30)
    expect(files.filter((f) => f.startsWith("components/")).length).toBeGreaterThan(100)
  })

  it("finds a viewport variant in page code, but not in a dialog's content", () => {
    const sample = `
      export function Page() {
        return (
          <div className="grid lg:grid-cols-3">
            <Card className={cn("p-4", wide && "md:hover:p-6")} />
            <DialogContent className="sm:max-w-lg"><div className="md:flex" /></DialogContent>
            <ShareDialog className="lg:w-96" />
            <div className="@3xl:grid-cols-3 @max-md:hidden @min-[480px]:p-2 data-[state=open]:block" />
            <div className="min-[480px]:p-2 max-md:hidden [@media(min-width:40rem)]:flex" />
          </div>
        )
      }`
    expect(viewportVariantsIn("sample.tsx", sample)).toEqual([
      "sample.tsx:4 lg:grid-cols-3",
      "sample.tsx:5 md:hover:p-6",
      "sample.tsx:9 min-[480px]:p-2 max-md:hidden [@media(min-width:40rem)]:flex",
    ])
  })

  it("lays page content out by the page's width (container variants), never the window's", () => {
    const offenders = files.flatMap((rel) => viewportVariantsIn(rel, readFileSync(join(SRC, rel), "utf8")))
    expect(
      offenders,
      "Use a container variant instead (sm→@md, md→@xl, lg→@3xl, xl→@5xl, 2xl→@7xl). " +
        "Only content a portal renders outside the page keeps a viewport variant.",
    ).toEqual([])
  })

  it("names only shell files that still exist", () => {
    for (const rel of Object.keys(SHELL)) expect(existsSync(join(SRC, rel)), rel).toBe(true)
  })
})
