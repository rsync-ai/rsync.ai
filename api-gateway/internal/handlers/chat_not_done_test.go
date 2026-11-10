package handlers

import (
	"encoding/json"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"api-gateway/internal/chat"
)

// KI-NL-REQUESTED-STEP-DROPPED-SILENTLY: "sync X to Y, then notify me on Slack
// when it finishes" built a pipeline that synced and never notified, and
// nothing said so. A pipeline has no per-run notification (failure alerts are
// instance-wide, set under Admin → Notifications, and there is deliberately no
// success alert), so the Confirm card must say the notification is not
// included before the user says yes — whichever planner builds the pipeline.

func TestNotificationRequestChannel(t *testing.T) {
	cases := []struct {
		msg  string
		want string // the channel named, "" when none is, "-" when no notification is asked for
	}{
		{"Sync the events collection from MongoDB Source to GCS Dest as a one-time batch, then notify me on Slack when it finishes", "Slack"},
		{"sync mysql to s3, then send slack notification", "Slack"},
		{"copy orders to bigquery and email me when done", "email"},
		{"load pg into snowflake and alert me if it fails", ""},
		{"sync postgres to s3 then send a notification", ""},
		{"move users to gcs and let me know when it's finished", ""},
		{"sync pg to bq, notifications via slack please", "Slack"},
		// Not a notification request: a table, a column, or Slack as the source.
		{"sync the alerts table from postgres to s3", "-"},
		{"copy the email column from users to bigquery", "-"},
		{"sync slack messages to s3", "-"},
		{"sync notifications from postgres to gcs", "-"},
		{"sync pg to bq", "-"},
	}
	for _, tc := range cases {
		channel, asked := notificationRequest(tc.msg)
		if tc.want == "-" {
			if asked {
				t.Errorf("%q: read as a notification request (channel %q), want none", tc.msg, channel)
			}
			continue
		}
		if !asked || channel != tc.want {
			t.Errorf("%q: notificationRequest = (%q, %v), want (%q, true)", tc.msg, channel, asked, tc.want)
		}
	}
}

func TestNotDoneCardLine(t *testing.T) {
	if got := notDoneCardLine(&chat.PendingIntent{OriginalRequest: "sync pg to bq"}); got != "" {
		t.Fatalf("nothing unsupported asked: card line = %q, want empty", got)
	}
	if got := notDoneCardLine(nil); got != "" {
		t.Fatalf("nil intent: card line = %q, want empty", got)
	}
	got := notDoneCardLine(&chat.PendingIntent{
		OriginalRequest: "Sync the events collection from MongoDB Source to GCS Dest as a one-time batch, then notify me on Slack when it finishes",
	})
	for _, w := range []string{"Not included", "Slack", "Admin → Notifications"} {
		if !strings.Contains(got, w) {
			t.Fatalf("card line = %q, missing %q", got, w)
		}
	}
}

// Every Confirm card carries the line, so no chat path can create a pipeline
// that silently drops the notification the user asked for.
func TestEveryConfirmCardSaysWhatIsNotIncluded(t *testing.T) {
	src, err := os.ReadFile("chat_nl_pipeline.go")
	if err != nil {
		t.Fatalf("read chat_nl_pipeline.go: %v", err)
	}
	cards := 0
	for i, line := range strings.Split(string(src), "\n") {
		if !strings.Contains(line, "Create and run this pipeline?") {
			continue
		}
		cards++
		if !strings.Contains(line, "notDoneCardLine(") {
			t.Errorf("chat_nl_pipeline.go:%d builds a Confirm card without notDoneCardLine", i+1)
		}
	}
	if cards == 0 {
		t.Fatal("found no Confirm card in chat_nl_pipeline.go; the check matched nothing")
	}
}

// The KI's write-up left this open: a request for any other step a pipeline
// cannot run (a webhook call, a condition, an AI step) was still dropped without
// a word. Every kind the runtime cannot run is now named on the card, and a
// request that only mentions such a thing as data is not.
var notIncludedCases = []struct {
	msg  string
	want []string // the kinds the card must name; empty = none
}{
	{"sync orders from postgres to s3, then call our webhook", []string{"api_call"}},
	{"copy users to bigquery and hit https://hooks.example.com/done when finished", []string{"api_call"}},
	{"load pg into snowflake then post the results to a webhook", []string{"api_call"}},
	{"sync tickets to bq and trigger the zapier webhook", []string{"api_call"}},
	{"sync reviews from postgres to bq using an llm to tag each review", []string{"llm"}},
	{"copy tickets to s3 and run sentiment analysis on them", []string{"llm"}},
	{"move feedback to gcs with ai-generated summaries", []string{"llm"}},
	{"sync pg to bq only if the orders table has new rows", []string{"condition"}},
	{"copy all tables to s3 unless they are empty", []string{"condition"}},
	{"if the load fails, call the pagerduty webhook", []string{"api_call", "condition"}},
	{"sync pg to bq then email me and call the webhook", []string{"notification", "api_call"}},
	{"Sync orders to BigQuery, then notify me on Slack", []string{"notification"}},
	// The condition belongs to the notification, which is already named.
	{"load pg into snowflake and alert me if it fails", []string{"notification"}},
	{"sync pg to bq, alert me only when it fails", []string{"notification"}},
	{"if the load fails, email me", []string{"notification"}},
	// Not a step: a table, a column, a row filter, a source, the product.
	{"sync the webhooks table from postgres to s3", nil},
	{"send the webhook_events table to s3", nil},
	{"copy the endpoint_url column to bigquery", nil},
	{"sync call logs from postgres to s3", nil},
	{"sync the sentiment column and the summaries table to bq", nil},
	{"categorize orders by amount and load them into snowflake", nil},
	{"copy only rows where status is active to s3", nil},
	{"sync only the users table to bigquery", nil},
	{"sync pg to bq with rsync.ai", nil},
	{"sync the ai_logs table from postgres to gcs", nil},
	{"sync pg to bq", nil},
}

// What the card says for each kind, so the table above reads by kind.
var notIncludedPhrase = map[string]string{
	"notification": "notification you asked for",
	"api_call":     "webhook call you asked for",
	"llm":          "AI step you asked for",
	"condition":    "condition you asked for",
}

func TestNotDoneCardLineNamesEveryStepAPipelineCannotRun(t *testing.T) {
	for _, tc := range notIncludedCases {
		got := notDoneCardLine(&chat.PendingIntent{OriginalRequest: tc.msg})
		if len(tc.want) == 0 {
			if got != "" {
				t.Errorf("%q: card says %q, want nothing", tc.msg, got)
			}
			continue
		}
		if !strings.Contains(got, "Not included") {
			t.Errorf("%q: card says %q, want a Not included line", tc.msg, got)
			continue
		}
		for kind, phrase := range notIncludedPhrase {
			if named := strings.Contains(got, phrase); named != slices.Contains(tc.want, kind) {
				t.Errorf("%q: names %s = %v, want %v (card %q)", tc.msg, kind, named, !named, got)
			}
		}
	}
}

// The class: a step a pipeline cannot run, asked for in chat and dropped without
// a word. Every not_runnable kind in the shared golden has a detector on the
// card and a request in the table above that it names; a kind added there
// fails here until the card can name it.
func TestEveryKindAPipelineCannotRunIsNamedOnTheCard(t *testing.T) {
	raw, err := os.ReadFile(filepath.Join("..", "..", "..", "shared", "workflow_node_kinds_golden.json"))
	if err != nil {
		t.Fatal(err)
	}
	var golden struct {
		Runnable    []string `json:"runnable"`
		NotRunnable []string `json:"not_runnable"`
	}
	if err := json.Unmarshal(raw, &golden); err != nil {
		t.Fatal(err)
	}
	// Denominator: the golden still lists the kinds this card is known to name.
	if len(golden.NotRunnable) < 4 || !slices.Contains(golden.NotRunnable, "notification") {
		t.Fatalf("not_runnable = %v; this census reads the wrong list", golden.NotRunnable)
	}

	detected := map[string]bool{}
	for _, s := range notIncludedSteps {
		if slices.Contains(golden.Runnable, s.kind) {
			t.Errorf("the card says %q is not included, but the runtime runs it", s.kind)
		}
		detected[s.kind] = true
	}
	for _, kind := range golden.NotRunnable {
		if !detected[kind] {
			t.Errorf("a request for a %q step is dropped without a word: notIncludedSteps has no detector for it", kind)
			continue
		}
		if notIncludedPhrase[kind] == "" {
			t.Errorf("no card wording for %q in notIncludedPhrase", kind)
		}
		positive := false
		for _, tc := range notIncludedCases {
			positive = positive || slices.Contains(tc.want, kind)
		}
		if !positive {
			t.Errorf("no request in notIncludedCases asks for a %q step, so its detector is never shown to fire", kind)
		}
	}
}

// Several steps left out are listed one per line, each with its reason.
func TestNotDoneCardLineListsEachStepLeftOut(t *testing.T) {
	got := notDoneCardLine(&chat.PendingIntent{OriginalRequest: "sync pg to bq then email me and call the webhook"})
	want := "\n\nNot included:\n- the email notification you asked for — a pipeline can't send its own notifications yet. " +
		"Alerts when any pipeline fails can go to Slack or email: set them up under Admin → Notifications." +
		"\n- the webhook call you asked for — a pipeline can't call a webhook or an endpoint as one of its steps."
	if got != want {
		t.Fatalf("card line =\n%q\nwant\n%q", got, want)
	}
}
