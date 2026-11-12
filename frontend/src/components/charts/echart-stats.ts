// Loaded by chart-modules.ts the first time a box plot or a candlestick is drawn.
import { BoxplotChart, CandlestickChart } from "echarts/charts"
import { use as registerModules } from "echarts/core"

registerModules([BoxplotChart, CandlestickChart])
