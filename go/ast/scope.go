// Copyright 2009 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

// This file implements scopes and the objects they contain.

package ast

import (
	"bytes"
	"fmt"
	"go/token"
)

// A ScopeA maintains the set of named language entities declared
// in the scope and a link to the immediately surrounding (outer)
// scope.
//
type ScopeA struct {
	Outer   *ScopeA
	Objects map[string]*ObjectA
}

// NewScopeA creates a new scope nested in the outer scope.
func NewScopeA(outer *ScopeA) *ScopeA {
	const n = 4 // initial scope capacity
	return &ScopeA{outer, make(map[string]*ObjectA, n)}
}

// Lookup returns the object with the given name if it is
// found in scope s, otherwise it returns nil. Outer scopes
// are ignored.
//
func (s *ScopeA) Lookup(name string) *ObjectA {
	return s.Objects[name]
}

// Insert attempts to insert a named object obj into the scope s.
// If the scope already contains an object alt with the same name,
// Insert leaves the scope unchanged and returns alt. Otherwise
// it inserts obj and returns nil.
//
func (s *ScopeA) Insert(obj *ObjectA) (alt *ObjectA) {
	if alt = s.Objects[obj.Name]; alt == nil {
		s.Objects[obj.Name] = obj
	}
	return
}

// Debugging support
func (s *ScopeA) String() string {
	var buf bytes.Buffer
	fmt.Fprintf(&buf, "scope %p {", s)
	if s != nil && len(s.Objects) > 0 {
		fmt.Fprintln(&buf)
		for _, obj := range s.Objects {
			fmt.Fprintf(&buf, "\t%s %s\n", obj.Kind, obj.Name)
		}
	}
	fmt.Fprintf(&buf, "}\n")
	return buf.String()
}

// ----------------------------------------------------------------------------
// Objects

// An ObjectA describes a named language entity such as a package,
// constant, type, variable, function (incl. methods), or label.
//
// The Data fields contains object-specific data:
//
//	Kind    Data type         Data value
//	Pkg     *Scope            package scope
//	Con     int               iota for the respective declaration
//
type ObjectA struct {
	Kind ObjKind
	Name string      // declared name
	Decl interface{} // corresponding Field, XxxSpec, FuncDecl, LabeledStmt, AssignStmt, Scope; or nil
	Data interface{} // object-specific data; or nil
	Type interface{} // placeholder for type information; may be nil
}

// NewObj creates a new object of a given kind and name.
func NewObj(kind ObjKind, name string) *ObjectA {
	return &ObjectA{Kind: kind, Name: name}
}

// Pos computes the source position of the declaration of an object name.
// The result may be an invalid position if it cannot be computed
// (obj.Decl may be nil or not correct).
func (obj *ObjectA) Pos() token.Pos {
	name := obj.Name
	switch d := obj.Decl.(type) {
	case *Field:
		for _, n := range d.Names {
			if n.Name == name {
				return n.Pos()
			}
		}
	case *ImportSpec:
		if d.Name != nil && d.Name.Name == name {
			return d.Name.Pos()
		}
		return d.Path.Pos()
	case *ValueSpec:
		for _, n := range d.Names {
			if n.Name == name {
				return n.Pos()
			}
		}
	case *TypeSpec:
		if d.Name.Name == name {
			return d.Name.Pos()
		}
	case *FuncDecl:
		if d.Name.Name == name {
			return d.Name.Pos()
		}
	case *LabeledStmt:
		if d.Label.Name == name {
			return d.Label.Pos()
		}
	case *AssignStmt:
		for _, x := range d.Lhs {
			if ident, isIdent := x.(*Ident); isIdent && ident.Name == name {
				return ident.Pos()
			}
		}
	case *ScopeA:
		// predeclared object - nothing to do for now
	}
	return token.NoPos
}

// ObjKind describes what an object represents.
type ObjKind int

// The list of possible Object kinds.
const (
	BadKind ObjKind = iota // for error handling
	PkgKind                // package
	ConKind                // constant
	TypKind                // type
	VarKind                // variable
	FunKind                // function or method
	LblKind                // label
)

var objKindStrings = [...]string{
	BadKind: "bad",
	PkgKind: "package",
	ConKind: "const",
	TypKind: "type",
	VarKind: "var",
	FunKind: "func",
	LblKind: "label",
}

func (kind ObjKind) String() string { return objKindStrings[kind] }
