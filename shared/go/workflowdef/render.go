package workflowdef

import (
	"fmt"
	"html"
	"regexp"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"
)

// A message template is plain text with {{name}} slots and nothing else: no
// loops, no conditions, no functions, no filters. A name is a fixed variable
// (fixedPlaceholders) or a column of the query's first row. Logic belongs in
// the SQL, which the warehouse runs; a template that can only fill slots
// cannot be made to run anything.

// Format is where a rendered message goes.
type Format int

const (
	// FormatSlack escapes & < > (so a value cannot ping a channel or forge a
	// link) and draws the table in a code block.
	FormatSlack Format = iota
	// FormatText is a plain-text email.
	FormatText
	// FormatHTML is an HTML email: everything escaped, the table as <table>.
	FormatHTML
)

var fixedPlaceholders = []string{"workflow_name", "row_count", "value", "table", "run_time", "link"}

var (
	placeholderRe = regexp.MustCompile(`\{\{\s*([^{}]*?)\s*\}\}`)
	nameRe        = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_]{0,62}$`)
)

// UsesSlot reports whether text has the {{name}} slot.
func UsesSlot(text, name string) bool {
	for _, m := range placeholderRe.FindAllStringSubmatch(text, -1) {
		if m[1] == name {
			return true
		}
	}
	return false
}

// badPlaceholders lists the {{…}} in text that are not a bare name.
func badPlaceholders(text string) []string {
	var bad []string
	for _, m := range placeholderRe.FindAllStringSubmatch(text, -1) {
		if !nameRe.MatchString(m[1]) {
			bad = append(bad, m[0])
		}
	}
	return bad
}

// Vars is what a run fills a template with.
type Vars struct {
	WorkflowName string
	RunTime      time.Time
	Link         string
	Result       Result
	// Value is the number a value condition compared. When nil, {{value}} is
	// the first column of the first row.
	Value *float64
}

// Table limits per format. Slack's are tight because a message over about
// 3,000 characters is cut.
const (
	tableCellMax      = 40
	tableColumnsMax   = 8
	slackTableRows    = 20
	slackTableChars   = 2000
	textTableRows     = 50
	htmlTableRows     = 100
	htmlTableColumns  = 12
	SlackMessageMax   = 3000
	EmailSubjectMax   = 200
	EmailBodyCharsMax = 100_000
)

// Render fills tmpl for format f. unknown lists each {{name}} that is neither a
// fixed variable nor a column of the result; it renders as nothing, and the
// run records a note so the person can fix the template.
func Render(tmpl string, v Vars, f Format) (string, []string) {
	var (
		b       strings.Builder
		unknown []string
		last    int
	)
	for _, loc := range placeholderRe.FindAllStringSubmatchIndex(tmpl, -1) {
		b.WriteString(escapeLiteral(tmpl[last:loc[0]], f))
		last = loc[1]
		name := tmpl[loc[2]:loc[3]]
		if name == "table" {
			b.WriteString(renderTable(v.Result, f))
			continue
		}
		val, ok := lookup(name, v)
		if !ok {
			unknown = append(unknown, "{{"+name+"}}")
			continue
		}
		b.WriteString(escapeValue(val, f))
	}
	b.WriteString(escapeLiteral(tmpl[last:], f))
	return b.String(), unknown
}

// RenderSubject renders a one-line subject: no table, whitespace collapsed.
func RenderSubject(tmpl string, v Vars) (string, []string) {
	tmpl = placeholderRe.ReplaceAllStringFunc(tmpl, func(m string) string {
		if placeholderRe.FindStringSubmatch(m)[1] == "table" {
			return ""
		}
		return m
	})
	out, unknown := Render(tmpl, v, FormatText)
	return CapRunes(strings.Join(strings.Fields(out), " "), EmailSubjectMax), unknown
}

func lookup(name string, v Vars) (string, bool) {
	switch name {
	case "workflow_name":
		return v.WorkflowName, true
	case "row_count":
		n := readableNumber(strconv.Itoa(v.Result.RowCount))
		if v.Result.Truncated {
			n += "+"
		}
		return n, true
	case "run_time":
		return v.RunTime.UTC().Format("2006-01-02 15:04 UTC"), true
	case "link":
		return v.Link, true
	case "value":
		if v.Value != nil {
			return readableNumber(strconv.FormatFloat(*v.Value, 'f', -1, 64)), true
		}
		if len(v.Result.Rows) > 0 && len(v.Result.Columns) > 0 {
			c := v.Result.Columns[0]
			return messageCell(v.Result.Rows[0][c], c), true
		}
		return "", true
	}
	col, ok := resolveColumn(name, v.Result)
	if !ok {
		return "", false
	}
	if len(v.Result.Rows) == 0 {
		return "", true
	}
	return messageCell(v.Result.Rows[0][col], col), true
}

// identifierColumnRe matches a column named as an id, a code, a phone or a
// year (customer_id, zip, fiscal_year, customerId): its digits are a label.
var (
	identifierColumnRe = regexp.MustCompile(`(?i)(^|_)(id|code|zip|postcode|phone|number|no|year|yr)$`)
	camelIDColumnRe    = regexp.MustCompile(`[a-z0-9]I[Dd]$`)
)

// messageCell is a cell as a message shows it: a number gets its thousands
// grouped (22803 reads 22,803), unless its column is an id, code or year.
func messageCell(v any, column string) string {
	s := cellText(v)
	if _, ok := asNumber(v); !ok || identifierColumnRe.MatchString(column) || camelIDColumnRe.MatchString(column) {
		return s
	}
	return readableNumber(strings.TrimSpace(s))
}

// readableNumber puts a comma between each three digits of a plain decimal's
// whole part: 1000000000.5 reads 1,000,000,000.5. Text that is not a plain
// decimal, or whose whole part starts with 0, is left as it is.
func readableNumber(s string) string {
	sign, digits := "", s
	if strings.HasPrefix(digits, "-") {
		sign, digits = "-", digits[1:]
	}
	whole, frac, dot := strings.Cut(digits, ".")
	plain := func(d string) bool { return d != "" && strings.Trim(d, "0123456789") == "" }
	if len(whole) < 4 || whole[0] == '0' || !plain(whole) || (dot && !plain(frac)) {
		return s
	}
	var b strings.Builder
	b.WriteString(sign)
	for i := range whole {
		if i > 0 && (len(whole)-i)%3 == 0 {
			b.WriteByte(',')
		}
		b.WriteByte(whole[i])
	}
	if dot {
		b.WriteString("." + frac)
	}
	return b.String()
}

func escapeLiteral(s string, f Format) string {
	switch f {
	case FormatSlack:
		return slackEscape(s)
	case FormatHTML:
		return strings.ReplaceAll(html.EscapeString(s), "\n", "<br>\n")
	default:
		return s
	}
}

func escapeValue(s string, f Format) string {
	switch f {
	case FormatSlack:
		return slackEscape(s)
	case FormatHTML:
		return html.EscapeString(s)
	default:
		return s
	}
}

// slackEscape escapes the three characters Slack reads as markup (the same
// rule as notifier.slackEscape), so <!channel> cannot ping and <url|text>
// cannot become a link.
func slackEscape(s string) string {
	r := strings.NewReplacer("&", "&amp;", "<", "&lt;", ">", "&gt;")
	return r.Replace(s)
}

// CapRunes cuts s to at most max characters, the last of them "…".
func CapRunes(s string, max int) string {
	if max <= 0 || utf8.RuneCountInString(s) <= max {
		return s
	}
	return string([]rune(s)[:max-1]) + "…"
}

func renderTable(r Result, f Format) string {
	if len(r.Columns) == 0 {
		return ""
	}
	if len(r.Rows) == 0 {
		return escapeLiteral("(no rows)", f)
	}
	if f == FormatHTML {
		return htmlTable(r)
	}
	maxRows := textTableRows
	if f == FormatSlack {
		maxRows = slackTableRows
	}
	cols := r.Columns
	if len(cols) > tableColumnsMax {
		cols = cols[:tableColumnsMax]
	}
	cells := func(row map[string]any) []string {
		out := make([]string, len(cols))
		for i, c := range cols {
			out[i] = CapRunes(strings.Join(strings.Fields(messageCell(row[c], c)), " "), tableCellMax)
		}
		return out
	}
	widths := make([]int, len(cols))
	for i, c := range cols {
		widths[i] = utf8.RuneCountInString(CapRunes(c, tableCellMax))
	}
	shown := r.Rows
	if len(shown) > maxRows {
		shown = shown[:maxRows]
	}
	grid := make([][]string, 0, len(shown))
	for _, row := range shown {
		cs := cells(row)
		for i, s := range cs {
			if n := utf8.RuneCountInString(s); n > widths[i] {
				widths[i] = n
			}
		}
		grid = append(grid, cs)
	}
	line := func(cs []string) string {
		parts := make([]string, len(cs))
		for i, s := range cs {
			parts[i] = s + strings.Repeat(" ", widths[i]-utf8.RuneCountInString(s))
		}
		return strings.TrimRight(strings.Join(parts, "  "), " ")
	}
	header := make([]string, len(cols))
	rule := make([]string, len(cols))
	for i, c := range cols {
		header[i] = CapRunes(c, tableCellMax)
		rule[i] = strings.Repeat("-", widths[i])
	}
	var b strings.Builder
	b.WriteString(line(header) + "\n" + line(rule) + "\n")
	written := 0
	for _, cs := range grid {
		l := line(cs) + "\n"
		if f == FormatSlack && b.Len()+len(l) > slackTableChars {
			break
		}
		b.WriteString(l)
		written++
	}
	body := strings.TrimRight(b.String(), "\n")
	var more []string
	if rest := r.RowCount - written; rest > 0 || r.Truncated {
		if r.Truncated {
			more = append(more, "more rows not shown")
		} else {
			more = append(more, fmt.Sprintf("%d more row(s) not shown", rest))
		}
	}
	if len(r.Columns) > len(cols) {
		more = append(more, fmt.Sprintf("%d more column(s) not shown", len(r.Columns)-len(cols)))
	}
	if f == FormatSlack {
		out := "```\n" + slackEscape(body) + "\n```"
		if len(more) > 0 {
			out += "\n_" + strings.Join(more, "; ") + "_"
		}
		return out
	}
	if len(more) > 0 {
		body += "\n(" + strings.Join(more, "; ") + ")"
	}
	return body
}

func htmlTable(r Result) string {
	cols := r.Columns
	if len(cols) > htmlTableColumns {
		cols = cols[:htmlTableColumns]
	}
	rows := r.Rows
	if len(rows) > htmlTableRows {
		rows = rows[:htmlTableRows]
	}
	var b strings.Builder
	b.WriteString(`<table style="border-collapse:collapse;font-family:sans-serif;font-size:13px">`)
	b.WriteString("<tr>")
	for _, c := range cols {
		b.WriteString(`<th style="border:1px solid #ccc;padding:4px 8px;text-align:left;background:#f4f4f5">`)
		b.WriteString(html.EscapeString(c))
		b.WriteString("</th>")
	}
	b.WriteString("</tr>")
	for _, row := range rows {
		b.WriteString("<tr>")
		for _, c := range cols {
			b.WriteString(`<td style="border:1px solid #ccc;padding:4px 8px">`)
			b.WriteString(html.EscapeString(CapRunes(messageCell(row[c], c), 200)))
			b.WriteString("</td>")
		}
		b.WriteString("</tr>")
	}
	b.WriteString("</table>")
	var more []string
	if rest := r.RowCount - len(rows); rest > 0 && !r.Truncated {
		more = append(more, fmt.Sprintf("%d more row(s) not shown", rest))
	} else if r.Truncated {
		more = append(more, "more rows not shown")
	}
	if len(r.Columns) > len(cols) {
		more = append(more, fmt.Sprintf("%d more column(s) not shown", len(r.Columns)-len(cols)))
	}
	if len(more) > 0 {
		b.WriteString("<p><em>" + html.EscapeString(strings.Join(more, "; ")) + "</em></p>")
	}
	return b.String()
}
