package workers

import "testing"

// A V2 planner task carries the capability resolver's output under "connectors"
// and no source_connector/destination_connector, so the planner service got the
// connection ids only. Its own lookup of those ids needs a gateway session it does
// not hold, and the heuristic planner guessed which engine was the source: prod
// a6ca2103 (PostgreSQL -> MongoDB CDC) logged MongoDB's Debezium class. The
// worker must forward the types the workflow already resolved.

func TestForwardConnectorTypes(t *testing.T) {
	cases := []struct {
		name             string
		taskContext      map[string]interface{}
		wantSrc, wantDst interface{}
	}{
		{
			name: "V2 task: resolved connector names",
			taskContext: map[string]interface{}{
				"connectors": map[string]interface{}{
					"version":             2,
					"resolved_connectors": map[string]interface{}{"source": "postgresql", "destination": "mongodb"},
				},
			},
			wantSrc: "postgresql", wantDst: "mongodb",
		},
		{
			name: "resolved connectors as maps with a type",
			taskContext: map[string]interface{}{
				"connectors": map[string]interface{}{
					"resolved_connectors": map[string]interface{}{
						"source":      map[string]interface{}{"type": "mysql"},
						"destination": map[string]interface{}{"connector_type": "gcs"},
					},
				},
			},
			wantSrc: "mysql", wantDst: "gcs",
		},
		{
			name: "explicit types win over the resolver",
			taskContext: map[string]interface{}{
				"source_type": "mongodb", "destination_type": "postgresql",
				"connectors": map[string]interface{}{
					"resolved_connectors": map[string]interface{}{"source": "postgresql", "destination": "mongodb"},
				},
			},
			wantSrc: "mongodb", wantDst: "postgresql",
		},
		{
			name: "legacy connector fields",
			taskContext: map[string]interface{}{
				"source_connector":      "postgresql",
				"destination_connector": map[string]interface{}{"type": "bigquery"},
			},
			wantSrc: "postgresql", wantDst: "bigquery",
		},
		{
			name: "one side known",
			taskContext: map[string]interface{}{
				"connectors": map[string]interface{}{
					"resolved_connectors": map[string]interface{}{"source": "postgresql"},
				},
			},
			wantSrc: "postgresql", wantDst: nil,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			dst := map[string]interface{}{}
			forwardConnectorTypes(dst, tc.taskContext)
			if dst["source_type"] != tc.wantSrc || dst["destination_type"] != tc.wantDst {
				t.Fatalf("source_type=%v destination_type=%v, want %v/%v", dst["source_type"], dst["destination_type"], tc.wantSrc, tc.wantDst)
			}
		})
	}
}

func TestForwardConnectorTypes_NothingResolvedLeavesDstUntouched(t *testing.T) {
	for _, taskContext := range []map[string]interface{}{
		nil,
		{},
		{"connectors": map[string]interface{}{"resolved_connectors": map[string]interface{}{}}},
		{"connectors": "not-a-map", "source_connector": nil},
	} {
		dst := map[string]interface{}{}
		forwardConnectorTypes(dst, taskContext)
		if len(dst) != 0 {
			t.Fatalf("dst = %#v for %#v, want untouched", dst, taskContext)
		}
	}
}
