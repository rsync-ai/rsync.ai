# Charts and dashboards

A chart draws one model or saved query. A dashboard puts saved charts and text on one
page, with up to 4 filters. Both are under **Analyze** in the sidebar, next to the
Explorer. The design, and why it is built this way, is
dashboards-design.md.

| For | Read |
|---|---|
| Models and saved queries, the sources a chart draws | [Saved queries, models & schedules](saved-queries-and-models.md) |
| The Explorer, where a saved query is written | [Data Explorer](README.md) |

## Who can do what

| | Owner, admin | Member, viewer |
|---|---|---|
| Open a chart or dashboard they are shown | ✅ | ✅ |
| Pick a dashboard's filters | ✅ | ✅ |
| Make, change or delete a chart or dashboard | ✅ | — |
| Draft a dashboard with Ask rsync.ai, save a draft | ✅ | — |
| Ask rsync.ai to change one tile, apply the change | ✅ | — |
| See a dashboard's History | ✅ | ✅ |
| Restore a version from the History | ✅ | — |

A chart or dashboard is **Only me** or **The workspace**. Every chart reads its rows with
the rights of the person looking: a source they cannot see is not drawn, and a column the
Explorer masks for them is masked on the chart too. A tile whose chart is private to
another person says so instead of drawing it.

## Make a chart

**Analyze > Charts > New chart**, or **Create chart** in the Explorer.

1. **Source:** a model or a saved query. In the Explorer, **Create chart** picks the
   saved query you last opened from **Saved**, while the editor still holds its SQL;
   otherwise you pick one on the chart page.
2. **Chart type:** bar, horizontal bar, line, area, pie, donut, waterfall, funnel,
   histogram, scatter, bubble, heatmap, scorecard, table, pivot table, treemap, sunburst,
   sankey, calendar heatmap, bullet, box plot, candlestick, range bands or choropleth map.
3. **Columns:** the x axis, a measure (count, count distinct, sum, average, min or max of
   a column), and optionally a series column that splits the chart into one line or bar
   per value. Only the source's own columns are offered. A date or time axis can be
   grouped **By day**, **By week**, **By month**, **By quarter** or **By year** instead of
   by each value.
4. **Run** draws it and shows the SQL rsync.ai wrote; **Save chart** saves it.

What each type takes:

| Type | Columns |
|---|---|
| Bar, line, area | x and a measure; optionally a series, or a **Second measure** (below). A bar or an area chart with a series can **Stack the series**, then **Show each as a share of 100%** (of a count or a sum). With the **Default** order, a bar of one measure and no series is drawn largest first, unless its x is a date, grouped by a grain (**By month**, say) or not: then, like a line, oldest first, a row with no date last. Any other bar, line or area is drawn in the order of its x. A line or an area of dates leaves a gap at a period with no rows: a period of its grain, or with no grain a day, when most of its dates are a day apart. |
| Horizontal bar | **Categories** and a measure; optionally a series, which it can stack. Its **Default** order is a bar's. |
| Pie, donut | **Slices** (a column whose values are the slices) and a count or a sum. The 7 largest are drawn, largest first, then a grey **Other**. A slice of zero or below is not drawn, and the chart says how many. |
| Waterfall | x (the steps) and a count or a sum. Each bar starts where the one before ended, coloured as an **Increase** or a **Decrease**, and a grey **Total** bar ends it. A chart that reached the row limit has no Total, and it says so. |
| Funnel | **Stages** and a measure, largest first. Each band starts as wide as its stage's value and narrows to the next stage's width. Where a stage is larger than the one before (a funnel ordered by its stages' names, say), each band is drawn as wide as its own value instead, so none widens to the next; its numbers, labels and tooltips are each stage's own. A label is the stage's name, its value and its share of the first stage; where a name has no room (a phone's width), it is cut short with "…" and the numbers are kept. Names are measured in the page's own font. |
| Histogram | One number column (**Column to count in bins**), its rows counted in 20 bins, or as many as you type (1 to 100), equal parts of its least to its greatest value. Or type a **bin width** instead: bins that wide, counted from zero. A bin reads "10–20". Where there are more bins than fit the chart's width at 3 pixels a bar, neighbours are added up into one bar named for the range they cover, and the chart says how many each bar holds; the table still lists every bin. |
| Scatter | Two number columns, x and y: a point for each place rows fall, so rows with the same x and y (and size and colour) are one point, and its tooltip says how many rows it holds. Optionally a text (or true-or-false) column to colour the points by: the 7 colours with the most rows, then a grey **Other**, counted over every row that makes a point. A row with no x or no y has no point, nor, when the points are sized, one whose size is not above zero. |
| Bubble | A scatter whose points are sized by a third number column. A row with no size, or a size of zero or below, has no bubble, and it never takes the place of one drawn under the 10,000-point limit. Its axes reach past the points by half the largest bubble, so a bubble at an edge stays clear of the axis and its numbers. |
| Heatmap | x, y and a measure. Its legend is five steps of the measure and the values each covers, or one entry when every cell holds the same value. Each y name is written down the side, smaller where the rows are narrow; where even that is too small, every so many rows are named, evenly (a horizontal bar's categories too). |
| Scorecard | One measure. **Compare with the period before** by a date column: the last 1, 7, 30, 90 or 365 days against the same span before them. Over more than a day it can **Draw the last *n* days as a line under the number**: a point a day over 7 or 30 days, a week over 90, a month over 365. A period with no rows is a break in the line. The line is drawn on the page and in the PDF. |
| Table | Up to 20 columns. The rows are in the same order each time: by the sort you pick, then by each column in turn, so a table that reaches the row limit shows its first rows in that order. When your database cannot sort one of the columns (a list or a JSON value), or the sorted read runs out of time, the rows are in the order of the sort you picked alone, or your database's own when you picked none, and the table says so. |
| Pivot table | 1 to 3 **row** fields, up to 2 **column** fields (each by its values or by a date grain) and 1 to 3 measures. Each group of rows and columns has a **Total**, and the table a grand total, in bold. Every total is your database's own: an average's total is the average over its rows, not an average of the averages shown. |
| Treemap, sunburst | 1 to 3 **Levels, outermost first**, and a count or a sum: a rectangle, or a ring's slice, a part, as large as its measure. The largest 3,500 parts are drawn. A part of zero or below is not drawn, and the chart says how many. The 7 largest top-level groups, by their drawn parts, each take a colour. Every other group's parts are drawn as they are, in grey, under one grey **Other** in the legend: not added up into one part, as a pie's Other is. A part's name is written where it fits whole, or as its first letters and "…"; a smaller part shows its name in its tooltip. A treemap's group is outlined, and named along its top, only where it is large enough. A part under a pixel at the size the chart is drawn is too small to see, and the chart says how many there are; its group's value still counts it. |
| Sankey | **From** and **To** columns and a count or a sum: a band a flow, as wide as its measure, the largest first, between at most 100 names. A flow is not drawn, and the chart says how many, when it goes from a name to itself, would close a loop (a sankey's flows only go forward), or reaches past the 100th name. |
| Calendar heatmap | A date column (**Day**) and a measure: a square a day, each week a column from Monday to Sunday, over the latest 366 days. |
| Bullet | A measure and a **Target**: **A number** you type, or **A measure** (the **Target measure**). One bar as long as the number, a mark across it at the target; the tooltip says how much of the target it is. |
| Box plot | **Groups** (x) and **Values**, a number column: each group's least value, quartiles and greatest. Not offered on MySQL, MariaDB or SQL Server, which have no exact percentile to give it; hover the greyed-out type for why. On BigQuery and ClickHouse the quartiles are your database's estimate, and the chart says they are approximate. |
| Candlestick | A **Date** column and four number columns, **Open**, **High**, **Low** and **Close**, by a date grain: each period opens at its first row's open and closes at its last row's close, by the date; its high and low are its greatest and least. |
| Range bands | x and two measures, **Low** and **High**: the band between them. The tooltip gives each point's low and high. A band of dates breaks at a period with no rows, as a line does. |
| Choropleth map | **Shapes**, **World countries** or **US states**, a column of codes (**Country codes** or **US state codes**) and a measure: each row fills one shape, in five shades of one colour from the lowest value to the highest. A country is read by its ISO code (FR, FRA or 250) and a state by its ISO code (US-CA or CA). A name such as "France" is not read, and nothing is looked up outside rsync.ai: the shapes ship with it. A row whose code names no shape, or a shape another row already fills, is not drawn, and the chart says how many. A masked column is not offered for the codes. |

**A second measure** turns a bar, line or area chart into a combo: each measure is drawn
**As bars**, **As a line** or **As an area**, on the **Left axis** or the **Right axis**.
Two measures at most. With one on each side, each axis is titled with its measure, so it is
clear which one reads against which scale. A chart with a second measure has no series.

rsync.ai writes the SQL from these choices, so the grouping runs on your database and a
chart is one read-only statement. The settings are checked when you save, and again each
time the chart is drawn: a column the source no longer returns, an aggregate not in the
list, a colour that is not one of the seven series colours, or settings over 8 KB are
refused, with the reason.

A column of the wrong kind is refused too, with the reason. A histogram's column, a
scatter's or a bubble's x, y and size, a box plot's values and a candlestick's **Open**,
**High**, **Low** and **Close** need numbers. A scatter's or a bubble's colour needs
text, or true or false. A map's codes can be text or numbers. A sum or an average needs
a number column, and so does a min or a max, except in a table or a pivot table: there a
min or a max of dates or text is shown as the column holds it. A calendar's **Day**, a
candlestick's **Date**, a scorecard's date column and a column grouped by a date grain
(**By month**, say) need dates. A date held as a number (20261001) is tried. So is a date
held as text, on a calendar or grouped by a grain. Either is refused only when your database cannot read it as a date. On Postgres, MySQL, SQL Server and Databricks a column's kind is the type your database declares, so digits held as text are text; on other databases it is read from the values.

**Weeks and fiscal years.** A week starts on Monday and a fiscal year in January unless the
source's connection says otherwise: **Calendar for charts** on the connection's page sets
**Weeks start on** and **Fiscal year starts in** for every chart over it.

**Series colours.** A chart draws up to 7 series, each in a fixed colour. More series
are drawn as the 7 largest plus a grey **Other** when the measure can be combined (count,
sum, min, max). An average or a distinct count cannot be added up, so the rest are left
out and the chart says how many. Your database picks the 7 and adds up Other over every
row, not only the rows the chart reads. On MySQL this needs 8.0 or later, and on MariaDB
10.2 or later. A treemap's and a sunburst's colours are their top-level groups', picked
from the parts drawn (above).

## Make a dashboard

**Analyze > Dashboards > New dashboard**: add saved charts and text, drag a tile by its
handle, and add up to 4 filters. A dashboard has at most 30 tiles. **Edit** on an open
dashboard changes it; **Save dashboard** saves the whole page.

**Sizing a tile:** drag its right edge, its bottom edge or its corner; the 12 columns show
while it moves. Or pick a width from the ↔ button in its header: full, ¾, ½, ⅓ or ¼ of the
row. The tiles in its way move down. A new chart is a third of the row wide and goes into
the first space it fits.

**Filters:**

- **Date range** filters a date column to the days you pick; it applies once both days
  are picked.
- **List of values** filters a column to the values you tick. The list is read live when
  you open it, up to 200 values; type any other value. A masked column lists nothing:
  type the values instead.
- A filter applies to each chart whose source has its column; any other chart says it
  is not filtered by it.
- Values you pick are sent as bound parameters, never pasted into the SQL. A connection
  that cannot take bound values is drawn unfiltered, under a warning.

## What a tile tells you

Every tile is read live when the dashboard opens: nothing is copied or cached. A chart
reads at most 10,000 rows; one that reaches it says "First 10,000 rows drawn". A scatter
or bubble chart is 10,000 points, those holding the most rows; one that reaches it says
so. A pivot table is 10,000 cells and totals, its totals read first; one that reaches it
says each total still counts every row. When the totals alone reach it, no cell is shown
and it says a total may be missing. A value past the limit reads "…"; a blank cell has no rows. A heatmap
that reaches the limit says its last column may have values its blank cells do not show. A treemap or a
sunburst is its largest 3,500 parts, and a calendar its latest 366 days; each says so when
the source has more.

Above a chart on a **model**:

- **Data as of**: when the rows were current, from the CDC position its last good run read,
  or when that run finished.
- **On time** or **Late**, when the model has a freshness deadline.
- **Checks n/m passed**: its data checks on that run; hover for each one.
- **Lineage**: the model on the lineage graph.

A chart on a **saved query** says when it was read. A failed last run, a late model or a
run a data check stopped is a warning above the chart; the chart is still drawn from the
rows there are.

On a short tile the badges and warnings scroll, from their first line; the number or the
chart below them stays in view.

## Draft one with Ask rsync.ai

Ask, for example, "a sales dashboard from the daily orders query, with a date filter". A
card under the answer lists the charts and filters it drafted, with **Preview**, **Who
sees it** and **Save dashboard**. Nothing is saved until an owner or admin selects Save;
an unsaved draft is kept for 24 hours. The model sees the sources' column names and kinds,
never a row. → [assistant.md § Draft a dashboard](../api/assistant.md#draft-a-dashboard)

An MCP client can draft one too, with the `plan_dashboard` tool and a token that has the
`dashboards:write` scope → [agent-gateway.md](../api/agent-gateway.md).

## Change a tile with Ask rsync.ai

On an open dashboard, ask about it, for example "make the orders chart a line" or "make
the first chart full width". A card under the answer shows the tile now and after the
change, row by row, with what changes marked, and the tiles that move down to make room.

- One change is one tile: its chart's type, columns, measure, sort, colour and title, and
  the tile's width and height. For two tiles, ask for two changes.
- The changed chart gets the same checks as a chart you save yourself, and its query runs
  once, as you, to check it. The model sees the tiles' places, the charts' names and
  settings and the sources' columns, never a row, a series colour or a text tile's text.
- **Apply** changes the dashboard. A chart only on this dashboard is changed in place. A
  chart on another dashboard too is copied for this tile, so the other dashboard keeps
  its chart; the card says which before you apply.
- Nothing changes until an owner or admin selects **Apply**. A change is kept for 24
  hours. If the dashboard or the chart was saved after the change was drafted, the card
  says it is out of date and offers no Apply: ask again.
- The link the assistant gives opens the dashboard with the card above the tiles.

An MCP client can do the same with `list_dashboards`, `get_dashboard` and
`plan_dashboard_change`; proposing needs the `dashboards:write` scope →
[agent-gateway.md](../api/agent-gateway.md).

## History

**History** on an open dashboard lists its versions, newest first. Every save keeps the
dashboard as it was before it, with who saved and how: an edit, a change from Ask
rsync.ai, or a restore. Select a version to see its name, filters and tiles.

- **Restore this version** asks first, then saves that version as the dashboard. The
  dashboard as it is now is kept as the next version, so a restore never deletes one.
- A restore puts back the dashboard's name, filters and tiles; each chart is drawn with
  its settings as they are now, and who sees the dashboard stays as it is.
- If someone saved the dashboard after you opened the History, the restore is refused and
  the list reloads.
- Versions are kept as long as the dashboard is, and hold no rows.

## Export and email

- **Export:** a chart as PNG or SVG (drawn in the browser), a table or pivot table tile as
  CSV (the rows on screen), a dashboard as an A4 landscape PDF on its 12-column grid with the filters you
  picked (made on the frontend's server, as you).
- **Email a chart or a board:** a workflow's `chart` step puts a saved chart in the sends
  after it (in email a plotted chart as a picture, a scorecard, table or pivot table as text, with no sparkline; its numbers
  as text in Slack), and a `dashboard` step puts every tile in (the tiles in email with the
  board's PDF attached; its scorecards' numbers and a link in Slack). Both read as the run's user. A board's **Schedule email** button (owners and
  admins, where Ask rsync.ai is on) opens Ask rsync.ai to draft that workflow, which a
  person saves. See workflows-design.md § 3.1
  and dashboards-design.md § 8.

## Routes

Session routes; every one is held to the active workspace.

| Route | Role |
|---|---|
| `GET /api/v1/charts/sources`, `GET /api/v1/charts/sources/:id/columns`, `POST /api/v1/charts/preview` | viewer |
| `GET /api/v1/charts`, `GET /api/v1/charts/:id`, `POST /api/v1/charts/:id/draw` | viewer |
| `POST /api/v1/charts`, `PUT /api/v1/charts/:id`, `DELETE /api/v1/charts/:id` | admin |
| `GET /api/v1/dashboards`, `GET /api/v1/dashboards/:id` | viewer |
| `POST /api/v1/dashboards/:id/tiles/:tileId/draw`, `GET /api/v1/dashboards/:id/filters/:filterId/values` | viewer |
| `POST /api/v1/dashboards`, `PUT /api/v1/dashboards/:id`, `DELETE /api/v1/dashboards/:id` | admin |
| `GET /api/v1/dashboard-proposals/:id`, `POST /api/v1/dashboard-proposals/:id/save` | the person who drafted it; save needs admin |
| `GET /api/v1/dashboard-changes/:id`, `POST /api/v1/dashboard-changes/:id/apply` | the person who drafted it; apply needs admin |
| `GET /api/v1/dashboards/:id/versions` | viewer, on a dashboard they may see |
| `POST /proxy/dashboards/:id/pdf` (frontend; the filters picked go in the body) | viewer, on a dashboard they may see |

A restore is `PUT /api/v1/dashboards/:id` with the version's name, description, filters
and tiles, the dashboard's `updated_at` and `restored_from: <version>`. A version the
dashboard does not have answers 422; a save since the `updated_at` answers 409.
