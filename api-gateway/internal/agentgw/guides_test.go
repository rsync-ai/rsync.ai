package agentgw

import (
	"encoding/json"
	"regexp"
	"slices"
	"strings"
	"testing"

	"api-gateway/internal/charts"
)

// The topic guides (docs/internal/assistant-token-design.md § 7.2): each holds
// the rules step.yaml and the tool descriptions used to say, in the same words.

func guideBody(topic string) string { return strings.Join(guides[topic], " ") }

// Each rule moved out of step.yaml keeps its wording in its guide, so a
// guide rewrite that drops one fails here, the way the prompt tests did.
func TestGuides_KeepTheMovedRules(t *testing.T) {
	moved := map[string][]string{
		"workflows": {
			"Use plan_workflow for reports, digests and alerts",
			"A workflow never moves data",
			"propose a pipeline instead",
			"A draft sends nothing",
			"rsync.ai runs the query once to check a draft",
			"call plan_workflow again rather than telling the user it failed",
			"the window runs before the LIMIT cuts",
			"Never sum or count a CTE or subquery that has a LIMIT",
			"never say the workflow is on, has run or has sent anything",
			"SELECT COUNT(*) AS total_users makes {{total_users}}",
			"add COUNT(*) OVER () AS total to the list query",
			"Put {{table}} where the rows go whenever the user wants to see rows",
			"{{row_count}} is only how many rows the query returned",
			"{{value}} is only the number a value condition compares",
			"{{workflow_name}}, {{run_time}} and {{link}}",
			"{{link}} is this workflow's page, never a dashboard",
			"as many conditions as the user asks for",
			"only the first that is true sends its own message to its own channels",
			"A workflow never joins two connections or runs a second query",
			"call list_workflows for its id and condition numbers, then plan_workflow_change",
			"a condition you leave out is removed",
			"You are not shown a workflow's SQL or messages",
			"A change does nothing until the user clicks Apply",
			"never say the workflow changed",
			"Before its query, a workflow may run steps",
			"run_pipeline starts a batch pipeline's run, refresh_model rebuilds a model, and wait pauses",
			"Each step runs after the steps its after names",
			"Take each pipeline_id from list_pipelines (a batch one) and each model_id from list_models; never guess",
			"all_success, the default, when every step it is after succeeded",
			"all_done when they all ended, either way; any_failed when one failed",
			"steps_rule says the same for the query",
			"leave out connection_id and sql and give one always condition",
			"may use only the {{workflow_name}}, {{run_time}} and {{link}} slots",
			"Only such a workflow may also draw saved charts into its message",
			"the chart_id of a chart tile from get_dashboard, never guessed",
			"use one dashboard step with the dashboard_id from list_dashboards, never guessed, not a chart step per tile",
			"attaches the dashboard as a PDF",
			"to change them, draft a new workflow with plan_workflow",
			"it can run a batch pipeline the workspace has as a step",
			"each box on the workflow's canvas a node",
		},
		"dashboards": {
			"call list_chart_sources, then describe_chart_source for each source you use, then plan_dashboard",
			"Name only columns describe_chart_source returned, never one you guessed",
			"belongs in a model first; when no source has it, say so rather than draft it",
			"fix the chart it names and call plan_dashboard again rather than telling the user it failed",
			"the user saves it as a dashboard within 24 hours",
			"never say the dashboard exists or what its charts show",
			"read its tiles with get_dashboard, then call plan_dashboard_change with that tile_id",
			"send type and every setting it should have: the ones get_dashboard showed, with only what the user asked changed",
			"One tile per change: for two tiles, call it twice",
			"When the change is refused, fix what it names and call it again",
			"never say the dashboard changed",
			"Apply gives this tile its own copy: say so",
		},
		"pipelines": {
			"propose it with plan_pipeline",
			"connection ids from list_connections",
			"A plan creates nothing",
			"table_choices",
			"never say the pipeline exists, runs or has moved data",
			"Tell the user each of the plan's notes",
		},
		"models": {
			"create_silver_model, which writes",
			"never write a silver model's SQL by hand",
			"only from the table and column names and types",
			"never say what a model holds or how many rows",
			"after_upstream",
			"give the app's clicks, never a tool's name",
			"Runs to \"After a pipeline or model runs\", pick them under Rebuild after",
			"active at once",
			"read the table's columns with describe_schema and draft them with plan_model_checks",
			"row_count_change with max_drop_pct 50 as a warning",
			"When the draft is refused, fix the checks it names and draft again",
			"the user saves the checks on the model's Checks tab",
			"never say a check exists, has run, passed or failed",
			"list_model_checks says which checks a model has, never their results",
		},
		"connectors": {
			"check list_connectors first",
			"Pass that URL exactly as they typed it, never one you found or made up; without one, ask for it",
			"only workspace owners and admins can generate",
			"check get_connector_generation",
			"Never ask for credentials or pass any",
		},
	}
	if len(moved) != len(guideTopics) {
		t.Fatalf("%d guides pinned, %d exist", len(moved), len(guideTopics))
	}
	for topic, phrases := range moved {
		body := guideBody(topic)
		for _, p := range phrases {
			if !strings.Contains(body, p) {
				t.Errorf("guide %s lost %q", topic, p)
			}
		}
	}
}

// The tools call a workflow's conditions branches; the person never sees the
// word, and no guide states a limit on how many there may be.
func TestGuides_SayConditionsNotBranches(t *testing.T) {
	said := "the tools name them branches, but never say branch to the user"
	// A dashboard's "up to 4 filters" is a limit too, but not on conditions.
	limit := regexp.MustCompile(`(?i)\b(up to|at most) (\d+|three) (branches|conditions)|\b(three|3) (branches|conditions)`)
	for _, topic := range guideTopics {
		body := guideBody(topic)
		n := 0
		if topic == "workflows" {
			n = 1
		}
		if got := strings.Count(body, said); got != n {
			t.Errorf("guide %s says %q %d times, want %d", topic, said, got, n)
		}
		if strings.Contains(strings.ToLower(strings.ReplaceAll(body, said, "")), "branch") {
			t.Errorf("guide %s says branch outside the one rule that names the word", topic)
		}
		if m := limit.FindString(body); m != "" {
			t.Errorf("guide %s states a limit: %q", topic, m)
		}
	}
}

// A guide is sent as JSON, which escapes < > & as < and so on: the model
// would read the escapes.
func TestGuides_HaveNoCharactersJSONEscapes(t *testing.T) {
	for _, topic := range guideTopics {
		if i := strings.IndexAny(guideBody(topic), "<>&"); i >= 0 {
			t.Errorf("guide %s has %q", topic, guideBody(topic)[i])
		}
	}
}

// The assistant loads a guide without a tool call; what it sends is what
// read_guide returns, so the model reads the same rules either way.
func TestGuideText_IsWhatReadGuideReturns(t *testing.T) {
	s, c, rows := inProcess(t, assistantActor)
	for _, topic := range guideTopics {
		want, ok := GuideText(topic)
		if !ok {
			t.Fatalf("GuideText(%q) not ok", topic)
		}
		args, _ := json.Marshal(map[string]string{"topic": topic})
		if r := s.Run(c, GuideTool, args); r.IsError || r.Text != want {
			t.Fatalf("%s: read_guide = %q…, GuideText = %q…", topic, r.Text[:60], want[:60])
		}
		var got guideOut
		if err := json.Unmarshal([]byte(want), &got); err != nil || got.Topic != topic || strings.Join(got.Rules, " ") != guideBody(topic) {
			t.Fatalf("%s: GuideText does not carry the whole guide: %v", topic, err)
		}
	}
	if len(*rows) != len(guideTopics) || (*rows)[0] != (auditRow{GuideTool, "guide", "", "ok", 200}) {
		t.Fatalf("audit = %+v", *rows)
	}
	if _, ok := GuideText("billing"); ok {
		t.Fatal("an unknown topic has no guide")
	}
}

// chartTypesGuide is the dashboards guide's paragraphs on what each chart type takes,
// from the registry: each type's description once, after the types it describes, with a
// type's label when its name does not say it. It is too long for the type argument's
// description, so the guide carries it and this test holds the guide to it. The list runs
// on into a next paragraph, each type whole, when one would pass maxStringRunes.
func chartTypesGuide() []string {
	var order []string
	names := map[string][]string{}
	for _, t := range charts.Registry {
		if t.Deprecated {
			continue
		}
		name := t.Type
		if l := strings.ToLower(t.Label); l != t.Type {
			name += " (" + l + ")"
		}
		if _, ok := names[t.Description]; !ok {
			order = append(order, t.Description)
		}
		names[t.Description] = append(names[t.Description], name)
	}
	paras := []string{"Chart types and what each takes:"}
	for _, d := range order {
		part := strings.Join(names[d], ", ") + ": " + d + "."
		if len([]rune(paras[len(paras)-1]+" "+part)) > maxStringRunes {
			paras = append(paras, "More chart types:")
		}
		paras[len(paras)-1] += " " + part
	}
	return paras
}

// The dashboards guide lists each chart type and what it takes as the registry says:
// the type argument's description is too short to, so a type added to the registry
// fails here until the guide names it.
func TestDashboardsGuide_ListsTheRegistrysTypes(t *testing.T) {
	want := chartTypesGuide()
	got := guides["dashboards"]
	at := slices.Index(got, want[0])
	if at < 0 || at+len(want) > len(got) || !slices.Equal(got[at:at+len(want)], want) {
		t.Fatalf("guides/dashboards.md has no paragraphs reading\n%s", strings.Join(want, "\n\n"))
	}
	for _, typ := range charts.Types {
		if !strings.Contains(strings.Join(want, " "), typ) {
			t.Errorf("control: the registry's list leaves out %s", typ)
		}
	}
}

// read_guide is a tool on both doors: an AI app connected over MCP reads the
// same guide.
func TestReadGuide_OverMCP(t *testing.T) {
	r, _ := testServer(t, Handlers{}, readerActor)
	want, _ := GuideText("dashboards")
	if res := call(t, r, GuideTool, map[string]any{"topic": "dashboards"}); isError(res) || resultText(res) != want {
		t.Fatalf("read_guide over MCP = %v", res)
	}
	if res := call(t, r, GuideTool, map[string]any{"topic": "billing"}); !isError(res) {
		t.Fatalf("a topic not in the enum is refused: %v", res)
	}
}

func TestCheckGuides_RefusesAGuideTooLongOrAParagraphCut(t *testing.T) {
	if err := checkGuides(); err != nil {
		t.Fatalf("control: %v", err)
	}
	saved := guides["models"]
	defer func() { guides["models"] = saved }()
	// Paragraphs each within maxStringRunes, together over the cap.
	overCap := make([]string, maxGuideChars/maxStringRunes+1)
	for i := range overCap {
		overCap[i] = strings.Repeat("a", maxStringRunes)
	}
	for name, rules := range map[string][]string{
		"empty":            nil,
		"over the cap":     overCap,
		"a long paragraph": {strings.Repeat("é", maxStringRunes+1)},
	} {
		guides["models"] = rules
		if err := checkGuides(); err == nil {
			t.Errorf("%s: no error", name)
		}
	}
}
