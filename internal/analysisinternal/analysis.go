// Copyright 2020 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

// Package analysisinternal provides gopls' internal analyses with a
// number of helper functions that operate on typed syntax trees.
package analysisinternal

import (
	"bytes"
	"fmt"
	"go/ast"
	"go/printer"
	"go/scanner"
	"go/token"
	"go/types"
	"os"
	pathpkg "path"
	"slices"
	"strings"

	"golang.org/x/tools/go/analysis"
	"golang.org/x/tools/internal/typesinternal"
)

func TypeErrorEndPos(fset *token.FileSet, src []byte, start token.Pos) token.Pos {
	// Get the end position for the type error.
	file := fset.File(start)
	if file == nil {
		return start
	}
	if offset := file.PositionFor(start, false).Offset; offset > len(src) {
		return start
	} else {
		src = src[offset:]
	}

	// Attempt to find a reasonable end position for the type error.
	//
	// TODO(rfindley): the heuristic implemented here is unclear. It looks like
	// it seeks the end of the primary operand starting at start, but that is not
	// quite implemented (for example, given a func literal this heuristic will
	// return the range of the func keyword).
	//
	// We should formalize this heuristic, or deprecate it by finally proposing
	// to add end position to all type checker errors.
	//
	// Nevertheless, ensure that the end position at least spans the current
	// token at the cursor (this was golang/go#69505).
	end := start
	{
		var s scanner.Scanner
		fset := token.NewFileSet()
		f := fset.AddFile("", fset.Base(), len(src))
		s.Init(f, src, nil /* no error handler */, scanner.ScanComments)
		pos, tok, lit := s.Scan()
		if tok != token.SEMICOLON && token.Pos(f.Base()) <= pos && pos <= token.Pos(f.Base()+f.Size()) {
			off := file.Offset(pos) + len(lit)
			src = src[off:]
			end += token.Pos(off)
		}
	}

	// Look for bytes that might terminate the current operand. See note above:
	// this is imprecise.
	if width := bytes.IndexAny(src, " \n,():;[]+-*/"); width > 0 {
		end += token.Pos(width)
	}
	return end
}

// WalkASTWithParent walks the AST rooted at n. The semantics are
// similar to ast.Inspect except it does not call f(nil).
func WalkASTWithParent(n ast.Node, f func(n ast.Node, parent ast.Node) bool) {
	var ancestors []ast.Node
	ast.Inspect(n, func(n ast.Node) (recurse bool) {
		if n == nil {
			ancestors = ancestors[:len(ancestors)-1]
			return false
		}

		var parent ast.Node
		if len(ancestors) > 0 {
			parent = ancestors[len(ancestors)-1]
		}
		ancestors = append(ancestors, n)
		return f(n, parent)
	})
}

// MatchingIdents finds the names of all identifiers in 'node' that match any of the given types.
// 'pos' represents the position at which the identifiers may be inserted. 'pos' must be within
// the scope of each of identifier we select. Otherwise, we will insert a variable at 'pos' that
// is unrecognized.
func MatchingIdents(typs []types.Type, node ast.Node, pos token.Pos, info *types.Info, pkg *types.Package) map[types.Type][]string {

	// Initialize matches to contain the variable types we are searching for.
	matches := make(map[types.Type][]string)
	for _, typ := range typs {
		if typ == nil {
			continue // TODO(adonovan): is this reachable?
		}
		matches[typ] = nil // create entry
	}

	seen := map[types.Object]struct{}{}
	ast.Inspect(node, func(n ast.Node) bool {
		if n == nil {
			return false
		}
		// Prevent circular definitions. If 'pos' is within an assignment statement, do not
		// allow any identifiers in that assignment statement to be selected. Otherwise,
		// we could do the following, where 'x' satisfies the type of 'f0':
		//
		// x := fakeStruct{f0: x}
		//
		if assign, ok := n.(*ast.AssignStmt); ok && pos > assign.Pos() && pos <= assign.End() {
			return false
		}
		if n.End() > pos {
			return n.Pos() <= pos
		}
		ident, ok := n.(*ast.Ident)
		if !ok || ident.Name == "_" {
			return true
		}
		obj := info.Defs[ident]
		if obj == nil || obj.Type() == nil {
			return true
		}
		if _, ok := obj.(*types.TypeName); ok {
			return true
		}
		// Prevent duplicates in matches' values.
		if _, ok = seen[obj]; ok {
			return true
		}
		seen[obj] = struct{}{}
		// Find the scope for the given position. Then, check whether the object
		// exists within the scope.
		innerScope := pkg.Scope().Innermost(pos)
		if innerScope == nil {
			return true
		}
		_, foundObj := innerScope.LookupParent(ident.Name, pos)
		if foundObj != obj {
			return true
		}
		// The object must match one of the types that we are searching for.
		// TODO(adonovan): opt: use typeutil.Map?
		if names, ok := matches[obj.Type()]; ok {
			matches[obj.Type()] = append(names, ident.Name)
		} else {
			// If the object type does not exactly match
			// any of the target types, greedily find the first
			// target type that the object type can satisfy.
			for typ := range matches {
				if equivalentTypes(obj.Type(), typ) {
					matches[typ] = append(matches[typ], ident.Name)
				}
			}
		}
		return true
	})
	return matches
}

func equivalentTypes(want, got types.Type) bool {
	if types.Identical(want, got) {
		return true
	}
	// Code segment to help check for untyped equality from (golang/go#32146).
	if rhs, ok := want.(*types.Basic); ok && rhs.Info()&types.IsUntyped > 0 {
		if lhs, ok := got.Underlying().(*types.Basic); ok {
			return rhs.Info()&types.IsConstType == lhs.Info()&types.IsConstType
		}
	}
	return types.AssignableTo(want, got)
}

// MakeReadFile returns a simple implementation of the Pass.ReadFile function.
func MakeReadFile(pass *analysis.Pass) func(filename string) ([]byte, error) {
	return func(filename string) ([]byte, error) {
		if err := CheckReadable(pass, filename); err != nil {
			return nil, err
		}
		return os.ReadFile(filename)
	}
}

// CheckReadable enforces the access policy defined by the ReadFile field of [analysis.Pass].
func CheckReadable(pass *analysis.Pass, filename string) error {
	if slicesContains(pass.OtherFiles, filename) ||
		slicesContains(pass.IgnoredFiles, filename) {
		return nil
	}
	for _, f := range pass.Files {
		if pass.Fset.File(f.FileStart).Name() == filename {
			return nil
		}
	}
	return fmt.Errorf("Pass.ReadFile: %s is not among OtherFiles, IgnoredFiles, or names of Files", filename)
}

// TODO(adonovan): use go1.21 slices.Contains.
func slicesContains[S ~[]E, E comparable](slice S, x E) bool {
	for _, elem := range slice {
		if elem == x {
			return true
		}
	}
	return false
}

// AddImport checks whether this file already imports pkgpath and
// that import is in scope at pos. If so, it returns the name under
// which it was imported and a zero edit. Otherwise, it adds a new
// import of pkgpath, using a name derived from the preferred name,
// and returns the chosen name along with the edit for the new import.
//
// It does not mutate its arguments.
func AddImport(info *types.Info, file *ast.File, pos token.Pos, pkgpath, preferredName string) (name string, newImport []analysis.TextEdit) {
	// Find innermost enclosing lexical block.
	scope := info.Scopes[file].Innermost(pos)
	if scope == nil {
		panic("no enclosing lexical block")
	}

	// Is there an existing import of this package?
	// If so, are we in its scope? (not shadowed)
	for _, spec := range file.Imports {
		pkgname := info.PkgNameOf(spec)
		if pkgname != nil && pkgname.Imported().Path() == pkgpath {
			if _, obj := scope.LookupParent(pkgname.Name(), pos); obj == pkgname {
				return pkgname.Name(), nil
			}
		}
	}

	// We must add a new import.
	// Ensure we have a fresh name.
	newName := preferredName
	for i := 0; ; i++ {
		if _, obj := scope.LookupParent(newName, pos); obj == nil {
			break // fresh
		}
		newName = fmt.Sprintf("%s%d", preferredName, i)
	}

	// For now, keep it real simple: create a new import
	// declaration before the first existing declaration (which
	// must exist), including its comments, and let goimports tidy it up.
	//
	// Use a renaming import whenever the preferred name is not
	// available, or the chosen name does not match the last
	// segment of its path.
	newText := fmt.Sprintf("import %q\n\n", pkgpath)
	if newName != preferredName || newName != pathpkg.Base(pkgpath) {
		newText = fmt.Sprintf("import %s %q\n\n", newName, pkgpath)
	}
	decl0 := file.Decls[0]
	var before ast.Node = decl0
	switch decl0 := decl0.(type) {
	case *ast.GenDecl:
		if decl0.Doc != nil {
			before = decl0.Doc
		}
	case *ast.FuncDecl:
		if decl0.Doc != nil {
			before = decl0.Doc
		}
	}
	return newName, []analysis.TextEdit{{
		Pos:     before.Pos(),
		End:     before.Pos(),
		NewText: []byte(newText),
	}}
}

// Format returns a string representation of the expression e.
func Format(fset *token.FileSet, e ast.Expr) string {
	var buf strings.Builder
	printer.Fprint(&buf, fset, e) // ignore errors
	return buf.String()
}

// Imports returns true if path is imported by pkg.
func Imports(pkg *types.Package, path string) bool {
	for _, imp := range pkg.Imports() {
		if imp.Path() == path {
			return true
		}
	}
	return false
}

// IsTypeNamed reports whether t is (or is an alias for) a
// package-level defined type with the given package path and one of
// the given names. It returns false if t is nil.
//
// This function avoids allocating the concatenation of "pkg.Name",
// which is important for the performance of syntax matching.
func IsTypeNamed(t types.Type, pkgPath string, names ...string) bool {
	if named, ok := types.Unalias(t).(*types.Named); ok {
		tname := named.Obj()
		return tname != nil &&
			isPackageLevel(tname) &&
			tname.Pkg().Path() == pkgPath &&
			slices.Contains(names, tname.Name())
	}
	return false
}

// IsPointerToNamed reports whether t is (or is an alias for) a pointer to a
// package-level defined type with the given package path and one of the given
// names. It returns false if t is not a pointer type.
func IsPointerToNamed(t types.Type, pkgPath string, names ...string) bool {
	r := typesinternal.Unpointer(t)
	if r == t {
		return false
	}
	return IsTypeNamed(r, pkgPath, names...)
}

// IsFunctionNamed reports whether obj is a package-level function
// defined in the given package and has one of the given names.
// It returns false if obj is nil.
//
// This function avoids allocating the concatenation of "pkg.Name",
// which is important for the performance of syntax matching.
func IsFunctionNamed(obj types.Object, pkgPath string, names ...string) bool {
	f, ok := obj.(*types.Func)
	return ok &&
		isPackageLevel(obj) &&
		f.Pkg().Path() == pkgPath &&
		f.Type().(*types.Signature).Recv() == nil &&
		slices.Contains(names, f.Name())
}

// IsMethodNamed reports whether obj is a method defined on a
// package-level type with the given package and type name, and has
// one of the given names. It returns false if obj is nil.
//
// This function avoids allocating the concatenation of "pkg.TypeName.Name",
// which is important for the performance of syntax matching.
func IsMethodNamed(obj types.Object, pkgPath string, typeName string, names ...string) bool {
	if fn, ok := obj.(*types.Func); ok {
		if recv := fn.Type().(*types.Signature).Recv(); recv != nil {
			_, T := typesinternal.ReceiverNamed(recv)
			return T != nil &&
				IsTypeNamed(T, pkgPath, typeName) &&
				slices.Contains(names, fn.Name())
		}
	}
	return false
}

// isPackageLevel reports whether obj is a package-level symbol.
//
// TODO(adonovan): publish in typesinternal and factor with
// gopls/internal/golang/rename_check.go, refactor/rename/util.go.
func isPackageLevel(obj types.Object) bool {
	return obj.Pkg() != nil && obj.Parent() == obj.Pkg().Scope()
}
