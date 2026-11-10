package kafka

import (
	"context"
	"errors"
	"go/ast"
	"go/parser"
	"go/token"
	"io/fs"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/IBM/sarama"
)

func TestConsumerGroupDialBackoffDoublesToItsCap(t *testing.T) {
	b := ConsumerGroupDialBackoff{Initial: 2 * time.Second, Max: 30 * time.Second}
	var got []time.Duration
	for wait := b.Initial; len(got) < 6; wait = b.next(wait) {
		got = append(got, wait)
	}
	want := []time.Duration{2 * time.Second, 4 * time.Second, 8 * time.Second, 16 * time.Second, 30 * time.Second, 30 * time.Second}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("waits %v, want %v", got, want)
		}
	}
}

func TestConnectConsumerGroupReturnsTheContextErrorWhenItEnds(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	refused := errors.New("dial tcp 127.0.0.1:1: connect: connection refused")
	dials := 0
	group, err := ConnectConsumerGroup(ctx, "test consumer", ConsumerGroupDialBackoff{Initial: time.Millisecond, Max: time.Millisecond},
		func() (sarama.ConsumerGroup, error) {
			if dials++; dials == 3 {
				cancel()
			}
			return nil, refused
		})
	if group != nil || !errors.Is(err, context.Canceled) {
		t.Fatalf("got (%v, %v), want (nil, context.Canceled)", group, err)
	}
	if dials != 3 {
		t.Errorf("dialled %d times after the context ended at the third", dials)
	}
}

// eagerSaramaDials are the sarama constructors that connect to a broker
// before they return.
var eagerSaramaDials = map[string]bool{
	"NewConsumerGroup": true, "NewConsumer": true, "NewClient": true,
	"NewSyncProducer": true, "NewAsyncProducer": true, "NewClusterAdmin": true,
}

// Every sarama dial in this service must go through ConnectConsumerGroup.
//
// This is the class guard for the 2026-10-25 boot: a long-running consumer
// that dials Kafka once at startup is off for the life of the process whenever
// the gateway starts before Kafka does. The kafka-go Readers and Writers are
// not in scope: they connect on first use and redial on their own.
//
// The rule, per file: a file that names an eager sarama constructor must also
// call ConnectConsumerGroup. A file that dials without a retry fails here.
func TestEverySaramaDialRetriesUntilKafkaAnswers(t *testing.T) {
	root := filepath.Join("..", "..")
	sites := 0
	err := filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			if name := d.Name(); name == "vendor" || name == "node_modules" || strings.HasPrefix(name, ".") && path != root {
				return filepath.SkipDir
			}
			return nil
		}
		if !strings.HasSuffix(path, ".go") || strings.HasSuffix(path, "_test.go") {
			return nil
		}
		file, err := parser.ParseFile(token.NewFileSet(), path, nil, 0)
		if err != nil {
			return err
		}
		var dials []string
		retries := false
		ast.Inspect(file, func(n ast.Node) bool {
			switch n := n.(type) {
			case *ast.SelectorExpr:
				if pkg, ok := n.X.(*ast.Ident); ok && pkg.Name == "sarama" && eagerSaramaDials[n.Sel.Name] {
					dials = append(dials, "sarama."+n.Sel.Name)
				}
				if n.Sel.Name == "ConnectConsumerGroup" {
					retries = true
				}
			case *ast.CallExpr:
				if id, ok := n.Fun.(*ast.Ident); ok && id.Name == "ConnectConsumerGroup" {
					retries = true
				}
			}
			return true
		})
		sites += len(dials)
		if len(dials) > 0 && !retries {
			t.Errorf("%s dials Kafka with %s but never calls ConnectConsumerGroup: started before Kafka, it stays off until a restart",
				path, strings.Join(dials, ", "))
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if sites == 0 {
		t.Fatal("found no sarama dial at all: the walk is not reading this service's sources")
	}
}
