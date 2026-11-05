/*
Copyright The Gentian OS Authors.

This Source Code Form is subject to the terms of the Mozilla Public
License, v. 2.0. If a copy of the MPL was not distributed with this
file, You can obtain one at https://mozilla.org/MPL/2.0/.

SPDX-License-Identifier: MPL-2.0
*/

// Command result-with-error refuses a function returning a controller-runtime
// Result and an error that returns anything but an empty Result together with
// a non-nil error. controller-runtime drops the Result when the error is set
// and requeues with its own backoff, so a RequeueAfter beside an error is a
// delay that never happens. A function that knows its delay logs the error and
// returns the Result with nil; one that wants the backoff returns
// Result{} and the error.
//
// The check is syntactic: an error expression other than nil next to anything
// but a literal empty Result is refused, a forwarded `res, err` included,
// because the reader cannot tell from that line which of the two is meant.
package main

import (
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
)

func main() {
	roots := os.Args[1:]
	if len(roots) == 0 {
		roots = []string{"."}
	}
	var findings []string
	for _, root := range roots {
		err := filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
			if err != nil {
				return err
			}
			if d.IsDir() {
				if name := d.Name(); name == "vendor" || name == "testdata" || (strings.HasPrefix(name, ".") && path != root) {
					return filepath.SkipDir
				}
				return nil
			}
			if !strings.HasSuffix(path, ".go") {
				return nil
			}
			found, err := check(path)
			findings = append(findings, found...)
			return err
		})
		if err != nil {
			fmt.Fprintln(os.Stderr, err)
			os.Exit(2)
		}
	}
	for _, f := range findings {
		fmt.Println(f)
	}
	if len(findings) > 0 {
		fmt.Fprintf(os.Stderr, "%d return(s) pair a Result with an error; controller-runtime ignores the Result then.\n"+
			"Return Result{} with the error for the backoff, or log the error and return the Result with nil.\n", len(findings))
		os.Exit(1)
	}
}

func check(path string) ([]string, error) {
	fset := token.NewFileSet()
	file, err := parser.ParseFile(fset, path, nil, 0)
	if err != nil {
		return nil, err
	}
	var findings []string
	var visit func(results *ast.FieldList, body *ast.BlockStmt)
	visit = func(results *ast.FieldList, body *ast.BlockStmt) {
		if body == nil {
			return
		}
		applies := returnsResultAndError(results)
		ast.Inspect(body, func(n ast.Node) bool {
			switch n := n.(type) {
			case *ast.FuncLit:
				visit(n.Type.Results, n.Body)
				return false
			case *ast.ReturnStmt:
				if applies && len(n.Results) == 2 && !isNil(n.Results[1]) && !isEmptyResult(n.Results[0]) {
					findings = append(findings, fmt.Sprintf("%s: Result returned with an error", fset.Position(n.Pos())))
				}
			}
			return true
		})
	}
	for _, decl := range file.Decls {
		if fn, ok := decl.(*ast.FuncDecl); ok {
			visit(fn.Type.Results, fn.Body)
		}
	}
	return findings, nil
}

// returnsResultAndError reports a result list of exactly (<pkg>.Result, error).
func returnsResultAndError(results *ast.FieldList) bool {
	if results == nil {
		return false
	}
	var types []ast.Expr
	for _, f := range results.List {
		n := len(f.Names)
		if n == 0 {
			n = 1
		}
		for range n {
			types = append(types, f.Type)
		}
	}
	if len(types) != 2 || !isResultType(types[0]) {
		return false
	}
	id, ok := types[1].(*ast.Ident)
	return ok && id.Name == "error"
}

func isResultType(e ast.Expr) bool {
	sel, ok := e.(*ast.SelectorExpr)
	if !ok || sel.Sel.Name != "Result" {
		return false
	}
	pkg, ok := sel.X.(*ast.Ident)
	return ok && (pkg.Name == "ctrl" || pkg.Name == "reconcile")
}

func isNil(e ast.Expr) bool {
	id, ok := e.(*ast.Ident)
	return ok && id.Name == "nil"
}

func isEmptyResult(e ast.Expr) bool {
	lit, ok := e.(*ast.CompositeLit)
	return ok && len(lit.Elts) == 0 && isResultType(lit.Type)
}
