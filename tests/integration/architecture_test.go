// Package integration holds the tests that check properties of the repository
// as a whole rather than of any one package.
//
// The architecture test in this file is the one that keeps the central safety
// claim of the platform enforceable. Documentation asserting that "the LLM
// cannot influence a signal" is worth nothing if a future change can quietly
// add the import that makes it false; this test fails the build instead.
package integration

import (
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"testing"
)

const modulePath = "github.com/udaykishoreresu/quantos"

// repoRoot walks up from the test's directory until it finds go.mod.
func repoRoot(t *testing.T) string {
	t.Helper()
	dir, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 6; i++ {
		if _, err := os.Stat(filepath.Join(dir, "go.mod")); err == nil {
			return dir
		}
		dir = filepath.Dir(dir)
	}
	t.Fatal("could not locate the repository root")
	return ""
}

// graph maps a package's import path (relative to the module, e.g.
// "internal/risk") to the module-internal packages it imports directly.
type graph map[string]map[string]bool

// buildGraph parses every non-test Go file in the module and records the
// module-internal import edges.
//
// Test files are excluded deliberately: a test may legitimately import both
// sides of a boundary in order to check that they agree. What must not happen
// is a production import edge.
func buildGraph(t *testing.T, root string) graph {
	t.Helper()
	g := graph{}
	fset := token.NewFileSet()

	err := filepath.WalkDir(root, func(path string, d os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			switch d.Name() {
			case ".git", "node_modules", "web", "ml", "vendor", "deploy", "docs":
				return filepath.SkipDir
			}
			return nil
		}
		if !strings.HasSuffix(path, ".go") || strings.HasSuffix(path, "_test.go") {
			return nil
		}
		rel, err := filepath.Rel(root, filepath.Dir(path))
		if err != nil {
			return err
		}
		rel = filepath.ToSlash(rel)

		f, err := parser.ParseFile(fset, path, nil, parser.ImportsOnly)
		if err != nil {
			return err
		}
		if g[rel] == nil {
			g[rel] = map[string]bool{}
		}
		for _, spec := range f.Imports {
			p, err := strconv.Unquote(spec.Path.Value)
			if err != nil {
				continue
			}
			if !strings.HasPrefix(p, modulePath+"/") {
				continue
			}
			g[rel][strings.TrimPrefix(p, modulePath+"/")] = true
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(g) < 20 {
		t.Fatalf("the import graph has only %d packages; the walk is not finding the source", len(g))
	}
	return g
}

// reaches returns the path from `from` to `to` through the import graph, or nil
// if there is none. Returning the path rather than a boolean means a failure
// message can name the exact chain a developer has to break.
func (g graph) reaches(from, to string) []string {
	type node struct {
		pkg  string
		path []string
	}
	seen := map[string]bool{from: true}
	queue := []node{{pkg: from, path: []string{from}}}
	for len(queue) > 0 {
		n := queue[0]
		queue = queue[1:]
		deps := make([]string, 0, len(g[n.pkg]))
		for d := range g[n.pkg] {
			deps = append(deps, d)
		}
		sort.Strings(deps) // deterministic failure messages
		for _, d := range deps {
			if d == to {
				return append(append([]string{}, n.path...), d)
			}
			if seen[d] {
				continue
			}
			seen[d] = true
			queue = append(queue, node{pkg: d, path: append(append([]string{}, n.path...), d)})
		}
	}
	return nil
}

// TestTheLLMCannotReachADecision enforces ADR-005.
//
// The platform's core promise is that no language model invents a trading
// signal. That promise is kept structurally: the packages that talk to a model
// have no way to reach the packages that decide anything, so the compiler makes
// the mistake impossible rather than the code review catching it.
func TestTheLLMCannotReachADecision(t *testing.T) {
	g := buildGraph(t, repoRoot(t))

	llmSide := []string{"internal/llm", "internal/analyst", "internal/brief"}
	decisionSide := []string{
		"internal/signal",
		"internal/risk",
		"internal/predict",
		"internal/scoring",
		"internal/rules",
		"internal/opportunity",
		"internal/paper",
	}
	for _, from := range llmSide {
		if _, ok := g[from]; !ok {
			t.Fatalf("package %s does not exist; this test is guarding nothing", from)
		}
		for _, to := range decisionSide {
			if path := g.reaches(from, to); path != nil {
				t.Errorf("%s can reach %s (%s).\n"+
					"An explanation layer that can call a decision layer can also feed it. "+
					"The narrative packages must consume finished domain values only.",
					from, to, strings.Join(path, " -> "))
			}
		}
	}
}

// TestNoDecisionPackageImportsTheLLM is the same boundary from the other side:
// nothing that computes a number may call a model to help.
func TestNoDecisionPackageImportsTheLLM(t *testing.T) {
	g := buildGraph(t, repoRoot(t))
	decisionSide := []string{
		"internal/signal", "internal/risk", "internal/predict", "internal/scoring",
		"internal/rules", "internal/features", "internal/regime", "internal/opportunity",
		"internal/paper", "internal/backtest", "internal/evaluation", "internal/quant",
		"internal/valuation", "internal/fundamentals",
	}
	for _, from := range decisionSide {
		if _, ok := g[from]; !ok {
			continue
		}
		for _, to := range []string{"internal/llm", "internal/analyst"} {
			if path := g.reaches(from, to); path != nil {
				t.Errorf("%s can reach %s (%s).\n"+
					"A quantitative layer must produce the same output for the same input, "+
					"which a model call cannot guarantee.",
					from, to, strings.Join(path, " -> "))
			}
		}
	}
}

// TestDomainDependsOnNothing keeps the shared vocabulary free of behaviour.
//
// domain is imported by every other package. The moment it imports one of them
// back, the dependency graph has a cycle in spirit even where the compiler
// permits it, and the types stop being safe to pass across boundaries.
func TestDomainDependsOnNothing(t *testing.T) {
	g := buildGraph(t, repoRoot(t))
	if deps := g["internal/domain"]; len(deps) > 0 {
		names := make([]string, 0, len(deps))
		for d := range deps {
			names = append(names, d)
		}
		sort.Strings(names)
		t.Fatalf("internal/domain imports %s; it must depend on the standard library alone",
			strings.Join(names, ", "))
	}
}

// TestObservabilityDoesNotDependOnTheApplication guards the other leaf.
func TestObservabilityDoesNotDependOnTheApplication(t *testing.T) {
	g := buildGraph(t, repoRoot(t))
	for dep := range g["internal/obs"] {
		if dep != "internal/domain" {
			t.Errorf("internal/obs imports %s; the observability layer must be usable "+
				"from anywhere, which means it may not depend on anything but domain", dep)
		}
	}
}

// TestOnlyTheCompositionRootWiresStorage keeps persistence out of the engines.
//
// An engine that can write to a database is an engine that cannot be run inside
// a backtest, a replay, or a unit test without one.
func TestOnlyTheCompositionRootWiresStorage(t *testing.T) {
	g := buildGraph(t, repoRoot(t))
	allowed := map[string]bool{
		"internal/app": true, "internal/api": true, "internal/cli": true,
		"internal/store": true, "internal/demo": true, "internal/pipeline": true,
	}
	for pkg, deps := range g {
		if !strings.HasPrefix(pkg, "internal/") || allowed[pkg] {
			continue
		}
		if deps["internal/store"] {
			t.Errorf("%s imports internal/store directly. Engines take the data they need "+
				"as arguments; only the composition root knows where it came from.", pkg)
		}
	}
}

// TestEnginesDoNotReadTheWallClock enforces the determinism rule.
//
// A decision that depends on time.Now() cannot be replayed, so every engine
// takes an obs.Clock. Measuring how long an operation took is a different thing
// and is permitted, so the check looks for time.Now used as a *value* rather
// than as the start of a duration measurement.
func TestEnginesDoNotReadTheWallClock(t *testing.T) {
	root := repoRoot(t)
	engines := []string{
		"internal/features", "internal/rules", "internal/predict", "internal/risk",
		"internal/signal", "internal/scoring", "internal/regime", "internal/opportunity",
		"internal/paper", "internal/backtest", "internal/evaluation", "internal/quant",
	}
	fset := token.NewFileSet()
	for _, pkg := range engines {
		dir := filepath.Join(root, filepath.FromSlash(pkg))
		if _, err := os.Stat(dir); err != nil {
			continue
		}
		entries, err := os.ReadDir(dir)
		if err != nil {
			t.Fatal(err)
		}
		for _, e := range entries {
			if e.IsDir() || !strings.HasSuffix(e.Name(), ".go") || strings.HasSuffix(e.Name(), "_test.go") {
				continue
			}
			path := filepath.Join(dir, e.Name())
			f, err := parser.ParseFile(fset, path, nil, 0)
			if err != nil {
				t.Fatal(err)
			}
			ast.Inspect(f, func(n ast.Node) bool {
				assign, ok := n.(*ast.AssignStmt)
				if !ok {
					return true
				}
				// `start := time.Now()` is a latency measurement, not a decision
				// input. Anything else that reads the wall clock is.
				measuring := false
				for _, lhs := range assign.Lhs {
					if id, ok := lhs.(*ast.Ident); ok {
						switch id.Name {
						case "start", "t0", "began", "_":
							measuring = true
						}
					}
				}
				if measuring {
					return true
				}
				for _, rhs := range assign.Rhs {
					if usesTimeNow(rhs) {
						pos := fset.Position(assign.Pos())
						t.Errorf("%s:%d reads the wall clock in a decision path. "+
							"Take an obs.Clock so the behaviour can be replayed.",
							strings.TrimPrefix(path, root+string(filepath.Separator)), pos.Line)
					}
				}
				return true
			})
		}
	}
}

func usesTimeNow(n ast.Node) bool {
	found := false
	ast.Inspect(n, func(x ast.Node) bool {
		call, ok := x.(*ast.CallExpr)
		if !ok {
			return true
		}
		sel, ok := call.Fun.(*ast.SelectorExpr)
		if !ok || sel.Sel.Name != "Now" {
			return true
		}
		if id, ok := sel.X.(*ast.Ident); ok && id.Name == "time" {
			found = true
		}
		return true
	})
	return found
}

// TestServicesAreThinEntrypoints enforces ADR-011: the deployable services are
// process boundaries, not places where behaviour hides. Any logic added to one
// of them is logic that no other deployment shape can reach.
func TestServicesAreThinEntrypoints(t *testing.T) {
	root := repoRoot(t)
	dir := filepath.Join(root, "services")
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Skipf("no services directory: %v", err)
	}
	const maxLines = 120
	for _, e := range entries {
		if !e.IsDir() {
			continue
		}
		files, err := filepath.Glob(filepath.Join(dir, e.Name(), "*.go"))
		if err != nil {
			t.Fatal(err)
		}
		total := 0
		for _, f := range files {
			b, err := os.ReadFile(f)
			if err != nil {
				t.Fatal(err)
			}
			total += strings.Count(string(b), "\n")
		}
		if total > maxLines {
			t.Errorf("services/%s is %d lines. A service entrypoint parses flags and calls "+
				"into internal/; behaviour that lives here cannot be tested or reused.",
				e.Name(), total)
		}
	}
}
