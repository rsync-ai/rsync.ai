package workflows

import (
	"context"
	"errors"
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"sort"
	"strconv"
	"strings"
	"sync"
	"testing"

	"go.temporal.io/sdk/temporal"
	"go.temporal.io/sdk/testsuite"
)

// A V2 activity signals "wait for the user" (table selection, a missing
// connection…) and "run the next chunk" by returning a policy ActivityError.
// Those are expected outcomes the workflow parks or loops on, not failures, but
// the Temporal SDK logs every activity error at ERROR unless the error is
// benign (activityTaskHandlerImpl.Execute: "Activity error."). So every pipeline
// waiting for table selection logged an ERROR.
//
// Bug class: an expected control-flow outcome signalled as an activity error is
// logged as a failure. These tests run the error through the SDK's own activity
// task handler and read the level it logs at.

// wantPolicyCodeIsAWait classifies every policy code. A code is a wait when the
// workflow parks or re-dispatches on it instead of failing or repairing.
var wantPolicyCodeIsAWait = map[string]bool{
	PolicyCodeTableSelectionRequired: true,  // parks in StateWaitingForTableSelection
	PolicyCodeNeedsContinuation:      true,  // re-dispatches the executor for the next chunk
	PolicyCodeNodeInputRequired:      true,  // DAG node parks in NodeStatusWaiting
	PolicyCodeMissingConnectors:      true,  // parks in StateWaitingForConnectorGeneration
	PolicyCodeMissingConnections:     true,  // parks in StateWaitingForConnectionConfiguration
	PolicyCodeInvalidConfiguration:   false, // fails the run
	PolicyCodeAuthRefreshFailed:      false, // auth repair
	PolicyCodeAuthReauthRequired:     false, // fails the run until the user reconnects
	PolicyCodeSchemaDriftDetected:    false, // schema repair
	PolicyCodeSchemaApprovalRequired: false, // schema repair that could not proceed
	PolicyCodeApprovalRequired:       false, // nothing parks on it
	PolicyCodeTokenBudgetExceeded:    false, // nothing parks on it
}

// capturedLogger records the level of every log line the SDK writes.
type capturedLogger struct {
	mu    sync.Mutex
	lines []string
}

func (l *capturedLogger) add(level, msg string) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.lines = append(l.lines, level+" "+msg)
}
func (l *capturedLogger) Debug(msg string, _ ...interface{}) { l.add("DEBUG", msg) }
func (l *capturedLogger) Info(msg string, _ ...interface{})  { l.add("INFO", msg) }
func (l *capturedLogger) Warn(msg string, _ ...interface{})  { l.add("WARN", msg) }
func (l *capturedLogger) Error(msg string, _ ...interface{}) { l.add("ERROR", msg) }

// activityErrorLogLevel runs an activity that returns err through the SDK's
// activity task handler and returns the level of its "Activity error." line,
// plus the error the caller (the workflow) receives.
func activityErrorLogLevel(t *testing.T, err error) (string, error) {
	t.Helper()
	logger := &capturedLogger{}
	suite := &testsuite.WorkflowTestSuite{}
	suite.SetLogger(logger)
	env := suite.NewTestActivityEnvironment()
	failing := func(ctx context.Context) error { return err }
	env.RegisterActivity(failing)

	_, got := env.ExecuteActivity(failing)
	if got == nil {
		t.Fatalf("the activity returned %v but the caller saw no error", err)
	}
	logger.mu.Lock()
	defer logger.mu.Unlock()
	level := ""
	for _, line := range logger.lines {
		if strings.HasSuffix(line, " Activity error.") {
			if level != "" {
				t.Fatalf("the SDK logged the activity error twice: %v", logger.lines)
			}
			level = strings.SplitN(line, " ", 2)[0]
		}
	}
	if level == "" {
		t.Fatalf("the SDK logged no \"Activity error.\" line, so this probe reads nothing: %v", logger.lines)
	}
	return level, got
}

func TestAWaitingPipelineIsNotLoggedAsAnError(t *testing.T) {
	for code, isAWait := range wantPolicyCodeIsAWait {
		code, isAWait := code, isAWait
		t.Run(code, func(t *testing.T) {
			level, got := activityErrorLogLevel(t, toTemporalError(NewPolicyError(code, "expected outcome", map[string]interface{}{"k": "v"})))

			want := "ERROR"
			if isAWait {
				want = "DEBUG"
			}
			if level != want {
				t.Fatalf("policy code %s: the SDK logged the activity error at %s, want %s", code, level, want)
			}

			// The workflow still branches on the same typed error.
			activityErr, ok := unwrapV2ActivityError(got)
			if !ok || activityErr.Type != ErrTypePolicy || activityErr.Code != code || activityErr.Metadata["k"] != "v" {
				t.Fatalf("policy code %s: the workflow decodes %+v (ok=%v), want the policy error it was sent", code, activityErr, ok)
			}
			var appErr *temporal.ApplicationError
			if !errors.As(got, &appErr) || !appErr.NonRetryable() {
				t.Fatalf("policy code %s: want a non-retryable application error, got %v", code, got)
			}
		})
	}
}

func TestARealActivityFailureIsStillLoggedAsAnError(t *testing.T) {
	for _, e := range []*ActivityError{
		NewTransientError("RESPONSE_TIMEOUT", "timed out", nil),
		NewInfraError("KAFKA_DOWN", "broker unreachable", nil),
		NewDeterministicError("CONNECTOR_RESOLUTION_FAILED", "bad response", nil),
		// A wait code under another type is not a wait.
		NewDeterministicError(PolicyCodeTableSelectionRequired, "not a policy error", nil),
	} {
		e := e
		t.Run(string(e.Type)+"/"+e.Code, func(t *testing.T) {
			level, _ := activityErrorLogLevel(t, toTemporalError(e))
			if level != "ERROR" {
				t.Fatalf("%s: the SDK logged the activity error at %s, want ERROR", e, level)
			}
		})
	}
}

// Every policy code must be classified above, so a new code cannot slip in as a
// wait that logs ERROR, or as a failure that logs DEBUG, without a decision.
func TestEveryPolicyCodeIsClassifiedAsAWaitOrAFailure(t *testing.T) {
	fset := token.NewFileSet()
	file, err := parser.ParseFile(fset, "nl_pipeline_v2_errors.go", nil, 0)
	if err != nil {
		t.Fatal(err)
	}
	var declared []string
	ast.Inspect(file, func(n ast.Node) bool {
		spec, ok := n.(*ast.ValueSpec)
		if !ok {
			return true
		}
		for i, name := range spec.Names {
			if !strings.HasPrefix(name.Name, "PolicyCode") || i >= len(spec.Values) {
				continue
			}
			lit, ok := spec.Values[i].(*ast.BasicLit)
			if !ok {
				t.Fatalf("%s is not a string literal", name.Name)
			}
			value, err := strconv.Unquote(lit.Value)
			if err != nil {
				t.Fatal(err)
			}
			declared = append(declared, value)
		}
		return true
	})
	if len(declared) == 0 {
		t.Fatal("found no PolicyCode constants, so this guard checks nothing")
	}
	var unclassified []string
	for _, code := range declared {
		if _, ok := wantPolicyCodeIsAWait[code]; !ok {
			unclassified = append(unclassified, code)
		}
	}
	sort.Strings(unclassified)
	if len(unclassified) > 0 {
		t.Fatalf("policy codes with no wait/failure decision in wantPolicyCodeIsAWait: %s", strings.Join(unclassified, ", "))
	}
	if len(declared) != len(wantPolicyCodeIsAWait) {
		t.Fatalf("%d PolicyCode constants declared, %d classified: %v", len(declared), len(wantPolicyCodeIsAWait), fmt.Sprint(declared))
	}
}
