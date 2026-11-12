"use client"

import { Component, type ReactNode } from "react"

interface Props {
  children: ReactNode
  /** Try again when any of these change: new settings or rows may draw. */
  resetKeys: readonly unknown[]
}

interface State {
  failed: boolean
  keys: readonly unknown[]
}

const sameKeys = (a: readonly unknown[], b: readonly unknown[]) => a.length === b.length && a.every((k, i) => Object.is(k, b[i]))

/**
 * A chart that cannot be drawn, or whose chunk cannot load, says so in its card
 * instead of taking the page with it: without a boundary the error reaches the
 * route's error page.
 */
export class ChartBoundary extends Component<Props, State> {
  state: State = { failed: false, keys: this.props.resetKeys }

  static getDerivedStateFromError(): Partial<State> {
    return { failed: true }
  }

  static getDerivedStateFromProps(props: Props, state: State): Partial<State> | null {
    return sameKeys(props.resetKeys, state.keys) ? null : { failed: false, keys: props.resetKeys }
  }

  componentDidCatch(error: unknown) {
    console.error("[charts] could not draw a chart", error)
  }

  render() {
    if (this.state.failed) return <p className="text-xs text-red-700 dark:text-red-300">This chart could not be drawn.</p>
    return this.props.children
  }
}
