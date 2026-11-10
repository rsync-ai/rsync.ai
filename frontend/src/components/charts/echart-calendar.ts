// Loaded by chart-modules.ts the first time a calendar heatmap is drawn: its days are a
// heatmap's cells, which every chart registers, on a calendar.
import { CalendarComponent } from "echarts/components"
import { use as registerModules } from "echarts/core"

registerModules([CalendarComponent])
