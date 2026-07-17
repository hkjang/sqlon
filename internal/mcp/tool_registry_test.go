package mcp

import (
	"context"
	"encoding/json"
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"testing"
)

func TestDirectDBAMutationToolsAreInternalOnly(t *testing.T) {
	public := toolNameSet(t, "public tools", registeredToolNames(t))
	for name := range internalDBAExecutors {
		if _, ok := public[name]; ok {
			t.Fatalf("internal DBA executor %q is publicly advertised", name)
		}
		params, err := json.Marshal(map[string]any{"name": name, "arguments": map[string]any{}})
		if err != nil {
			t.Fatal(err)
		}
		got, err := (&Server{}).callTool(context.Background(), params)
		if err != nil {
			t.Fatal(err)
		}
		result, ok := got.(map[string]any)
		if !ok || result["status"] != "deprecated" {
			t.Fatalf("direct call %q was not rejected: %#v", name, got)
		}
	}
}

func TestToolRegistryMatchesDispatcher(t *testing.T) {
	registry := registeredToolNames(t)
	dispatch := dispatchedToolNames(t, "server.go")
	assertSameToolNames(t, "tools registry", registry, "callTool dispatcher", dispatch)
}

func TestREADMEListsEveryRegisteredTool(t *testing.T) {
	registry := registeredToolNames(t)
	readmePath := filepath.Join("..", "..", "README.md")
	contents, err := os.ReadFile(readmePath)
	if err != nil {
		t.Fatalf("read %s: %v", readmePath, err)
	}
	readme := strings.ReplaceAll(string(contents), "\r\n", "\n")

	countPattern := regexp.MustCompile(`MCP 도구\s+레퍼런스\((\d+)종\)`)
	countMatch := countPattern.FindStringSubmatch(readme)
	if countMatch == nil {
		t.Fatalf("README must declare the MCP tool reference count")
	}
	documentedCount, err := strconv.Atoi(countMatch[1])
	if err != nil {
		t.Fatalf("parse README tool count %q: %v", countMatch[1], err)
	}
	if documentedCount != len(registry) {
		t.Fatalf("README declares %d MCP tools; registry contains %d", documentedCount, len(registry))
	}

	toolsSection, ok := markdownSection(readme, "## Tools")
	if !ok {
		t.Fatalf("README is missing the ## Tools section")
	}
	var documented []string
	toolNamePattern := regexp.MustCompile("`([a-z][a-z0-9_]*)`")
	for _, line := range strings.Split(toolsSection, "\n") {
		if !strings.HasPrefix(line, "- `") {
			continue
		}
		label, _, _ := strings.Cut(line, " \u2014 ")
		for _, match := range toolNamePattern.FindAllStringSubmatch(label, -1) {
			documented = append(documented, match[1])
		}
	}
	assertSameToolNames(t, "tools registry", registry, "README ## Tools", documented)
}

func registeredToolNames(t *testing.T) []string {
	t.Helper()
	definitions := (&Server{}).tools()
	names := make([]string, 0, len(definitions))
	seen := make(map[string]struct{}, len(definitions))
	for i, definition := range definitions {
		name, ok := definition["name"].(string)
		if !ok || name == "" {
			t.Fatalf("tool definition %d has invalid name %#v", i, definition["name"])
		}
		if _, exists := seen[name]; exists {
			t.Fatalf("tool registry contains duplicate name %q", name)
		}
		seen[name] = struct{}{}
		names = append(names, name)
	}
	return names
}

func dispatchedToolNames(t *testing.T, filename string) []string {
	t.Helper()
	fset := token.NewFileSet()
	file, err := parser.ParseFile(fset, filename, nil, 0)
	if err != nil {
		t.Fatalf("parse %s: %v", filename, err)
	}

	var callTool *ast.FuncDecl
	for _, declaration := range file.Decls {
		fn, ok := declaration.(*ast.FuncDecl)
		if ok && fn.Name.Name == "callTool" {
			callTool = fn
			break
		}
	}
	if callTool == nil {
		t.Fatalf("%s does not define callTool", filename)
	}

	var names []string
	found := false
	ast.Inspect(callTool.Body, func(node ast.Node) bool {
		switchStmt, ok := node.(*ast.SwitchStmt)
		if !ok || !isRequestName(switchStmt.Tag) {
			return true
		}
		found = true
		for _, statement := range switchStmt.Body.List {
			clause := statement.(*ast.CaseClause)
			for _, expression := range clause.List {
				literal, ok := expression.(*ast.BasicLit)
				if !ok || literal.Kind != token.STRING {
					t.Fatalf("callTool has a non-string dispatch case at %s", fset.Position(expression.Pos()))
				}
				name, err := strconv.Unquote(literal.Value)
				if err != nil {
					t.Fatalf("decode dispatch case %s: %v", literal.Value, err)
				}
				if !internalDBAExecutors[name] {
					names = append(names, name)
				}
			}
		}
		return false
	})
	if !found {
		t.Fatalf("callTool does not dispatch on req.Name")
	}
	return names
}

func isRequestName(expression ast.Expr) bool {
	selector, ok := expression.(*ast.SelectorExpr)
	if !ok || selector.Sel.Name != "Name" {
		return false
	}
	receiver, ok := selector.X.(*ast.Ident)
	return ok && receiver.Name == "req"
}

func markdownSection(markdown, heading string) (string, bool) {
	start := strings.Index(markdown, heading+"\n")
	if start < 0 {
		return "", false
	}
	section := markdown[start+len(heading)+1:]
	if end := strings.Index(section, "\n## "); end >= 0 {
		section = section[:end]
	}
	return section, true
}

func assertSameToolNames(t *testing.T, wantLabel string, want []string, gotLabel string, got []string) {
	t.Helper()
	wantSet := toolNameSet(t, wantLabel, want)
	gotSet := toolNameSet(t, gotLabel, got)

	var missing, extra []string
	for name := range wantSet {
		if _, ok := gotSet[name]; !ok {
			missing = append(missing, name)
		}
	}
	for name := range gotSet {
		if _, ok := wantSet[name]; !ok {
			extra = append(extra, name)
		}
	}
	sort.Strings(missing)
	sort.Strings(extra)
	if len(missing) != 0 || len(extra) != 0 {
		t.Fatalf("%s and %s differ: missing=%v extra=%v", wantLabel, gotLabel, missing, extra)
	}
}

func toolNameSet(t *testing.T, label string, names []string) map[string]struct{} {
	t.Helper()
	set := make(map[string]struct{}, len(names))
	for _, name := range names {
		if _, exists := set[name]; exists {
			t.Fatalf("%s contains duplicate tool %q", label, name)
		}
		set[name] = struct{}{}
	}
	return set
}
