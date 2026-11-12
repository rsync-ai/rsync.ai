"use client"

import { useEffect } from "react"
import { useReactFlow, useStore } from "@xyflow/react"

/**
 * Fits a React Flow canvas to its nodes again whenever the canvas changes size
 * or gets different nodes. The `fitView` prop fits only once, when the canvas
 * first lays out, so a canvas narrowed later (by a panel beside it, the window,
 * or the docked Ask rsync.ai panel) kept its old zoom and cut off every node
 * past its new edge. Render it inside <ReactFlow>.
 *
 * Only for a canvas that shows a whole graph: a resize gives up a zoom or pan
 * the user chose. `shape` names the nodes and where they sit, not which one is
 * selected, so clicking a node does not move the view.
 *
 * With `minZoom`, a graph that would only fit below it (a long one in a narrow
 * box, its labels too small to read) is shown at `minZoom` instead, from its
 * left end where it starts, and the rest is a drag away. Only for a canvas
 * that can be dragged.
 */
export function FitViewOnResize({
  padding,
  maxZoom,
  minZoom,
  shape,
}: {
  padding: number
  maxZoom: number
  minZoom?: number
  shape: string
}) {
  const width = useStore((s) => s.width)
  const height = useStore((s) => s.height)
  const { fitView, getViewport, getNodes, getNodesBounds, setViewport } = useReactFlow()

  useEffect(() => {
    // Not laid out yet, or hidden: there is nothing to fit into.
    if (width <= 0 || height <= 0) return
    // A fit resolves once the nodes are measured; only the latest one moves the view.
    let latest = true
    void fitView({ padding, maxZoom }).then(() => {
      if (!latest || minZoom === undefined || getViewport().zoom >= minZoom) return
      const b = getNodesBounds(getNodes())
      // The margin React Flow leaves around a fit, so the start sits where it would.
      const margin = (size: number) => Math.floor((size - size / (1 + padding)) / 2)
      const tall = b.height * minZoom + 2 * margin(height)
      void setViewport({
        x: margin(width) - b.x * minZoom,
        y: tall <= height ? (height - b.height * minZoom) / 2 - b.y * minZoom : margin(height) - b.y * minZoom,
        zoom: minZoom,
      })
    })
    return () => {
      latest = false
    }
  }, [width, height, shape, padding, maxZoom, minZoom, fitView, getViewport, getNodes, getNodesBounds, setViewport])

  return null
}
