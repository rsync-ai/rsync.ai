package kafka

import (
	"errors"
	"go/ast"
	"go/parser"
	"go/token"
	"io/fs"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/IBM/sarama"
	kafkaclient "github.com/rsync-ai/shared/kafkaclient"
	"github.com/rsync-ai/shared/kafkaclient/saramaauth"
)

// errBrokerRefused is what the orchestrator logged eight times on the
// 2026-10-25 cold boot, once per container restart, while Kafka was still
// starting.
var errBrokerRefused = errors.New("kafka: client has run out of available brokers to talk to: dial tcp 172.18.0.5:29092: connect: connection refused")

// clearKafkaSecurityEnv leaves the security config PLAINTEXT, whatever the
// developer's shell exports.
func clearKafkaSecurityEnv(t *testing.T) {
	t.Helper()
	for _, k := range []string{
		kafkaclient.EnvBrokers, kafkaclient.EnvBootstrapServers, kafkaclient.EnvClientID,
		kafkaclient.EnvSecurityProtocol, kafkaclient.EnvSASLMechanism,
		kafkaclient.EnvSASLUsername, kafkaclient.EnvSASLPassword,
		kafkaclient.EnvSSLCALocation, kafkaclient.EnvTLSCAAlias,
		kafkaclient.EnvSSLCertLocation, kafkaclient.EnvSSLKeyLocation,
		kafkaclient.EnvSSLSkipVerify, kafkaclient.EnvSSLSkipVerifyAlias,
	} {
		t.Setenv(k, "")
	}
}

// stubKafkaStartup replaces NewManager's dial for one test and shrinks the
// wait between dials.
func stubKafkaStartup(t *testing.T, budget time.Duration, dial func(kafkaclient.Config, *sarama.Config) (sarama.Client, error)) {
	t.Helper()
	clearKafkaSecurityEnv(t)
	prevDial, prevBudget, prevWait := newKafkaClient, kafkaStartupBudget, kafkaStartupWait
	newKafkaClient = dial
	kafkaStartupBudget, kafkaStartupWait = budget, time.Millisecond
	t.Cleanup(func() { newKafkaClient, kafkaStartupBudget, kafkaStartupWait = prevDial, prevBudget, prevWait })
}

// mockBroker is one in-process broker that answers the metadata a client asks
// for on connect. Its API versions list Metadata (key 3): the mock's default
// lists only Produce and Fetch, and a client pinned to Kafka 3.3 then refuses
// to send the metadata request at all.
func mockBroker(t *testing.T) *sarama.MockBroker {
	t.Helper()
	b := sarama.NewMockBroker(t, 1)
	b.SetHandlerByMap(map[string]sarama.MockResponse{
		"MetadataRequest": sarama.NewMockMetadataResponse(t).
			SetBroker(b.Addr(), b.BrokerID()).
			SetController(b.BrokerID()),
		"ApiVersionsRequest": sarama.NewMockApiVersionsResponse(t).SetApiKeys([]sarama.ApiVersionsResponseKey{
			{ApiKey: 3, MinVersion: 0, MaxVersion: 12},
			{ApiKey: 18, MinVersion: 0, MaxVersion: 3},
		}),
	})
	t.Cleanup(b.Close)
	return b
}

// The orchestrator must outwait a Kafka that is still starting.
//
// sarama.NewClient connects to a broker before it returns. Pre-fix NewManager
// dialled once and main.go turned the refusal into log.Fatalf: on the
// 2026-10-25 cold boot on two vCPUs Kafka took about two minutes to accept
// connections, and the orchestrator crash-looped eight times before it came up.
// Every restart replays the boot schema wait and every other startup step, and
// a crash loop reads as an outage to anybody watching.
func TestNewManagerWaitsForAKafkaThatIsStillStarting(t *testing.T) {
	broker := mockBroker(t)
	var dials atomic.Int32
	stubKafkaStartup(t, 5*time.Second, func(c kafkaclient.Config, cfg *sarama.Config) (sarama.Client, error) {
		if dials.Add(1) <= 2 {
			return nil, errBrokerRefused
		}
		return saramaauth.NewClient(c, cfg)
	})

	m, err := NewManager(Config{Brokers: broker.Addr(), GroupID: "boot-test"})
	if err != nil {
		t.Fatalf("NewManager gave up on a broker that was still starting: %v", err)
	}
	defer m.Close()
	if got := dials.Load(); got != 3 {
		t.Errorf("dialled %d times, want 3: two refusals, then the connect", got)
	}
}

// Running out of budget still fails startup, with the cause: every consumer,
// the producer and the cluster admin are built on this client, so a process
// without it has nothing to run.
func TestNewManagerGivesUpOnceTheBudgetIsSpent(t *testing.T) {
	var dials atomic.Int32
	stubKafkaStartup(t, 20*time.Millisecond, func(kafkaclient.Config, *sarama.Config) (sarama.Client, error) {
		dials.Add(1)
		return nil, errBrokerRefused
	})

	m, err := NewManager(Config{Brokers: "kafka:29092", GroupID: "boot-test"})
	if err == nil {
		m.Close()
		t.Fatal("NewManager returned a manager with no Kafka behind it")
	}
	if !errors.Is(err, errBrokerRefused) {
		t.Errorf("error %q lost its cause", err)
	}
	if dials.Load() < 2 {
		t.Errorf("dialled %d times inside a %s budget, want it to retry", dials.Load(), kafkaStartupBudget)
	}
}

// A security config that can never work is not something to wait out: it
// fails at once, without a dial.
func TestNewManagerDoesNotWaitOutABadSecurityConfig(t *testing.T) {
	var dials atomic.Int32
	stubKafkaStartup(t, time.Minute, func(kafkaclient.Config, *sarama.Config) (sarama.Client, error) {
		dials.Add(1)
		return nil, errBrokerRefused
	})
	t.Setenv(kafkaclient.EnvSecurityProtocol, "NOT_A_PROTOCOL")

	start := time.Now()
	if _, err := NewManager(Config{Brokers: "kafka:29092", GroupID: "boot-test"}); err == nil {
		t.Fatal("NewManager accepted an unknown security protocol")
	}
	if dials.Load() != 0 || time.Since(start) > time.Second {
		t.Errorf("dialled %d times over %s for a config that can never connect", dials.Load(), time.Since(start))
	}
}

// The budget has to outlast the slowest boot this service has been seen to
// need: about two minutes on 2026-10-25, against 60 s in temporal-adapter's
// first version of this wait, which ran out once on that same boot.
func TestKafkaStartupBudgetOutwaitsTheObservedColdBoot(t *testing.T) {
	const observedColdBoot = 2 * time.Minute
	if kafkaStartupBudget < observedColdBoot+time.Minute {
		t.Errorf("kafkaStartupBudget is %s, want at least %s: a minute over the %s cold boot of 2026-10-25",
			kafkaStartupBudget, observedColdBoot+time.Minute, observedColdBoot)
	}
	if kafkaStartupWait <= 0 || kafkaStartupWait > 5*time.Second {
		t.Errorf("kafkaStartupWait is %s, want a few seconds", kafkaStartupWait)
	}
}

// kafkaBootDials is every orchestrator source file that dials Kafka eagerly,
// with the reason a Kafka that is still starting neither crash-loops the
// process nor switches the feature off until the next restart.
var kafkaBootDials = map[string]string{
	"internal/kafka/manager.go":                  "NewManager dials through connectKafkaClient, which waits out a Kafka that is still starting; each subscription's consumer group dials after that",
	"internal/kafka/topology.go":                 "main.go builds the topology manager only after NewManager has connected",
	"internal/agents/cdcstats/kafka_identity.go": "the stats agent opens its consumer groups on every sync pass, so a refused dial is retried on the next one",
	"internal/agents/consumer/registry.go":       "first dials after NewManager has connected, but its sync.Once keeps a refused dial for the life of the process: filed in BACKLOG.md",
}

// eagerKafkaDials are the constructors that connect to a broker before they
// return. The ...FromClient variants reuse a client that already connected.
var eagerKafkaDials = map[string]map[string]bool{
	"sarama": {
		"NewConsumerGroup": true, "NewConsumer": true, "NewClient": true,
		"NewSyncProducer": true, "NewAsyncProducer": true, "NewClusterAdmin": true,
	},
	"saramaauth": {"NewClient": true},
}

// Every orchestrator file that dials Kafka is listed in kafkaBootDials.
//
// This is the class guard for the 2026-10-25 boot: a long-running service that
// dials Kafka once at startup, with no wait or too short a one, either exits
// (NewManager, eight restarts) or runs with the feature off until somebody
// restarts it (the gateway's two consumers). A new dial fails here until its
// author says why a Kafka that is still starting cannot do either.
func TestEveryKafkaDialIsOutwaitedAtBoot(t *testing.T) {
	root := filepath.Join("..", "..")
	found := map[string][]string{}
	err := filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			if name := d.Name(); name == "vendor" || name == "testdata" || strings.HasPrefix(name, ".") && path != root {
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
		rel, err := filepath.Rel(root, path)
		if err != nil {
			return err
		}
		rel = filepath.ToSlash(rel)
		ast.Inspect(file, func(n ast.Node) bool {
			if sel, ok := n.(*ast.SelectorExpr); ok {
				if pkg, ok := sel.X.(*ast.Ident); ok && eagerKafkaDials[pkg.Name][sel.Sel.Name] {
					found[rel] = append(found[rel], pkg.Name+"."+sel.Sel.Name)
				}
			}
			return true
		})
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(found) == 0 {
		t.Fatal("found no Kafka dial at all: the walk is not reading this service's sources")
	}
	for rel, dials := range found {
		if kafkaBootDials[rel] == "" {
			t.Errorf("%s dials Kafka with %s: dial through connectKafkaClient, or add it to kafkaBootDials with the reason a Kafka that is still starting neither crash-loops the orchestrator nor leaves the feature off",
				rel, strings.Join(dials, ", "))
		}
	}
	for rel := range kafkaBootDials {
		if _, ok := found[rel]; !ok {
			t.Errorf("kafkaBootDials lists %s, which no longer dials Kafka: drop it", rel)
		}
	}
}
