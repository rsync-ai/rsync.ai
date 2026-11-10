package diagnose

// quota_and_sink_error_test.go — generator-trust phase 2.
//
// Three wrong answers from the rule order:
//
//  1. "Request failed with status 429" — what a REST connector reports for a
//     rate-limited page (shared/mcp-connectors/base_connector.py _page_error) —
//     matched no rule ("http 429" only) and escalated as unknown.
//  2. A bare "quota exceeded" was read as a rate limit and retried. A storage
//     or plan quota does not clear by waiting, and the free Atlas cluster's
//     "you are over your space quota" did not match the capacity rule at all.
//     Bug class: an ambiguous word routed to retry. An explicit 429 is a rate
//     limit; a storage phrase is capacity; a bare quota is not retried.
//  3. A silent drop whose sink error names the cause ("…; sink error: <cause>",
//     agents/executor/silent_drop_check.go) was sent to "regenerate connector"
//     without reading the cause. A full or refusing destination is not a
//     connector bug.

import "testing"

func TestRateLimitIncludesTheConnectorStatusWording(t *testing.T) {
	for _, msg := range []string{
		"batch export failed (partial sync, 0 rows landed before error): Request failed with status 429",
		"googleapi: Error 429: Quota exceeded for quota metric 'Read requests' (status 429)",
	} {
		got := New().Diagnose(Signal{ErrorMessage: msg})
		if got.Category != CategoryRateLimit || got.SuggestedAction != ActionBackoffRetry {
			t.Errorf("msg=%q: got %s/%s, want rate_limit/backoff_retry", msg, got.Category, got.SuggestedAction)
		}
	}
}

func TestAQuotaWithNo429IsNeverRetried(t *testing.T) {
	for _, msg := range []string{
		"write failed: you are over your space quota, using 513 MB of 512 MB",
		"insert into events: quota exceeded",
		"storage quota exceeded for project p1",
	} {
		got := New().Diagnose(Signal{ErrorMessage: msg})
		if got.SuggestedAction == ActionBackoffRetry {
			t.Errorf("msg=%q: got backoff_retry — a storage quota does not clear by waiting", msg)
		}
		if got.Category != CategoryDestCapacity || got.SuggestedAction != ActionEscalate {
			t.Errorf("msg=%q: got %s/%s, want dest_capacity/escalate", msg, got.Category, got.SuggestedAction)
		}
	}
}

func TestSilentDropReadsTheSinkErrorBeforeBlamingTheConnector(t *testing.T) {
	// The reason as silent_drop_check.go builds it.
	const head = "destination silently dropped all rows: read 500 from source, wrote 0 to destination; sink error: "
	cases := []struct {
		name, sinkErr string
		wantCat       Category
		wantAction    Action
	}{
		{"destination full", "write failed: you are over your space quota, using 513 MB of 512 MB",
			CategoryDestCapacity, ActionEscalate},
		{"destination refuses the credential", "authentication failed for user rsync_writer",
			CategoryAuthScope, ActionRequestUserConfig},
		{"destination token expired", "HTTP 401 Unauthorized: token expired",
			CategoryAuthExpired, ActionRefreshAuth},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got := New().Diagnose(Signal{
				ExecutorStatus: "silent_drop_detected",
				ErrorMessage:   head + c.sinkErr,
				SourceRowCount: 500,
				WrittenRows:    0,
			})
			if got.Category != c.wantCat || got.SuggestedAction != c.wantAction {
				t.Fatalf("got %s/%s, want %s/%s (%s)", got.Category, got.SuggestedAction,
					c.wantCat, c.wantAction, got.Rationale)
			}
		})
	}
}

// A sink error that is only a transport blip, or no cause at all, must not
// turn a silent drop into a retry: a re-run re-exports into a destination that
// may already hold the rows (KI-DESTINATION-WRITE-UNCONFIRMED-RETRIED).
func TestSilentDropWithATransientSinkErrorIsStillNotRetried(t *testing.T) {
	for _, sinkErr := range []string{
		"i/o timeout",
		"HTTP 429 Too Many Requests",
		"no destination acks recorded within the reconcile deadline — sink stalled or destination unreachable",
	} {
		got := New().Diagnose(Signal{
			ExecutorStatus: "silent_drop_detected",
			ErrorMessage:   "destination write unconfirmed: dispatched 500 rows via sink; sink error: " + sinkErr,
			SourceRowCount: 500,
			WrittenRows:    0,
		})
		if got.SuggestedAction != ActionRegenerateConnector {
			t.Errorf("sink error %q: got %s, want the unchanged regenerate_connector", sinkErr, got.SuggestedAction)
		}
	}
}

// A destination that answered and refused the rows — an API's 4xx on the
// record, a resource it cannot write — is the user's to fix (point the run at a
// writable resource, supply the field it requires), not a connector to
// regenerate: the connector delivered the rows and read the answer (P5 live,
// 2026-10-06: 10 ClickUp tasks refused with HTTP 400, healer said regenerate).
func TestSilentDropRefusedByTheDestinationAsksTheUser(t *testing.T) {
	// The reason as silent_drop_check.go builds it when the sink said why.
	const head = "destination refused all rows: dispatched 10, ack ledger confirmed 0 landed across 1 ack batches (execution e1); sink error: "
	for _, sinkErr := range []string{
		"dest error: 10 of 10 rows refused: HTTP 400",
		"dest error: 3 of 3 rows refused: HTTP 422 Unprocessable Entity: name is required",
		"dest error: Table 'products' is not writable (no create capability); writable: list_task",
		"dest error: 2 errors: HTTP 404; HTTP 409",
	} {
		got := New().Diagnose(Signal{
			ExecutorStatus: "silent_drop_detected",
			ErrorMessage:   head + sinkErr,
			SourceRowCount: 10,
			WrittenRows:    0,
		})
		if got.Category != CategoryUserConfig || got.SuggestedAction != ActionRequestUserConfig {
			t.Errorf("sink error %q: got %s/%s, want user_config/request_user_config (%s)",
				sinkErr, got.Category, got.SuggestedAction, got.Rationale)
		}
	}
}

// The pre-run check's own failure (executor dest_writable_check.go) reaches the
// healer as a plain failed run and asks the user too.
func TestADestinationResourceThatIsNotWritableAsksTheUser(t *testing.T) {
	got := New().Diagnose(Signal{ErrorMessage: `destination resource "products" is not writable on t-clickup: ` +
		`it takes writes to list_task. Name one as the destination (e.g. "… as list_task")`})
	if got.Category != CategoryUserConfig || got.SuggestedAction != ActionRequestUserConfig {
		t.Fatalf("got %s/%s, want user_config/request_user_config (%s)", got.Category, got.SuggestedAction, got.Rationale)
	}
}
