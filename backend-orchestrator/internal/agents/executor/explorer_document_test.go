package executor

import (
	"errors"
	"testing"

	"github.com/rsync-ai/backend-orchestrator/internal/mcp"
)

func TestExplorerDocumentRequestCarriesSpecAndConfig(t *testing.T) {
	req := explorerDocumentRequest("mongodb",
		map[string]interface{}{"connection_string": "x", "connector_version": " v1.0.0 "},
		"materialize",
		map[string]interface{}{"collection": "orders", "target": "orders_by_day", "replace": true})

	if req.Connector != "mongodb" || req.Operation != "materialize" || req.Version != "v1.0.0" {
		t.Fatalf("request = %s/%s@%q, want mongodb/materialize@v1.0.0", req.Connector, req.Operation, req.Version)
	}
	if req.Params["collection"] != "orders" || req.Params["target"] != "orders_by_day" || req.Params["replace"] != true {
		t.Fatalf("spec keys did not become params: %v", req.Params)
	}
	cfg, ok := req.Params["config"].(map[string]string)
	if !ok || cfg["connection_string"] != "x" {
		t.Fatalf("params.config = %#v, want the stringified connection config", req.Params["config"])
	}
	if req.LongCall {
		t.Fatal("the shared request must not be a LongCall: only ExplorerMaterialize sets it")
	}
}

func TestExplorerDocumentResultMapsTheConnectorsErrorCode(t *testing.T) {
	_, err := explorerDocumentResult("mongodb", "materialize", &mcp.ExecuteResponse{
		Success: false,
		Error:   "Collection shop.orders_by_day already exists and this model did not create it.",
		Result:  map[string]interface{}{"error_code": "target_exists"},
	}, nil)
	var fe *ExplorerFindError
	if !errors.As(err, &fe) || fe.Code != "target_exists" {
		t.Fatalf("err = %v, want an ExplorerFindError with code target_exists", err)
	}

	_, err = explorerDocumentResult("mongodb", "aggregate", &mcp.ExecuteResponse{
		Success: false,
		Result:  map[string]interface{}{"error_code": "operator_not_allowed", "path": "pipeline[0].$facet.b[0].$out"},
	}, nil)
	if !errors.As(err, &fe) || fe.Code != "operator_not_allowed" || fe.Path != "pipeline[0].$facet.b[0].$out" {
		t.Fatalf("err = %#v, want operator_not_allowed at the refused path", err)
	}
	if fe.Message == "" {
		t.Fatal("a failure with no connector message must still say which operation failed")
	}

	_, err = explorerDocumentResult("mongodb", "aggregate", nil, errors.New("dial tcp: connection refused"))
	if !errors.As(err, &fe) || fe.Code != "connection_failed" {
		t.Fatalf("err = %v, want connection_failed for an unreachable connector", err)
	}

	got, err := explorerDocumentResult("mongodb", "materialize", &mcp.ExecuteResponse{
		Success: true,
		Result:  map[string]interface{}{"rows_affected": 3},
	}, nil)
	if err != nil || got["rows_affected"] != 3 {
		t.Fatalf("success = %v, %v; want the connector's result", got, err)
	}
}
