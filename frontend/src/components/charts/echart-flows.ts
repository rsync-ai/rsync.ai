// Loaded by chart-modules.ts the first time a sankey is drawn.
import { SankeyChart } from "echarts/charts"
import { use as registerModules } from "echarts/core"
// A sankey hides the names that would sit on one another (labelLayout); echarts/core
// registers this by itself from a module the production bundle drops, so it is registered here.
import { LabelLayout } from "echarts/features"

registerModules([SankeyChart, LabelLayout])
