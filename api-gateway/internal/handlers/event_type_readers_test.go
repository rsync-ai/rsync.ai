package handlers

import (
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"testing"
)

// A handler that filters pipeline_run_events on an event type no service writes reads a
// permanent empty answer, and the page shows it as "nothing happened". fetchAgentReasoning
// selected AGENT_DECISION, PLANNER_DECISION and DECISION_MADE, which no producer emitted,
// so the Overview's "automated decisions" line could never appear. This requires every
// event type a handler names in SQL to be written, as a string literal, by some producer.

// sqlEventTypeFilter finds `event_type = 'X'` and `event_type IN ('X', 'Y')` in SQL text;
// quotedEventType then takes each 'X' out of the match.
var (
	sqlEventTypeFilter = regexp.MustCompile(`(?i)event_type\s*(?:=\s*'[A-Z0-9_]+'|IN\s*\([^)]*\))`)
	quotedEventType    = regexp.MustCompile(`'([A-Z][A-Z0-9_]+)'`)
)

// eventProducerRoots are the trees whose code emits pipeline events, relative to the
// repository root, with the string literal shape each language writes a type in. A Go
// string is double-quoted, so the handlers' single-quoted SQL never counts as its own
// producer.
var eventProducerRoots = []struct {
	dir, ext string
	literal  *regexp.Regexp
}{
	{"backend-orchestrator", ".go", regexp.MustCompile(`"([A-Z][A-Z0-9_]+)"`)},
	{"backend-temporal-adapter", ".go", regexp.MustCompile(`"([A-Z][A-Z0-9_]+)"`)},
	{"api-gateway", ".go", regexp.MustCompile(`"([A-Z][A-Z0-9_]+)"`)},
	{"llm-service/src", ".py", regexp.MustCompile(`["']([A-Z][A-Z0-9_]+)["']`)},
	{"shared/mcp-connectors/internal", ".py", regexp.MustCompile(`["']([A-Z][A-Z0-9_]+)["']`)},
}

func isTestSource(name string) bool {
	return strings.HasSuffix(name, "_test.go") || strings.HasPrefix(name, "test_") || strings.HasSuffix(name, "_test.py")
}

func TestEventTypeFiltersNameATypeSomethingWrites(t *testing.T) {
	read := map[string]string{} // event type -> where a handler reads it
	files, err := filepath.Glob("*.go")
	if err != nil {
		t.Fatal(err)
	}
	for _, f := range files {
		if isTestSource(f) {
			continue
		}
		src, err := os.ReadFile(f)
		if err != nil {
			t.Fatal(err)
		}
		for _, at := range sqlEventTypeFilter.FindAllIndex(src, -1) {
			line := 1 + strings.Count(string(src[:at[0]]), "\n")
			for _, m := range quotedEventType.FindAllSubmatch(src[at[0]:at[1]], -1) {
				if _, seen := read[string(m[1])]; !seen {
					read[string(m[1])] = f + ":" + strconv.Itoa(line)
				}
			}
		}
	}
	// Arm the reader scan: the handlers filter on run completion and data-plane metrics.
	for _, want := range []string{"PIPELINE_COMPLETED", "DATA_PLANE_METRICS"} {
		if _, ok := read[want]; !ok {
			t.Fatalf("the SQL scan did not find the handlers' %s filter; found %v", want, read)
		}
	}

	written := map[string]bool{}
	root := filepath.Join("..", "..", "..")
	for _, p := range eventProducerRoots {
		scanned := 0
		err := filepath.WalkDir(filepath.Join(root, p.dir), func(path string, d fs.DirEntry, err error) error {
			if err != nil {
				return err
			}
			if d.IsDir() {
				switch d.Name() {
				case "node_modules", "vendor", ".venv", "venv", "__pycache__", "testdata", "tests":
					return filepath.SkipDir
				}
				return nil
			}
			if filepath.Ext(path) != p.ext || isTestSource(d.Name()) {
				return nil
			}
			src, err := os.ReadFile(path)
			if err != nil {
				return err
			}
			scanned++
			for _, m := range p.literal.FindAllSubmatch(src, -1) {
				written[string(m[1])] = true
			}
			return nil
		})
		if err != nil {
			t.Fatalf("scan %s: %v", p.dir, err)
		}
		// Arm the producer scan: a root that moved or emptied must fail here, not pass
		// every reader by default.
		if scanned == 0 {
			t.Fatalf("scanned no %s files under %s", p.ext, p.dir)
		}
	}

	for typ, where := range read {
		if !written[typ] {
			t.Errorf("%s filters pipeline_run_events on %s, which no producer writes: the read is always empty. "+
				"Point it at an event that is written, or remove it", where, typ)
		}
	}
}
