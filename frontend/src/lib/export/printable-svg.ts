/**
 * An ECharts SVG made ready for svg-to-pdfkit (dashboards-design.md § 8, D3). A label
 * drawn inside a shape carries an outline — stroke plus paint-order="stroke", so a
 * browser paints the outline under the letters. svg-to-pdfkit has no paint-order: it
 * fills the letters, then strokes over them, and the label disappears into its own
 * outline. The outline is dropped from every <text>; nothing else changes. No chart
 * type today labels its marks (their axis text has no outline), so this keeps the
 * first one that does readable on paper.
 * No DOM: this file is safe anywhere.
 */

const TEXT_OPEN = /<text\b[^>]*>/g
const OUTLINE_ATTR = /\s(?:stroke|stroke-width|stroke-opacity|stroke-linejoin|stroke-miterlimit|paint-order)="[^"]*"/g

export function printableSvg(svg: string): string {
  return svg.replace(TEXT_OPEN, (tag) => tag.replace(OUTLINE_ATTR, ""))
}
