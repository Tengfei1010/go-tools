// Copyright 2009 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

package types

import (
	"go/token"
)

// ----------------------------------------------------------------------------
// Export filtering

// exportFilter is a special filter function to extract exported nodes.
func exportFilter(name string) bool {
	return IsExported(name)
}

// FileExports trims the AST for a Go source file in place such that
// only exported nodes remain: all top-level identifiers which are not exported
// and their associated information (such as type, initial value, or function
// body) are removed. Non-exported fields and methods of exported types are
// stripped. The File.Comments list is not changed.
//
// FileExports reports whether there are exported declarations.
//
func FileExports(src *File) bool {
	return filterFile(src, exportFilter, true)
}

// ----------------------------------------------------------------------------
// General filtering

type Filter func(string) bool

func filterIdentList(list []*Ident, f Filter) []*Ident {
	j := 0
	for _, x := range list {
		if f(x.Name) {
			list[j] = x
			j++
		}
	}
	return list[0:j]
}

// fieldName assumes that x is the type of an anonymous field and
// returns the corresponding field name. If x is not an acceptable
// anonymous field, the result is nil.
//
func fieldName(x Expr) *Ident {
	switch t := x.(type) {
	case *Ident:
		return t
	case *SelectorExpr:
		if _, ok := t.X.(*Ident); ok {
			return t.Sel
		}
	case *StarExpr:
		return fieldName(t.X)
	}
	return nil
}

func filterFieldList(fields *FieldList, filter Filter, export bool) (removedFields bool) {
	if fields == nil {
		return false
	}
	list := fields.List
	j := 0
	for _, f := range list {
		keepField := false
		if len(f.Names) == 0 {
			// anonymous field
			name := fieldName(f.Type)
			keepField = name != nil && filter(name.Name)
		} else {
			n := len(f.Names)
			f.Names = filterIdentList(f.Names, filter)
			if len(f.Names) < n {
				removedFields = true
			}
			keepField = len(f.Names) > 0
		}
		if keepField {
			if export {
				filterType(f.Type, filter, export)
			}
			list[j] = f
			j++
		}
	}
	if j < len(list) {
		removedFields = true
	}
	fields.List = list[0:j]
	return
}

func filterCompositeLit(lit *CompositeLit, filter Filter, export bool) {
	n := len(lit.Elts)
	lit.Elts = filterExprList(lit.Elts, filter, export)
	if len(lit.Elts) < n {
		lit.Incomplete = true
	}
}

func filterExprList(list []Expr, filter Filter, export bool) []Expr {
	j := 0
	for _, exp := range list {
		switch x := exp.(type) {
		case *CompositeLit:
			filterCompositeLit(x, filter, export)
		case *KeyValueExpr:
			if x, ok := x.Key.(*Ident); ok && !filter(x.Name) {
				continue
			}
			if x, ok := x.Value.(*CompositeLit); ok {
				filterCompositeLit(x, filter, export)
			}
		}
		list[j] = exp
		j++
	}
	return list[0:j]
}

func filterParamList(fields *FieldList, filter Filter, export bool) bool {
	if fields == nil {
		return false
	}
	var b bool
	for _, f := range fields.List {
		if filterType(f.Type, filter, export) {
			b = true
		}
	}
	return b
}

func filterType(typ Expr, f Filter, export bool) bool {
	switch t := typ.(type) {
	case *Ident:
		return f(t.Name)
	case *ParenExpr:
		return filterType(t.X, f, export)
	case *ArrayType:
		return filterType(t.Elt, f, export)
	case *StructType:
		if filterFieldList(t.Fields, f, export) {
			t.Incomplete = true
		}
		return len(t.Fields.List) > 0
	case *FuncType:
		b1 := filterParamList(t.Params, f, export)
		b2 := filterParamList(t.Results, f, export)
		return b1 || b2
	case *InterfaceType:
		if filterFieldList(t.Methods, f, export) {
			t.Incomplete = true
		}
		return len(t.Methods.List) > 0
	case *MapType:
		b1 := filterType(t.Key, f, export)
		b2 := filterType(t.Value, f, export)
		return b1 || b2
	case *ChanType:
		return filterType(t.Value, f, export)
	}
	return false
}

func filterSpec(spec Spec, f Filter, export bool) bool {
	switch s := spec.(type) {
	case *ValueSpec:
		s.Names = filterIdentList(s.Names, f)
		s.Values = filterExprList(s.Values, f, export)
		if len(s.Names) > 0 {
			if export {
				filterType(s.Type, f, export)
			}
			return true
		}
	case *TypeSpec:
		if f(s.Name.Name) {
			if export {
				filterType(s.Type, f, export)
			}
			return true
		}
		if !export {
			// For general filtering (not just exports),
			// filter type even if name is not filtered
			// out.
			// If the type contains filtered elements,
			// keep the declaration.
			return filterType(s.Type, f, export)
		}
	}
	return false
}

func filterSpecList(list []Spec, f Filter, export bool) []Spec {
	j := 0
	for _, s := range list {
		if filterSpec(s, f, export) {
			list[j] = s
			j++
		}
	}
	return list[0:j]
}

// FilterDecl trims the AST for a Go declaration in place by removing
// all names (including struct field and interface method names, but
// not from parameter lists) that don't pass through the filter f.
//
// FilterDecl reports whether there are any declared names left after
// filtering.
//
func FilterDecl(decl Decl, f Filter) bool {
	return filterDecl(decl, f, false)
}

func filterDecl(decl Decl, f Filter, export bool) bool {
	switch d := decl.(type) {
	case *GenDecl:
		d.Specs = filterSpecList(d.Specs, f, export)
		return len(d.Specs) > 0
	case *FuncDecl:
		return f(d.Name.Name)
	}
	return false
}

// FilterFile trims the AST for a Go file in place by removing all
// names from top-level declarations (including struct field and
// interface method names, but not from parameter lists) that don't
// pass through the filter f. If the declaration is empty afterwards,
// the declaration is removed from the AST. Import declarations are
// always removed. The File.Comments list is not changed.
//
// FilterFile reports whether there are any top-level declarations
// left after filtering.
//
func FilterFile(src *File, f Filter) bool {
	return filterFile(src, f, false)
}

func filterFile(src *File, f Filter, export bool) bool {
	j := 0
	for _, d := range src.Decls {
		if filterDecl(d, f, export) {
			src.Decls[j] = d
			j++
		}
	}
	src.Decls = src.Decls[0:j]
	return j > 0
}

// ----------------------------------------------------------------------------
// Merging of package files

// The MergeMode flags control the behavior of MergePackageFiles.
type MergeMode uint

const (
	// If set, duplicate function declarations are excluded.
	FilterFuncDuplicates MergeMode = 1 << iota
	// If set, comments that are not associated with a specific
	// AST node (as Doc or Comment) are excluded.
	FilterUnassociatedComments
	// If set, duplicate import declarations are excluded.
	FilterImportDuplicates
)

// nameOf returns the function (foo) or method name (foo.bar) for
// the given function declaration. If the AST is incorrect for the
// receiver, it assumes a function instead.
//
func nameOf(f *FuncDecl) string {
	if r := f.Recv; r != nil && len(r.List) == 1 {
		// looks like a correct receiver declaration
		t := r.List[0].Type
		// dereference pointer receiver types
		if p, _ := t.(*StarExpr); p != nil {
			t = p.X
		}
		// the receiver type must be a type name
		if p, _ := t.(*Ident); p != nil {
			return p.Name + "." + f.Name.Name
		}
		// otherwise assume a function instead
	}
	return f.Name.Name
}

// separator is an empty //-style comment that is interspersed between
// different comment groups when they are concatenated into a single group
//
var separator = &Comment{token.NoPos, "//"}
