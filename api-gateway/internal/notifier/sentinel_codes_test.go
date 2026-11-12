package notifier

import (
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"testing"
)

// The sentinel agents live in the other module and publish their own catalog
// codes (backend-orchestrator/internal/agents/sentinel/notify.go). Nothing
// links the two: a code renamed or added over there compiles fine here, and
// the only symptom is a degraded headline in somebody's inbox — the humanized
// event type instead of the sentence written for that failure.
//
// So read the producer and check every code it emits has copy here. This reads
// the file rather than a golden list on purpose: a golden list is one more
// thing to forget to update, and it would pass while the producer drifts.
func TestEverySentinelCodeHasCopy(t *testing.T) {
	path := filepath.Join("..", "..", "..", "backend-orchestrator",
		"internal", "agents", "sentinel", "notify.go")

	src, err := os.ReadFile(path)
	if err != nil {
		// Not a skip. A skip here is a green test covering nothing: the guard
		// would go quiet exactly when the producer was moved or deleted.
		t.Fatalf("cannot read the sentinel alert producer at %s: %v\n"+
			"If it moved, update this path — do not delete the check.", path, err)
	}

	codes := regexp.MustCompile(`(?m)^\s*code:\s*"([A-Z0-9_]+)"`).FindAllStringSubmatch(string(src), -1)
	if len(codes) == 0 {
		t.Fatalf("parsed 0 codes out of %s — the producer's shape changed and this "+
			"check silently stopped checking anything", path)
	}

	for _, m := range codes {
		code := m[1]
		entry, ok := catalog[code]
		if !ok {
			t.Errorf("the sentinel publishes %s but no catalog entry renders it; "+
				"the bell would fall back to a humanized event type", code)
			continue
		}
		if entry.Title == "" || entry.Impact == "" || entry.ActionLabel == "" {
			t.Errorf("%s has a catalog entry with an empty field: %+v", code, entry)
		}
		// Every one of these means the data stopped moving. If one is filed as
		// info it drops below the delivery threshold for email and Slack.
		if entry.Severity != severityCritical {
			t.Errorf("%s severity = %q, want %q — a sentinel alert that is not "+
				"critical will not be delivered off the bell", code, entry.Severity, severityCritical)
		}
		if _, ok := codeCategory[code]; !ok {
			t.Errorf("%s has no category, so a user cannot mute it on its own", code)
		}
	}
	t.Logf("checked %d sentinel codes against the catalog", len(codes))
}

// The same drift can hit the other direction: a resolved notice a producer sends that
// resolvedPairs does not know is delivered as a plain info notice with no dedup
// pairing or "was the alert pushed" gate, and one in resolvedPairs that no producer
// sends is dead policy. So the set of resolved codes the two producers write must
// equal the set the gateway pairs, and each must have copy and a category.
func TestEveryResolvedCodeAProducerSendsIsPairedHere(t *testing.T) {
	root := filepath.Join("..", "..", "..")
	producers := []struct {
		path string
		re   *regexp.Regexp
	}{
		// The sentinel names its resolved codes in a field called resolvedCode, so
		// TestEverySentinelCodeHasCopy's `code:` scan, which requires critical, skips them.
		{filepath.Join(root, "backend-orchestrator", "internal", "agents", "sentinel", "resolved_notice.go"),
			regexp.MustCompile(`(?m)^\s*resolvedCode:\s*"([A-Z0-9_]+)"`)},
		{filepath.Join(root, "api-gateway", "internal", "handlers", "core_service_watch.go"),
			regexp.MustCompile(`"([A-Z0-9_]+_RESOLVED)"`)},
	}

	sent := map[string]bool{}
	for _, p := range producers {
		src, err := os.ReadFile(p.path)
		if err != nil {
			t.Fatalf("cannot read the resolved-notice producer at %s: %v\n"+
				"If it moved, update this path — do not delete the check.", p.path, err)
		}
		matches := p.re.FindAllStringSubmatch(string(src), -1)
		if len(matches) == 0 {
			t.Fatalf("parsed 0 resolved codes out of %s — the producer's shape changed and "+
				"this check silently stopped checking anything", p.path)
		}
		for _, m := range matches {
			sent[m[1]] = true
		}
	}

	for code := range sent {
		if _, ok := resolvedPairs[code]; !ok {
			t.Errorf("a producer sends %s but resolvedPairs does not pair it with an alert, "+
				"so the gateway would deliver it ungated", code)
		}
		entry, ok := catalog[code]
		switch {
		case !ok:
			t.Errorf("a producer sends %s but no catalog entry renders it", code)
		case entry.Severity != severityInfo:
			t.Errorf("%s severity = %q, want %q", code, entry.Severity, severityInfo)
		}
		if _, ok := codeCategory[code]; !ok {
			t.Errorf("%s has no category, so its alert's mutes would not silence it", code)
		}
	}
	var unsent []string
	for code := range resolvedPairs {
		if !sent[code] {
			unsent = append(unsent, code)
		}
	}
	sort.Strings(unsent)
	if len(unsent) > 0 {
		t.Errorf("resolvedPairs has codes no producer sends: %v", unsent)
	}
	t.Logf("checked %d resolved codes against the producers", len(sent))
}
