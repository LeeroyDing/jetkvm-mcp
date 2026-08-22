package ci_test

import (
	"bufio"
	"fmt"
	"go/ast"
	"go/build"
	"go/parser"
	"go/token"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"testing"
)

const (
	defaultFuzzBudget = 50_000
	hidFuzzBudget     = 500_000
	slowFuzzBudget    = 2_500
)

// expectedFuzzTargets freezes both the audited target surface and its calibrated
// count budget. Exact budgets guard coverage on fast targets and shard timeouts
// on slow targets; changing either requires an explicit policy update here.
var expectedFuzzTargets = map[string]int{
	"./cmd/jetkvmctl FuzzParseCommandFlags":                      defaultFuzzBudget,
	"./cmd/jetkvmctl FuzzParseKeychainPassword":                  defaultFuzzBudget,
	"./cmd/jetkvmctl FuzzParseReadTextRegion":                    defaultFuzzBudget,
	"./internal/hidproto FuzzEncodeDecode":                       hidFuzzBudget,
	"./internal/hidproto FuzzMessageRoundTrip":                   hidFuzzBudget,
	"./internal/jetkvm FuzzBuildPointerDragReports":              defaultFuzzBudget,
	"./internal/jetkvm FuzzCanonicalBaseURL":                     defaultFuzzBudget,
	"./internal/jetkvm FuzzChangedPixelFractionImageBacking":     defaultFuzzBudget,
	"./internal/jetkvm FuzzCheckDeviceMetadata":                  defaultFuzzBudget,
	"./internal/jetkvm FuzzHTTPResponseHandling":                 defaultFuzzBudget,
	"./internal/jetkvm FuzzKeyCombo":                             defaultFuzzBudget,
	"./internal/jetkvm FuzzRPCHandleMessage":                     defaultFuzzBudget,
	"./internal/jetkvm FuzzResolveKeySequence":                   defaultFuzzBudget,
	"./internal/jetkvm FuzzResolveMouseButton":                   defaultFuzzBudget,
	"./internal/jetkvm FuzzTypeStringMapping":                    defaultFuzzBudget,
	"./internal/jetkvm FuzzValidateHoldMS":                       defaultFuzzBudget,
	"./internal/jetkvm FuzzValidateKeyCombo":                     defaultFuzzBudget,
	"./internal/jetkvm FuzzValidateKeySequenceLength":            defaultFuzzBudget,
	"./internal/jetkvm FuzzValidateKeypress":                     defaultFuzzBudget,
	"./internal/jetkvm FuzzValidatePointer":                      defaultFuzzBudget,
	"./internal/jetkvm FuzzValidatePointerDragReports":           defaultFuzzBudget,
	"./internal/jetkvm FuzzValidateScreenshotDimensions":         defaultFuzzBudget,
	"./internal/jetkvm FuzzValidateScroll":                       defaultFuzzBudget,
	"./internal/jetkvm FuzzValidateTypeDelay":                    defaultFuzzBudget,
	"./internal/jetkvm FuzzWaitForTextDurationValidation":        defaultFuzzBudget,
	"./internal/jetkvm FuzzWaitForTextTextAndRegex":              defaultFuzzBudget,
	"./internal/jetkvm FuzzWaitStableOptionValidation":           defaultFuzzBudget,
	"./internal/mcpserver FuzzDoubleClickToolArgumentValidation": slowFuzzBudget,
	"./internal/mcpserver FuzzDragToolArgumentValidation":        slowFuzzBudget,
	"./internal/mcpserver FuzzHoldKeyToolArgumentValidation":     slowFuzzBudget,
	"./internal/mcpserver FuzzKeypressToolArgumentValidation":    slowFuzzBudget,
	"./internal/mcpserver FuzzKeySequenceToolArgumentValidation": slowFuzzBudget,
	"./internal/mcpserver FuzzMouseButtonToolArgumentValidation": slowFuzzBudget,
	"./internal/mcpserver FuzzReadTextToolArgumentValidation":    slowFuzzBudget,
	"./internal/mcpserver FuzzRenderScreenshotForText":           defaultFuzzBudget,
	"./internal/mcpserver FuzzScreenshotCapturedPNGConfig":       defaultFuzzBudget,
	"./internal/mcpserver FuzzScreenshotOptionsAndRender":        defaultFuzzBudget,
	"./internal/mcpserver FuzzScrollToolArgumentValidation":      slowFuzzBudget,
	"./internal/mcpserver FuzzWaitForTextDurationArgs":           defaultFuzzBudget,
	"./internal/mcpserver FuzzWaitForTextTextAndRegexArgs":       defaultFuzzBudget,
	"./internal/mcpserver FuzzWaitStableArgs":                    defaultFuzzBudget,
}

type fuzzWorkflowTarget struct {
	budget int
}

func TestFuzzWorkflowTargetBudgets(t *testing.T) {
	repoRoot := repositoryRoot(t)
	workflowPath := filepath.Join(repoRoot, ".github", "workflows", "ci.yml")
	workflow, err := os.ReadFile(workflowPath)
	if err != nil {
		t.Fatalf("read CI workflow: %v", err)
	}

	workflowText := string(workflow)
	for _, required := range []string{
		"read -r target package budget extra",
		`-fuzztime="$budget"`,
	} {
		if !strings.Contains(workflowText, required) {
			t.Fatalf("CI fuzz loop does not contain %q", required)
		}
	}

	workflowTargets := parseWorkflowTargets(t, workflowText)
	sourceTargets := findSourceFuzzTargets(t, repoRoot)

	if len(workflowTargets) != len(expectedFuzzTargets) {
		t.Errorf("CI matrix contains %d fuzz targets, want %d", len(workflowTargets), len(expectedFuzzTargets))
	}
	if len(sourceTargets) != len(expectedFuzzTargets) {
		t.Errorf("source contains %d fuzz targets, want %d", len(sourceTargets), len(expectedFuzzTargets))
	}

	for key, expectedBudget := range expectedFuzzTargets {
		if _, ok := sourceTargets[key]; !ok {
			t.Errorf("expected fuzz target %s is missing from source", key)
		}
		target, ok := workflowTargets[key]
		if !ok {
			t.Errorf("expected fuzz target %s is missing from the CI matrix", key)
			continue
		}
		if target.budget != expectedBudget {
			t.Errorf("CI budget for %s = %dx, want calibrated %dx", key, target.budget, expectedBudget)
		}
	}

	for key := range workflowTargets {
		if _, ok := expectedFuzzTargets[key]; !ok {
			t.Errorf("CI matrix contains unexpected fuzz target %s", key)
		}
	}
	for key := range sourceTargets {
		if _, ok := expectedFuzzTargets[key]; !ok {
			t.Errorf("source contains unexpected fuzz target %s", key)
		}
	}
}

func parseWorkflowTargets(t *testing.T, workflow string) map[string]fuzzWorkflowTarget {
	t.Helper()
	linePattern := regexp.MustCompile(`^ {14}(Fuzz[A-Za-z0-9_]+)\s+(\./\S+)\s+([1-9][0-9]*)x\s*$`)
	jobPattern := regexp.MustCompile(`^  [A-Za-z0-9_-]+:$`)
	targets := make(map[string]fuzzWorkflowTarget)
	scanner := bufio.NewScanner(strings.NewReader(workflow))
	inFuzzJob := false
	foundFuzzJob := false
	inTargetsBlock := false
	targetBlocks := 0
	for scanner.Scan() {
		line := scanner.Text()
		if line == "  fuzz:" {
			if foundFuzzJob {
				t.Fatal("CI workflow contains duplicate fuzz jobs")
			}
			foundFuzzJob = true
			inFuzzJob = true
			continue
		}
		if inFuzzJob && jobPattern.MatchString(line) {
			inFuzzJob = false
			inTargetsBlock = false
		}
		if !inFuzzJob {
			continue
		}
		if line == "            targets: |-" {
			inTargetsBlock = true
			targetBlocks++
			continue
		}
		if !inTargetsBlock {
			continue
		}
		match := linePattern.FindStringSubmatch(line)
		if match == nil {
			inTargetsBlock = false
			continue
		}
		budget, err := strconv.Atoi(match[3])
		if err != nil {
			t.Fatalf("parse fuzz budget on line %q: %v", line, err)
		}
		key := fuzzTargetKey(match[2], match[1])
		if _, duplicate := targets[key]; duplicate {
			t.Fatalf("duplicate CI fuzz target %s", key)
		}
		targets[key] = fuzzWorkflowTarget{budget: budget}
	}
	if err := scanner.Err(); err != nil {
		t.Fatalf("scan CI workflow: %v", err)
	}
	if !foundFuzzJob {
		t.Fatal("CI workflow has no fuzz job")
	}
	if targetBlocks != 4 {
		t.Fatalf("CI matrix contains %d fuzz target blocks, want 4", targetBlocks)
	}
	return targets
}

func findSourceFuzzTargets(t *testing.T, repoRoot string) map[string]struct{} {
	t.Helper()
	targets := make(map[string]struct{})
	ciBuild := build.Default
	ciBuild.GOOS = "linux"
	ciBuild.GOARCH = "amd64"
	ciBuild.CgoEnabled = true
	err := filepath.WalkDir(repoRoot, func(path string, entry fs.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		if entry.IsDir() {
			if path != repoRoot && (entry.Name() == "vendor" || entry.Name() == "testdata" || strings.HasPrefix(entry.Name(), ".") || strings.HasPrefix(entry.Name(), "_")) {
				return filepath.SkipDir
			}
			return nil
		}
		if !strings.HasSuffix(entry.Name(), "_test.go") {
			return nil
		}
		matched, err := ciBuild.MatchFile(filepath.Dir(path), entry.Name())
		if err != nil {
			return fmt.Errorf("match build constraints for %s: %w", path, err)
		}
		if !matched {
			return nil
		}

		file, err := parser.ParseFile(token.NewFileSet(), path, nil, parser.SkipObjectResolution)
		if err != nil {
			return fmt.Errorf("parse %s: %w", path, err)
		}
		relativeDir, err := filepath.Rel(repoRoot, filepath.Dir(path))
		if err != nil {
			return fmt.Errorf("resolve package for %s: %w", path, err)
		}
		packagePath := "./" + filepath.ToSlash(relativeDir)
		testingNames := testingImportNames(file)
		for _, declaration := range file.Decls {
			function, ok := declaration.(*ast.FuncDecl)
			if !ok || !isFuzzTarget(function, testingNames) {
				continue
			}
			name := function.Name.Name
			key := fuzzTargetKey(packagePath, name)
			if _, duplicate := targets[key]; duplicate {
				return fmt.Errorf("duplicate fuzz target %s", key)
			}
			targets[key] = struct{}{}
		}
		return nil
	})
	if err != nil {
		t.Fatalf("find source fuzz targets: %v", err)
	}
	return targets
}

func fuzzTargetKey(packagePath, name string) string {
	return packagePath + " " + name
}

func testingImportNames(file *ast.File) map[string]struct{} {
	names := make(map[string]struct{})
	for _, spec := range file.Imports {
		path, err := strconv.Unquote(spec.Path.Value)
		if err != nil || path != "testing" {
			continue
		}
		name := "testing"
		if spec.Name != nil {
			name = spec.Name.Name
		}
		names[name] = struct{}{}
	}
	return names
}

func isFuzzTarget(function *ast.FuncDecl, testingNames map[string]struct{}) bool {
	if function.Recv != nil || !strings.HasPrefix(function.Name.Name, "Fuzz") {
		return false
	}
	if function.Type.Params == nil || len(function.Type.Params.List) != 1 {
		return false
	}
	pointer, ok := function.Type.Params.List[0].Type.(*ast.StarExpr)
	if !ok {
		return false
	}
	selector, ok := pointer.X.(*ast.SelectorExpr)
	if ok && selector.Sel.Name == "F" {
		qualifier, ok := selector.X.(*ast.Ident)
		if !ok {
			return false
		}
		_, ok = testingNames[qualifier.Name]
		return ok
	}
	identifier, ok := pointer.X.(*ast.Ident)
	if !ok || identifier.Name != "F" {
		return false
	}
	_, dotImported := testingNames["."]
	return dotImported
}

func repositoryRoot(t *testing.T) string {
	t.Helper()
	directory, err := os.Getwd()
	if err != nil {
		t.Fatalf("get working directory: %v", err)
	}
	for {
		if _, err := os.Stat(filepath.Join(directory, "go.mod")); err == nil {
			if _, err := os.Stat(filepath.Join(directory, ".github", "workflows", "ci.yml")); err == nil {
				return directory
			}
		}
		parent := filepath.Dir(directory)
		if parent == directory {
			t.Fatal("repository root not found")
		}
		directory = parent
	}
}
