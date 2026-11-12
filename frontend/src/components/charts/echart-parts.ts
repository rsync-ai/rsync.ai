// Loaded by chart-modules.ts the first time a treemap or a sunburst is drawn.
import { SunburstChart, TreemapChart } from "echarts/charts"
import { use as registerModules } from "echarts/core"

registerModules([TreemapChart, SunburstChart])
