package executor

import (
	"errors"
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"strings"
	"testing"

	"github.com/IBM/sarama"
)

// A Kafka user with Read/Write on the topic prefix but no Create cannot run a pipeline
// (docs/deployment/kafka-acls.md §5): each run names topics that do not exist yet, and
// broker auto-create needs the same Create grant. The executor used to log the refusal
// as a warning and carry on, so the run reported "running" while the sink consumed
// nothing. These tests pin the class: an ACL refusal on ANY topic pre-create in the
// executor fails the run and names the grant; any other pre-create error stays the
// best-effort warning it was.

func aclRefusal(topic string) error {
	return fmt.Errorf("failed to create topic '%s': %w", topic, &sarama.TopicError{Err: sarama.ErrTopicAuthorizationFailed})
}

// TestSinkTopicCreateRefusalFailsTheSinkStart: the refusal comes back from
// prepareSinkTopics as an error naming the topic and the grant. The control is the
// same call with a timeout instead, which still returns the topics and no error.
func TestSinkTopicCreateRefusalFailsTheSinkStart(t *testing.T) {
	withTopicPrefix(t, nil)
	const refused = "rsync.cdc-ec6d3a3b.shop.orders"

	km := newFakeSinkTopicManager(&sinkTopicEvents{})
	km.ensureErr[refused] = aclRefusal(refused)
	_, err := prepareSinkTopics(km, mongoSinkInputs("shop.customers", "shop.orders"))
	if err == nil {
		t.Fatal("prepareSinkTopics returned no error for a topic the broker refused to create; the sink would start on a topic that can never exist")
	}
	for _, want := range []string{refused, "Create", "kafka-acls.md"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error %q does not name %q", err, want)
		}
	}
	if !errors.Is(err, sarama.ErrTopicAuthorizationFailed) {
		t.Error("the error no longer unwraps to the broker's refusal")
	}

	ckm := newFakeSinkTopicManager(&sinkTopicEvents{})
	ckm.ensureErr[refused] = fmt.Errorf("failed to create topic '%s': %w", refused, sarama.ErrRequestTimedOut)
	got, cerr := prepareSinkTopics(ckm, mongoSinkInputs("shop.customers", "shop.orders"))
	if cerr != nil {
		t.Fatalf("a timeout is not a refusal and must stay a warning, got error %v", cerr)
	}
	assertTopicList(t, got, []string{partitionsLiveTopic, refused})
}

// TestEveryExecutorTopicPreCreateHandlesARefusal: every call in executor.go that
// pre-creates a topic through the Kafka manager (or a sinkTopicPreCreator) must pass
// its error through kafka.TopicCreateRefused, in the if statement that receives it.
// A new pre-create added with only a warning fails here.
func TestEveryExecutorTopicPreCreateHandlesARefusal(t *testing.T) {
	fset := token.NewFileSet()
	file, err := parser.ParseFile(fset, "executor.go", nil, 0)
	if err != nil {
		t.Fatalf("parse executor.go: %v", err)
	}
	creators := map[string]bool{
		"EnsureTopicExists": true, "EnsureTopicExistsWithConfig": true,
		"EnsureSignalTopic": true, "EnsureDDLTopic": true,
	}
	found := 0
	var unhandled []string
	ast.Inspect(file, func(n ast.Node) bool {
		ifs, ok := n.(*ast.IfStmt)
		if !ok || ifs.Init == nil {
			return true
		}
		assign, ok := ifs.Init.(*ast.AssignStmt)
		if !ok || len(assign.Rhs) != 1 {
			return true
		}
		call, ok := assign.Rhs[0].(*ast.CallExpr)
		if !ok {
			return true
		}
		sel, ok := call.Fun.(*ast.SelectorExpr)
		if !ok || !creators[sel.Sel.Name] {
			return true
		}
		found++
		handled := false
		ast.Inspect(ifs.Body, func(m ast.Node) bool {
			if s, ok := m.(*ast.SelectorExpr); ok && s.Sel.Name == "TopicCreateRefused" {
				handled = true
			}
			return !handled
		})
		if !handled {
			unhandled = append(unhandled, fmt.Sprintf("%s (%s)", fset.Position(call.Pos()), sel.Sel.Name))
		}
		return true
	})
	// Six today: signal, schema history, DDL, heartbeat, the batch data topic and the
	// CDC sink topics. A count well below that means the scan stopped matching.
	if found < 6 {
		t.Fatalf("found %d topic pre-create calls in executor.go, want at least 6; the scan no longer matches the code", found)
	}
	if len(unhandled) > 0 {
		t.Fatalf("these topic pre-creates only warn on an ACL refusal; pass the error through kafka.TopicCreateRefused and fail the run:\n  %s",
			strings.Join(unhandled, "\n  "))
	}
}
