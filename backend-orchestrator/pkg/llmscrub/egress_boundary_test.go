package llmscrub

import (
	"go/ast"
	"go/parser"
	"go/token"
	"io/fs"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
)

// Only llm-service talks to a model provider: its egress guard is where prompt
// fields are projected, counts masked and SQL literals taken out. Go code that
// names a provider endpoint or imports a provider SDK would send a prompt the
// guard never saw. This test reads every non-test Go file in the Go services
// and fails on either.
//
// It runs in backend-orchestrator's `go test ./...`, which CI runs for a
// change to any Go module (the `go` paths filter covers all of them).

// egressRoots are the repo-relative directories scanned.
var egressRoots = []string{
	"api-gateway",
	"backend-orchestrator",
	"backend-temporal-adapter",
	"connector-deployer",
	"shared/go",
	"shared/mcp-connectors",
}

// providerMarkers are substrings of a string literal that name a provider
// endpoint or a provider-shaped route.
var providerMarkers = []string{
	"/chat/completions",
	"/v1/completions",
	"/v1/messages",
	"api.openai.com",
	"api.anthropic.com",
	"generativelanguage.googleapis.com",
	"aiplatform.googleapis.com",
	"api.groq.com",
	"api.mistral.ai",
	"openrouter.ai",
	"api.cohere",
	"api.together",
	"bedrock-runtime",
}

// providerImports are import-path prefixes of provider SDKs.
var providerImports = []string{
	"github.com/sashabaranov/go-openai",
	"github.com/openai/",
	"github.com/anthropics/",
	"google.golang.org/genai",
	"github.com/google/generative-ai-go",
	"cloud.google.com/go/vertexai",
	"github.com/tmc/langchaingo",
	"github.com/ollama/",
	"github.com/aws/aws-sdk-go-v2/service/bedrockruntime",
}

// egressExceptions are files allowed to hold a marker, each with the Known
// issue that tracks removing it. The test fails when an exception stops being
// needed, so the list only shrinks.
var egressExceptions = map[string]string{
	// None. Keep the braces on their own lines even when empty:
	// scripts/gen_llm_egress_doc.py reads this block to publish the list.
}

// egressOffences returns what in one Go source file names a provider.
func egressOffences(fset *token.FileSet, path string, src []byte) ([]string, error) {
	f, err := parser.ParseFile(fset, path, src, parser.SkipObjectResolution)
	if err != nil {
		return nil, err
	}
	var out []string
	for _, imp := range f.Imports {
		p, _ := strconv.Unquote(imp.Path.Value)
		for _, prefix := range providerImports {
			if strings.HasPrefix(p, prefix) {
				out = append(out, "imports "+p)
			}
		}
	}
	ast.Inspect(f, func(n ast.Node) bool {
		lit, ok := n.(*ast.BasicLit)
		if !ok || lit.Kind != token.STRING {
			return true
		}
		s, err := strconv.Unquote(lit.Value)
		if err != nil {
			s = lit.Value
		}
		for _, m := range providerMarkers {
			if strings.Contains(s, m) {
				out = append(out, "names "+strconv.Quote(m)+" at line "+strconv.Itoa(fset.Position(lit.Pos()).Line))
			}
		}
		return true
	})
	return out, nil
}

func TestControl_TheScannerSeesAProviderCall(t *testing.T) {
	src := []byte(`package x
import _ "github.com/sashabaranov/go-openai"
const u = "https://api.openai.com" + "/v1/chat/completions"
`)
	got, err := egressOffences(token.NewFileSet(), "x.go", src)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 3 {
		t.Fatalf("control: want an import and two literals flagged, got %v", got)
	}
	clean, _ := egressOffences(token.NewFileSet(), "y.go", []byte(`package y
const u = "http://llm-service:5000/v1/completion"
`))
	if len(clean) != 0 {
		t.Fatalf("control: llm-service's own /v1/completion route is not a provider: %v", clean)
	}
}

func TestNoGoCodeTalksToAnLLMProvider(t *testing.T) {
	repo, err := filepath.Abs(filepath.Join("..", "..", ".."))
	if err != nil {
		t.Fatal(err)
	}
	fset := token.NewFileSet()
	scanned := map[string]int{}
	hit := map[string]bool{}
	for _, root := range egressRoots {
		dir := filepath.Join(repo, filepath.FromSlash(root))
		if _, err := os.Stat(dir); err != nil {
			t.Fatalf("scan root %s is missing (%v): the boundary would go unchecked", root, err)
		}
		err := filepath.WalkDir(dir, func(path string, d fs.DirEntry, err error) error {
			if err != nil {
				return err
			}
			if d.IsDir() {
				switch d.Name() {
				case "vendor", "node_modules", "testdata", ".git":
					return filepath.SkipDir
				}
				return nil
			}
			if !strings.HasSuffix(path, ".go") || strings.HasSuffix(path, "_test.go") {
				return nil
			}
			src, err := os.ReadFile(path)
			if err != nil {
				return err
			}
			rel, _ := filepath.Rel(repo, path)
			rel = filepath.ToSlash(rel)
			scanned[root]++
			offences, err := egressOffences(fset, rel, src)
			if err != nil {
				return nil // not ours to judge; `go vet` owns syntax
			}
			if len(offences) == 0 {
				return nil
			}
			if _, ok := egressExceptions[rel]; ok {
				hit[rel] = true
				return nil
			}
			t.Errorf("%s talks to a model provider directly (%s). Route the call through llm-service, whose egress guard projects every prompt field.",
				rel, strings.Join(offences, "; "))
			return nil
		})
		if err != nil {
			t.Fatalf("walk %s: %v", root, err)
		}
	}
	for _, root := range []string{"api-gateway", "backend-orchestrator"} {
		if scanned[root] < 50 {
			t.Fatalf("scanned only %d Go files under %s: the walk is not reading the service", scanned[root], root)
		}
	}
	for file, ki := range egressExceptions {
		if !hit[file] {
			t.Errorf("%s no longer names a provider: delete its exception here and close %s", file, ki)
		}
	}
}
