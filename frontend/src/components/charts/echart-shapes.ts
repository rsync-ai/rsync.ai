// Loaded by chart-modules.ts the first time a pie, a donut or a funnel is drawn.
import { FunnelChart, PieChart } from "echarts/charts"
import { use as registerModules } from "echarts/core"
// A funnel's labels sit beside it (labelLayout); echarts/core registers this by itself
// from a module the production bundle drops, so it is registered here.
import { LabelLayout } from "echarts/features"

registerModules([PieChart, FunnelChart, LabelLayout])
