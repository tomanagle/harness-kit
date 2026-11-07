// Package architecture_test enforces the server's composition boundaries.
package architecture_test

import (
	"go/ast"
	"go/parser"
	"go/token"
	"io/fs"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
)

func TestCompositionBoundaries(t *testing.T) {
	root := filepath.Join("..", "..")
	err := filepath.WalkDir(root, func(path string, entry fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if entry.IsDir() {
			if entry.Name() == "bin" {
				return filepath.SkipDir
			}
			return nil
		}
		if !strings.HasSuffix(path, ".go") {
			return nil
		}
		fset := token.NewFileSet()
		file, err := parser.ParseFile(fset, path, nil, 0)
		if err != nil {
			return err
		}
		for _, problem := range violations(file) {
			t.Errorf("%s: %s", fset.Position(problem.pos), problem.message)
		}
		if strings.HasSuffix(path, "_test.go") {
			return nil
		}
		for _, imp := range file.Imports {
			name, _ := strconv.Unquote(imp.Path.Value)
			if name == "github.com/jackc/pgx/v5/pgxpool" && filepath.Base(filepath.Dir(path)) != "database" {
				t.Errorf("%s: pool construction belongs in internal/database; consume a narrow interface", path)
			}
			if name == "crypto/rand" && file.Name.Name != "main" {
				t.Errorf("%s: inject an entropy source from main", path)
			}
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
}

type violation struct {
	pos     token.Pos
	message string
}

func violations(file *ast.File) []violation {
	var out []violation
	// Inspect every declaration, including initializers. Function literals never
	// inherit main's privilege: factories, callbacks, and handlers cannot hide Must.
	var inspect func(ast.Node, string, bool)
	inspect = func(node ast.Node, function string, main bool) {
		directCalls := map[*ast.Ident]bool{}
		ast.Inspect(node, func(n ast.Node) bool {
			if n == nil {
				return false
			}
			if literal, ok := n.(*ast.FuncLit); ok {
				inspect(literal.Body, "", false)
				return false
			}
			if call, ok := n.(*ast.CallExpr); ok {
				if name := calledIdentifier(call.Fun); name != nil {
					directCalls[name] = true
				}
				if name, ok := call.Fun.(*ast.Ident); ok && name.Name == "panic" && !strings.HasPrefix(function, "Must") {
					out = append(out, violation{call.Pos(), "panic is allowed only in Must functions"})
				}
			}
			// Check references, not just calls: aliasing MustNew or passing it as a
			// callback must not let runtime code bypass the rule.
			if id, ok := n.(*ast.Ident); ok && strings.HasPrefix(id.Name, "Must") && (!main || !directCalls[id]) {
				out = append(out, violation{id.Pos(), "Must functions may be referenced only directly inside func main"})
			}
			return true
		})
	}
	for _, decl := range file.Decls {
		if fn, ok := decl.(*ast.FuncDecl); ok {
			if fn.Body != nil {
				inspect(fn.Body, fn.Name.Name, file.Name.Name == "main" && fn.Name.Name == "main" && fn.Recv == nil)
			}
		} else {
			inspect(decl, "", false)
		}
	}
	return out
}

func TestRuleDetectsIndirectMustUsageAndRuntimePanics(t *testing.T) {
	for _, tc := range []struct {
		name, source string
		invalid      bool
	}{
		{"main", `package main;func main(){logger.MustNew()}`, false},
		{"startup wrapper", `package logger;func MustNew(){panic("invalid")}`, false},
		{"runtime panic", `package auth;func Token(){panic("entropy")}`, true},
		{"runtime must", `package auth;func Token(){logger.MustNew()}`, true},
		{"main helper", `package main;func run(){logger.MustNew()}`, true},
		{"alias", `package service;var create=logger.MustNew`, true},
		{"callback", `package main;func main(){go func(){logger.MustNew()}()}`, true},
		{"main alias", `package main;func main(){create:=logger.MustNew; create()}`, true},
		{"main factory", `package main;func main(){register(logger.MustNew)}`, true},
		{"must chaining", `package service;func MustNew(){logger.MustNew()}`, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			file, err := parser.ParseFile(token.NewFileSet(), "fixture.go", tc.source, 0)
			if err != nil {
				t.Fatal(err)
			}
			if got := len(violations(file)) > 0; got != tc.invalid {
				t.Fatalf("violations=%v, want invalid=%v", violations(file), tc.invalid)
			}
		})
	}
}

// Resolve direct callees, including parenthesized and generic function calls.
func calledIdentifier(expr ast.Expr) *ast.Ident {
	switch value := ast.Unparen(expr).(type) {
	case *ast.Ident:
		return value
	case *ast.SelectorExpr:
		return value.Sel
	case *ast.IndexExpr:
		return calledIdentifier(value.X)
	case *ast.IndexListExpr:
		return calledIdentifier(value.X)
	default:
		return nil
	}
}
