package agentgw

import (
	"fmt"
	"math"
	"net/http"
	"strconv"
	"strings"

	"api-gateway/internal/services"

	"github.com/gin-gonic/gin"
	"github.com/rsync-ai/backend-orchestrator/pkg/llmscrub"
)

// The model check tools: list a model's data checks and draft new ones. A check
// runs at every run of the model; set to block, a failing one keeps the new build
// from replacing the live table (handlers/saved_query_checks.go). Because a
// blocking check can hold a production table back, no tool saves one:
// plan_model_checks judges a draft as the Checks tab would save it, and the person
// adds the checks there.
//
// No output carries a check's SQL, a count or a result: a check is its name, type,
// columns and severity. Whether a check passed stays out of these tools; a blocked
// run reaches the agent only as a failed run in list_model_runs.

const (
	// maxCheckColumnsArg fits unique's 10 column names, comma-separated.
	maxCheckColumnsArg = 10 * 130
	// maxCheckSQL is the handler's own cap on a custom_sql check.
	maxCheckSQL = 20000
	// maxColumnsNamed caps the target's columns a refusal lists.
	maxColumnsNamed = 60
)

var (
	modelCheckTypes    = []string{"row_count", "row_count_change", "not_null", "unique", "custom_sql"}
	modelCheckTypeSyns = map[string]string{
		"count": "row_count", "rows": "row_count", "min_rows": "row_count", "not_empty": "row_count",
		"row_change": "row_count_change", "volume": "row_count_change", "count_change": "row_count_change",
		"row_count_drop": "row_count_change",
		"notnull":        "not_null", "required": "not_null", "no_nulls": "not_null", "non_null": "not_null",
		"distinct": "unique", "uniqueness": "unique", "no_duplicates": "unique",
		"sql": "custom_sql", "custom": "custom_sql", "query": "custom_sql", "assertion": "custom_sql",
	}
	checkSeverities   = []string{"block", "warn"}
	checkSeveritySyns = map[string]string{
		"blocking": "block", "error": "block", "fail": "block", "hard": "block",
		"warning": "warn", "soft": "warn", "alert": "warn",
	}
)

// ---- outputs ----------------------------------------------------------------

type modelCheckOut struct {
	CheckID  string   `json:"check_id"`
	Name     string   `json:"name"`
	Type     string   `json:"type"`
	Columns  []string `json:"columns"`
	Severity string   `json:"severity"`
	Enabled  bool     `json:"enabled"`
}

type listModelChecksOut struct {
	ModelID   string          `json:"model_id"`
	Checks    []modelCheckOut `json:"checks"`
	MaxChecks int             `json:"max_checks"`
	// CanBlock says the model's engine can hold its table back. When false, every
	// check only warns; CannotBlockReason says why, or UnsupportedReason says why the
	// model cannot have checks at all.
	CanBlock          bool   `json:"can_block"`
	CannotBlockReason string `json:"cannot_block_reason,omitempty"`
	UnsupportedReason string `json:"unsupported_reason,omitempty"`
	Note              string `json:"note,omitempty"`
}

type draftCheckOut struct {
	Name     string   `json:"name"`
	Type     string   `json:"type"`
	Columns  []string `json:"columns"`
	Severity string   `json:"severity"`
	Verdict  string   `json:"verdict"`
}

type modelChecksDraftOut struct {
	ModelID     string          `json:"model_id"`
	TargetTable string          `json:"target_table"`
	Checks      []draftCheckOut `json:"checks"`
	// ExistingChecks is how many checks the model has; with these it may have at
	// most MaxChecks.
	ExistingChecks    int    `json:"existing_checks"`
	MaxChecks         int    `json:"max_checks"`
	CanBlock          bool   `json:"can_block"`
	CannotBlockReason string `json:"cannot_block_reason,omitempty"`
	// ColumnsUnchecked says the columns could not be looked up on the target table,
	// with why in Note.
	ColumnsUnchecked bool   `json:"columns_unchecked,omitempty"`
	Note             string `json:"note,omitempty"`
	Next             string `json:"next"`
}

// ---- wire shapes --------------------------------------------------------------

type wireModelChecks struct {
	Checks []struct {
		ID       string   `json:"check_id"`
		Name     string   `json:"name"`
		Type     string   `json:"type"`
		Columns  []string `json:"columns"`
		Severity string   `json:"severity"`
		Enabled  bool     `json:"enabled"`
	} `json:"checks"`
	MaxChecks         int    `json:"max_checks"`
	CanBlock          bool   `json:"can_block"`
	CannotBlockReason string `json:"cannot_block_reason"`
	UnsupportedReason string `json:"unsupported_reason"`
}

// wireChecksPlan is handlers.modelChecksPlan.
type wireChecksPlan struct {
	ConnectionID         string `json:"connection_id"`
	TargetSchema         string `json:"target_schema"`
	TargetTable          string `json:"target_table"`
	ColumnsCaseSensitive bool   `json:"columns_case_sensitive"`
	CanBlock             bool   `json:"can_block"`
	CannotBlockReason    string `json:"cannot_block_reason"`
	Existing             int    `json:"existing"`
	MaxChecks            int    `json:"max_checks"`
	Checks               []struct {
		Name     string   `json:"name"`
		Type     string   `json:"type"`
		Columns  []string `json:"columns"`
		Severity string   `json:"severity"`
		Refusal  string   `json:"refusal"`
	} `json:"checks"`
}

type wireTableColumns struct {
	Tables []struct {
		Name    string `json:"name"`
		Schema  string `json:"schema"`
		Columns []struct {
			Name string `json:"name"`
		} `json:"columns"`
	} `json:"tables"`
}

// ---- registry ---------------------------------------------------------------

// modelCheckArgs are the fields of one drafted check. The optional numbers are
// strings: an integer argument left out reads as its default, and a check must
// tell "no max" from a max of 0.
func modelCheckArgs() []Arg {
	return []Arg{
		{Name: "name", Type: "string", Required: true, MaxLength: 100,
			Description: "A short name the person reads, unique on the model, such as order_id is unique."},
		{Name: "type", Type: "string", Required: true, Enum: modelCheckTypes, Synonyms: modelCheckTypeSyns,
			Description: "row_count (min to max rows), row_count_change (moved more than max_drop_pct since the last good " +
				"run), not_null, unique (NULLs ignored) or custom_sql."},
		{Name: "columns", Type: "string", MaxLength: maxCheckColumnsArg, Raw: true,
			Description: "Comma-separated column names of the model's table: one for not_null, 1 to 10 for unique."},
		{Name: "min", Type: "string", MaxLength: 20,
			Description: "For row_count: the fewest rows, a whole number such as \"1\". Default 1."},
		{Name: "max", Type: "string", MaxLength: 20,
			Description: "For row_count: the most rows, a whole number. Default no limit."},
		{Name: "max_drop_pct", Type: "string", MaxLength: 20,
			Description: "For row_count_change, required: the largest drop in percent (0 to 100), such as \"50\"."},
		{Name: "max_growth_pct", Type: "string", MaxLength: 20,
			Description: "For row_count_change: the largest growth in percent, such as \"200\". Default no limit."},
		{Name: "sql", Type: "string", MaxLength: maxCheckSQL, Raw: true,
			Description: "For custom_sql: a read-only SELECT of rule-breaking rows, {{this}} for the model's table, such " +
				"as SELECT * FROM {{this}} WHERE amount < 0. Any row fails it."},
		{Name: "severity", Type: "string", Enum: checkSeverities, Synonyms: checkSeveritySyns,
			Description: "block keeps a failing build from replacing the live table; warn publishes it and alerts. Default " +
				"block; warn for row_count_change and engines that cannot block."},
	}
}

func modelCheckTools(h Handlers) []*Tool {
	return []*Tool{
		{
			Name:  "list_model_checks",
			Title: "List a model's checks",
			Description: "Lists a model's data checks: each one's name, type, columns, severity and whether it is on, and " +
				"whether the model's engine can block at all. Never returns a check's SQL, a count or a result. " +
				"Rules: read_guide('models').",
			Guide:        "models",
			Args:         []Arg{argModelID},
			Action:       services.ActionModelRead,
			ResourceType: "model",
			ResourceArg:  "model_id",
			Output:       listModelChecksOut{},
			run: func(call *toolCall) (any, error) {
				return listModelChecks(call, h.ListModelChecks)
			},
		},
		{
			Name:  "plan_model_checks",
			Title: "Draft a model's checks",
			Description: "Drafts data checks for a model and validates them as a save would: types, settings, names, at " +
				"most 20 a model, a custom_sql that only reads, and columns against the model's table. It saves " +
				"and runs nothing. Rules: read_guide('models').",
			Guide: "models",
			Args: []Arg{
				argModelID,
				{Name: "checks", Type: "array", Required: true, MaxItems: 20, Items: modelCheckArgs(),
					Description: "The checks to add, at most 20."},
			},
			Action:       services.ActionModelRead,
			ResourceType: "model",
			ResourceArg:  "model_id",
			Output:       modelChecksDraftOut{},
			run: func(call *toolCall) (any, error) {
				return planModelChecks(call, h)
			},
		},
	}
}

// ---- runs -------------------------------------------------------------------

func listModelChecks(call *toolCall, h gin.HandlerFunc) (any, error) {
	id := call.str("model_id")
	var body wireModelChecks
	if err := call.get(h, gin.Params{{Key: "id", Value: id}}, nil, &body); err != nil {
		return nil, err
	}
	out := listModelChecksOut{
		ModelID:           id,
		Checks:            []modelCheckOut{},
		MaxChecks:         body.MaxChecks,
		CanBlock:          body.CanBlock,
		CannotBlockReason: cleanText(body.CannotBlockReason),
		UnsupportedReason: cleanText(body.UnsupportedReason),
	}
	for _, c := range body.Checks {
		out.Checks = append(out.Checks, modelCheckOut{
			CheckID:  ident(c.ID),
			Name:     cleanText(c.Name),
			Type:     ident(c.Type),
			Columns:  cleanList(c.Columns),
			Severity: ident(c.Severity),
			Enabled:  c.Enabled,
		})
	}
	if len(out.Checks) == 0 && out.UnsupportedReason == "" {
		out.Note = "This model has no checks. Draft some with plan_model_checks."
	}
	return out, nil
}

func planModelChecks(call *toolCall, h Handlers) (any, error) {
	items := call.items("checks")
	inputs := make([]map[string]any, 0, len(items))
	for i, it := range items {
		in, err := checkInput(it, fmt.Sprintf("checks[%d].", i))
		if err != nil {
			return nil, err
		}
		inputs = append(inputs, in)
	}
	id := call.str("model_id")
	var plan wireChecksPlan
	if err := call.do(http.MethodPost, h.PlanModelChecks, gin.Params{{Key: "id", Value: id}}, nil,
		map[string]any{"checks": inputs}, &plan); err != nil {
		return nil, explain(err)
	}
	if len(plan.Checks) != len(inputs) {
		return nil, &toolError{status: http.StatusInternalServerError, outcome: "error",
			message: "rsync.ai returned an answer this tool could not read."}
	}

	verdicts := make([]string, len(plan.Checks)) // "" is valid
	for i, p := range plan.Checks {
		verdicts[i] = p.Refusal
	}
	unchecked := checkColumnsOnTarget(call, h.GetSchemaIndex, plan, verdicts)

	refused := 0
	for _, v := range verdicts {
		if v != "" {
			refused++
		}
	}
	if refused > 0 {
		var b strings.Builder
		fmt.Fprintf(&b, "rsync.ai refused %d of the %d checks, so the draft is refused; nothing was saved. Fix "+
			"them and draft again.", refused, len(verdicts))
		for i, p := range plan.Checks {
			fmt.Fprintf(&b, " %d. %s: ", i+1, cleanText(p.Name))
			if verdicts[i] == "" {
				b.WriteString("valid.")
			} else {
				b.WriteString("refused: " + strings.TrimSuffix(llmscrub.Scrub(cleanText(verdicts[i])), ".") + ".")
			}
		}
		fmt.Fprintf(&b, " The model already has %d checks; a model has at most %d.", plan.Existing, plan.MaxChecks)
		return nil, &toolError{status: http.StatusBadRequest, outcome: "invalid_arguments", message: b.String()}
	}

	target := plan.TargetTable
	if plan.TargetSchema != "" {
		target = plan.TargetSchema + "." + target
	}
	out := modelChecksDraftOut{
		ModelID:           id,
		TargetTable:       cleanText(target),
		Checks:            make([]draftCheckOut, 0, len(plan.Checks)),
		ExistingChecks:    plan.Existing,
		MaxChecks:         plan.MaxChecks,
		CanBlock:          plan.CanBlock,
		CannotBlockReason: cleanText(plan.CannotBlockReason),
		ColumnsUnchecked:  unchecked != "",
		Note:              unchecked,
	}
	switch {
	case call.s.opts.Actor(call.c).Channel == services.ChannelAssistant:
		out.Next = "Nothing is saved yet. The person sees this draft as a card under your answer, opens it on the " +
			"model's Checks tab and adds each check there; Test now runs a check against the table. Tell them that; only they can."
	default:
		out.Next = "Nothing is saved yet. Tell the person to add these checks on the model's Checks tab in rsync.ai, " +
			"where Test now runs a check against the table. Only they can; no tool can."
	}
	for _, p := range plan.Checks {
		if p.Type == "custom_sql" {
			out.Next += " On a scheduled model, a custom_sql check waits for an admin's approval before it runs."
			break
		}
	}
	for _, p := range plan.Checks {
		out.Checks = append(out.Checks, draftCheckOut{Name: cleanText(p.Name), Type: ident(p.Type),
			Columns: cleanList(p.Columns), Severity: ident(p.Severity), Verdict: "valid"})
	}
	return out, nil
}

// checkInput is one drafted check as the handlers take it (modelCheckInput).
func checkInput(it map[string]any, path string) (map[string]any, error) {
	params := map[string]any{}
	for _, f := range []struct {
		name  string
		whole bool
	}{{"min", true}, {"max", true}, {"max_drop_pct", false}, {"max_growth_pct", false}} {
		s := strings.TrimSpace(itemStr(it, f.name))
		if s == "" {
			continue
		}
		if f.whole {
			n, err := strconv.ParseInt(s, 10, 64)
			if err != nil {
				return nil, argError("%s%s must be a whole number, such as 1", path, f.name)
			}
			params[f.name] = n
			continue
		}
		x, err := strconv.ParseFloat(strings.TrimSpace(strings.TrimSuffix(s, "%")), 64)
		if err != nil || math.IsNaN(x) || math.IsInf(x, 0) {
			return nil, argError("%s%s must be a number, such as 50", path, f.name)
		}
		params[f.name] = x
	}
	columns := []string{}
	for _, c := range strings.Split(itemStr(it, "columns"), ",") {
		if c = strings.TrimSpace(c); c != "" {
			columns = append(columns, c)
		}
	}
	return map[string]any{
		"name":     itemStr(it, "name"),
		"type":     itemStr(it, "type"),
		"columns":  columns,
		"params":   params,
		"sql_text": itemStr(it, "sql"),
		"severity": itemStr(it, "severity"),
	}, nil
}

// checkColumnsOnTarget refuses, in verdicts, a valid check naming a column the
// model's table does not have, read from the connection's schema index (what
// describe_schema reads). When it cannot look, it says why and leaves the checks
// as they are: the table is not in the index before the model's first run and a
// schema refresh, or the index cannot be read.
func checkColumnsOnTarget(call *toolCall, h gin.HandlerFunc, plan wireChecksPlan, verdicts []string) string {
	named := false
	for i, p := range plan.Checks {
		named = named || (verdicts[i] == "" && len(p.Columns) > 0)
	}
	if !named {
		return ""
	}
	const atRun = " A check on a column the table does not have fails with an error at the model's next run, " +
		"and a blocking one holds the table back."
	var idx wireTableColumns
	if plan.ConnectionID == "" ||
		call.get(h, gin.Params{{Key: "id", Value: plan.ConnectionID}}, nil, &idx) != nil {
		return "The columns are unchecked: rsync.ai could not read the connection's schema." + atRun
	}
	same := strings.EqualFold
	if plan.ColumnsCaseSensitive {
		same = func(a, b string) bool { return a == b }
	}
	found := -1
	for i, t := range idx.Tables {
		if !same(t.Name, plan.TargetTable) || (plan.TargetSchema != "" && !same(t.Schema, plan.TargetSchema)) {
			continue
		}
		if found >= 0 {
			return "The columns are unchecked: several schemas have a table named " + cleanText(plan.TargetTable) +
				"." + atRun
		}
		found = i
	}
	if found < 0 {
		return "The columns are unchecked: the model's table is not in the connection's schema yet. rsync.ai " +
			"creates it on the model's first run, and the schema shows it after a refresh." + atRun
	}
	cols := idx.Tables[found].Columns
	for i, p := range plan.Checks {
		if verdicts[i] != "" {
			continue
		}
	columns:
		for _, want := range p.Columns {
			for _, c := range cols {
				if same(c.Name, want) {
					continue columns
				}
			}
			names := make([]string, 0, len(cols))
			for _, c := range cols {
				if len(names) == maxColumnsNamed {
					names = append(names, "…")
					break
				}
				names = append(names, c.Name)
			}
			verdicts[i] = "the model's table has no column " + want + "; its columns are " + strings.Join(names, ", ")
			break
		}
	}
	return ""
}

func cleanList(in []string) []string {
	out := make([]string, 0, len(in))
	for _, s := range in {
		out = append(out, cleanText(s))
	}
	return out
}
