package config

import (
	"sync"
	"testing"
)

// The usage panel is a BILLING surface: plan, pipeline/query limits, trial
// expiry, metered transfer GB. Its default therefore follows plan enforcement
// rather than standing alone, so a self-host that has already opted out of
// billing does not have to opt out twice. These cases pin BOTH directions --
// a one-sided table would keep passing if the derivation were deleted.
func TestResolveUsagePanelFollowsBillingUnlessPinned(t *testing.T) {
	const (
		unset = "\x00" // distinct from the empty string, which is a real value
	)
	cases := []struct {
		name    string
		billing string
		panel   string
		want    bool
	}{
		// Cloud sets neither variable. The panel must show.
		{"both unset is cloud", unset, unset, true},
		// The OSS quickstart compose and the Helm chart set only the billing
		// flag. The panel must disappear with no second flag to remember.
		{"billing off hides the panel", "false", unset, false},
		{"billing 0 hides the panel", "0", unset, false},
		// billingEnforced() trims before parsing; the derived default must too,
		// or a value with stray whitespace silently re-enables the panel on a
		// self-host whose quotas are not enforced.
		{"billing off with whitespace still hides", " false ", unset, false},
		// Fail toward the cloud behaviour on junk, exactly as billing does.
		{"unparseable billing shows the panel", "yes-please", unset, true},
		{"billing on shows the panel", "true", unset, true},
		// The explicit flag wins in both directions.
		{"pinned on beats billing off", "false", "true", true},
		{"pinned off beats billing on", "true", "false", false},
		{"pinned off beats billing unset", unset, "false", false},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if tc.billing != unset {
				t.Setenv("RSYNC_BILLING_ENFORCED", tc.billing)
			} else {
				t.Setenv("RSYNC_BILLING_ENFORCED", "")
			}
			if tc.panel != unset {
				t.Setenv("FEATURE_USAGE_PANEL", tc.panel)
			} else {
				t.Setenv("FEATURE_USAGE_PANEL", "")
			}
			if got := resolveUsagePanel(); got != tc.want {
				t.Fatalf("resolveUsagePanel() = %v, want %v (billing=%q panel=%q)",
					got, tc.want, tc.billing, tc.panel)
			}
		})
	}
}

// The frontend reads this flag over /api/v1/features at runtime, which is the
// only path that reaches a PREBUILT image: NEXT_PUBLIC_* is inlined at Next.js
// build time, so an OSS compose cannot move it. Dropping the key from the
// response silently restores the panel's build-time default (shown) on every
// self-host, so the key's presence is worth asserting on its own.
func TestToAPIResponseCarriesUsagePanel(t *testing.T) {
	for _, want := range []bool{true, false} {
		resp := (&FeatureFlags{UsagePanel: want}).ToAPIResponse()
		got, ok := resp["usage_panel"]
		if !ok {
			t.Fatalf("usage_panel missing from ToAPIResponse(); keys present: %v", keysOf(resp))
		}
		if got != want {
			t.Fatalf("usage_panel = %v, want %v", got, want)
		}
	}
}

func keysOf(m map[string]interface{}) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	return out
}

// Ask rsync.ai and the agent gateway are on unless an install turns them off:
// cloud runs both, so per the OSS/cloud rule the code default is cloud's. Their
// routes exist only while on (main.go), and the frontend shows them only when
// /features says so.
func TestAssistantAndAgentGatewayAreOnUnlessDisabled(t *testing.T) {
	t.Cleanup(func() { featuresOnce, features = sync.Once{}, nil })
	for _, tc := range []struct {
		env  string
		want bool
	}{{"", true}, {"true", true}, {"false", false}, {"0", false}} {
		t.Setenv("RSYNC_ASSISTANT_ENABLED", tc.env)
		t.Setenv("RSYNC_AGENT_GATEWAY_ENABLED", tc.env)
		featuresOnce, features = sync.Once{}, nil // LoadFeatures reads the env once
		f := LoadFeatures()
		if f.Assistant != tc.want || f.AgentGateway != tc.want {
			t.Fatalf("env=%q: Assistant = %v, AgentGateway = %v, want %v", tc.env, f.Assistant, f.AgentGateway, tc.want)
		}
		resp := f.ToAPIResponse()
		for _, key := range []string{"assistant", "agent_gateway"} {
			if got, ok := resp[key]; !ok || got != tc.want {
				t.Fatalf("env=%q: /features %s = %v (present %v), want %v", tc.env, key, got, ok, tc.want)
			}
		}
	}
}

// Sample rows send row values to a third-party model, so this flag fails
// closed: only unset (cloud) or an explicit true allows them. A table with
// only "" and "false" would keep passing if a typo fell back to allowed.
func TestLLMSampleRowsAllowedFailsClosed(t *testing.T) {
	for _, tc := range []struct {
		env  string
		want bool
	}{
		{"", true},         // cloud sets nothing
		{"true", true},     //
		{" true ", true},   // trimmed, like BillingEnforced
		{"false", false},   // the self-host compose and chart
		{" false ", false}, // whitespace must not re-open them
		{"0", false},       //
		{"no", false},      // not a ParseBool value: closed
		{"flase", false},   // a typo: closed
	} {
		t.Setenv("RSYNC_LLM_SAMPLE_ROWS_ALLOWED", tc.env)
		if got := LLMSampleRowsAllowed(); got != tc.want {
			t.Fatalf("RSYNC_LLM_SAMPLE_ROWS_ALLOWED=%q: LLMSampleRowsAllowed() = %v, want %v", tc.env, got, tc.want)
		}
	}
}

// The frontend hides the sample-rows switches from llm_sample_rows; a missing
// key reads as the cloud default there, so its presence is asserted.
func TestToAPIResponseCarriesLLMSampleRows(t *testing.T) {
	t.Cleanup(func() { featuresOnce, features = sync.Once{}, nil })
	for _, tc := range []struct {
		env  string
		want bool
	}{{"", true}, {"false", false}} {
		t.Setenv("RSYNC_LLM_SAMPLE_ROWS_ALLOWED", tc.env)
		featuresOnce, features = sync.Once{}, nil
		got, ok := LoadFeatures().ToAPIResponse()["llm_sample_rows"]
		if !ok || got != tc.want {
			t.Fatalf("RSYNC_LLM_SAMPLE_ROWS_ALLOWED=%q: llm_sample_rows = %v (present %v), want %v", tc.env, got, ok, tc.want)
		}
	}
}

// One workspace per install is the self-host shape; cloud sets nothing and must
// stay multi-workspace, so a typo falls back to the cloud behaviour, as
// BillingEnforced does. The gate is soft by design (a self-hoster can flip it).
func TestSingleWorkspaceIsOffUnlessSet(t *testing.T) {
	for _, tc := range []struct {
		env  string
		want bool
	}{
		{"", false},        // cloud sets nothing
		{"true", true},     // the self-host compose and chart
		{" true ", true},   // trimmed, like BillingEnforced
		{"1", true},        //
		{"false", false},   //
		{" false ", false}, //
		{"yes", false},     // not a ParseBool value: cloud behaviour
		{"ture", false},    // a typo: cloud behaviour
	} {
		t.Setenv("RSYNC_SINGLE_WORKSPACE", tc.env)
		if got := SingleWorkspace(); got != tc.want {
			t.Fatalf("RSYNC_SINGLE_WORKSPACE=%q: SingleWorkspace() = %v, want %v", tc.env, got, tc.want)
		}
	}
}

// The header hides "New workspace" from single_workspace; a missing key reads as
// the cloud default there, so its presence is asserted.
func TestToAPIResponseCarriesSingleWorkspace(t *testing.T) {
	t.Cleanup(func() { featuresOnce, features = sync.Once{}, nil })
	for _, tc := range []struct {
		env  string
		want bool
	}{{"", false}, {"true", true}} {
		t.Setenv("RSYNC_SINGLE_WORKSPACE", tc.env)
		featuresOnce, features = sync.Once{}, nil
		got, ok := LoadFeatures().ToAPIResponse()["single_workspace"]
		if !ok || got != tc.want {
			t.Fatalf("RSYNC_SINGLE_WORKSPACE=%q: single_workspace = %v (present %v), want %v", tc.env, got, ok, tc.want)
		}
	}
}
