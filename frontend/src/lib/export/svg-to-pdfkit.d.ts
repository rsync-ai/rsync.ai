// svg-to-pdfkit ships no types; this is the one call the PDF export makes.
declare module "svg-to-pdfkit" {
  interface SVGtoPDFOptions {
    width?: number
    height?: number
    preserveAspectRatio?: string
    assumePt?: boolean
    /** The font name the document has registered, for an SVG font family. */
    fontCallback?: (family: string, bold: boolean, italic: boolean) => string
    warningCallback?: (message: string) => void
  }
  function SVGtoPDF(doc: PDFKit.PDFDocument, svg: string, x: number, y: number, options?: SVGtoPDFOptions): void
  export default SVGtoPDF
}
