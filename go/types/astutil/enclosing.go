// Copyright 2013 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

package astutil

// This file defines utilities for working with source positions.

import (
	"fmt"
	"go/token"
	"sort"

	"honnef.co/go/tools/go/types"
)

// PathEnclosingInterval returns the node that encloses the source
// interval [start, end), and all its ancestors up to the AST root.
//
// The definition of "enclosing" used by this function considers
// additional whitespace abutting a node to be enclosed by it.
// In this example:
//
//              z := x + y // add them
//                   <-A->
//                  <----B----->
//
// the types.BinaryExpr(+) node is considered to enclose interval B
// even though its [Pos()..End()) is actually only interval A.
// This behaviour makes user interfaces more tolerant of imperfect
// input.
//
// This function treats tokens as nodes, though they are not included
// in the result. e.g. PathEnclosingInterval("+") returns the
// enclosing types.BinaryExpr("x + y").
//
// If start==end, the 1-char interval following start is used instead.
//
// The 'exact' result is true if the interval contains only path[0]
// and perhaps some adjacent whitespace.  It is false if the interval
// overlaps multiple children of path[0], or if it contains only
// interior whitespace of path[0].
// In this example:
//
//              z := x + y // add them
//                <--C-->     <---E-->
//                  ^
//                  D
//
// intervals C, D and E are inexact.  C is contained by the
// z-assignment statement, because it spans three of its children (:=,
// x, +).  So too is the 1-char interval D, because it contains only
// interior whitespace of the assignment.  E is considered interior
// whitespace of the BlockStmt containing the assignment.
//
// Precondition: [start, end) both lie within the same file as root.
// TODO(adonovan): return (nil, false) in this case and remove precond.
// Requires FileSet; see loader.tokenFileContainsPos.
//
// Postcondition: path is never nil; it always contains at least 'root'.
//
func PathEnclosingInterval(root *types.File, start, end token.Pos) (path []types.Node, exact bool) {
	// fmt.Printf("EnclosingInterval %d %d\n", start, end) // debugging

	// Precondition: node.[Pos..End) and adjoining whitespace contain [start, end).
	var visit func(node types.Node) bool
	visit = func(node types.Node) bool {
		path = append(path, node)

		nodePos := node.Pos()
		nodeEnd := node.End()

		// fmt.Printf("visit(%T, %d, %d)\n", node, nodePos, nodeEnd) // debugging

		// Intersect [start, end) with interval of node.
		if start < nodePos {
			start = nodePos
		}
		if end > nodeEnd {
			end = nodeEnd
		}

		// Find sole child that contains [start, end).
		children := childrenOf(node)
		l := len(children)
		for i, child := range children {
			// [childPos, childEnd) is unaugmented interval of child.
			childPos := child.Pos()
			childEnd := child.End()

			// [augPos, augEnd) is whitespace-augmented interval of child.
			augPos := childPos
			augEnd := childEnd
			if i > 0 {
				augPos = children[i-1].End() // start of preceding whitespace
			}
			if i < l-1 {
				nextChildPos := children[i+1].Pos()
				// Does [start, end) lie between child and next child?
				if start >= augEnd && end <= nextChildPos {
					return false // inexact match
				}
				augEnd = nextChildPos // end of following whitespace
			}

			// fmt.Printf("\tchild %d: [%d..%d)\tcontains interval [%d..%d)?\n",
			// 	i, augPos, augEnd, start, end) // debugging

			// Does augmented child strictly contain [start, end)?
			if augPos <= start && end <= augEnd {
				_, isToken := child.(tokenNode)
				return isToken || visit(child)
			}

			// Does [start, end) overlap multiple children?
			// i.e. left-augmented child contains start
			// but LR-augmented child does not contain end.
			if start < childEnd && end > augEnd {
				break
			}
		}

		// No single child contained [start, end),
		// so node is the result.  Is it exact?

		// (It's tempting to put this condition before the
		// child loop, but it gives the wrong result in the
		// case where a node (e.g. ExprStmt) and its sole
		// child have equal intervals.)
		if start == nodePos && end == nodeEnd {
			return true // exact match
		}

		return false // inexact: overlaps multiple children
	}

	if start > end {
		start, end = end, start
	}

	if start < root.End() && end > root.Pos() {
		if start == end {
			end = start + 1 // empty interval => interval of size 1
		}
		exact = visit(root)

		// Reverse the path:
		for i, l := 0, len(path); i < l/2; i++ {
			path[i], path[l-1-i] = path[l-1-i], path[i]
		}
	} else {
		// Selection lies within whitespace preceding the
		// first (or following the last) declaration in the file.
		// The result nonetheless always includes the types.File.
		path = append(path, root)
	}

	return
}

// tokenNode is a dummy implementation of types.Node for a single token.
// They are used transiently by PathEnclosingInterval but never escape
// this package.
//
type tokenNode struct {
	pos token.Pos
	end token.Pos
}

func (n tokenNode) Pos() token.Pos {
	return n.pos
}

func (n tokenNode) End() token.Pos {
	return n.end
}

func tok(pos token.Pos, len int) types.Node {
	return tokenNode{pos, pos + token.Pos(len)}
}

// childrenOf returns the direct non-nil children of types.Node n.
// It may include fake types.Node implementations for bare tokens.
// it is not safe to call (e.g.) types.Walk on such nodes.
//
func childrenOf(n types.Node) []types.Node {
	var children []types.Node

	// First add nodes for all true subtrees.
	types.Inspect(n, func(node types.Node) bool {
		if node == n { // push n
			return true // recur
		}
		if node != nil { // push child
			children = append(children, node)
		}
		return false // no recursion
	})

	// Then add fake Nodes for bare tokens.
	switch n := n.(type) {
	case *types.ArrayType:
		children = append(children,
			tok(n.Lbrack, len("[")),
			tok(n.Elt.End(), len("]")))

	case *types.AssignStmt:
		children = append(children,
			tok(n.TokPos, len(n.Tok.String())))

	case *types.BasicLit:
		children = append(children,
			tok(n.ValuePos, len(n.Value)))

	case *types.BinaryExpr:
		children = append(children, tok(n.OpPos, len(n.Op.String())))

	case *types.BlockStmt:
		children = append(children,
			tok(n.Lbrace, len("{")),
			tok(n.Rbrace, len("}")))

	case *types.BranchStmt:
		children = append(children,
			tok(n.TokPos, len(n.Tok.String())))

	case *types.CallExpr:
		children = append(children,
			tok(n.Lparen, len("(")),
			tok(n.Rparen, len(")")))
		if n.Ellipsis != 0 {
			children = append(children, tok(n.Ellipsis, len("...")))
		}

	case *types.CaseClause:
		if n.List == nil {
			children = append(children,
				tok(n.Case, len("default")))
		} else {
			children = append(children,
				tok(n.Case, len("case")))
		}
		children = append(children, tok(n.Colon, len(":")))

	case *types.ChanType:
		switch n.Dir {
		case types.RecvOnly:
			children = append(children, tok(n.Begin, len("<-chan")))
		case types.SendOnly:
			children = append(children, tok(n.Begin, len("chan<-")))
		case types.SendRecv:
			children = append(children, tok(n.Begin, len("chan")))
		}

	case *types.CommClause:
		if n.Comm == nil {
			children = append(children,
				tok(n.Case, len("default")))
		} else {
			children = append(children,
				tok(n.Case, len("case")))
		}
		children = append(children, tok(n.Colon, len(":")))

	case *types.Comment:
		// nop

	case *types.CommentGroup:
		// nop

	case *types.CompositeLit:
		children = append(children,
			tok(n.Lbrace, len("{")),
			tok(n.Rbrace, len("{")))

	case *types.DeclStmt:
		// nop

	case *types.DeferStmt:
		children = append(children,
			tok(n.Defer, len("defer")))

	case *types.Ellipsis:
		children = append(children,
			tok(n.Ellipsis, len("...")))

	case *types.EmptyStmt:
		// nop

	case *types.ExprStmt:
		// nop

	case *types.Field:
		// TODO(adonovan): Field.{Doc,Comment,Tag}?

	case *types.FieldList:
		children = append(children,
			tok(n.Opening, len("(")),
			tok(n.Closing, len(")")))

	case *types.File:
		// TODO test: Doc
		children = append(children,
			tok(n.Package, len("package")))

	case *types.ForStmt:
		children = append(children,
			tok(n.For, len("for")))

	case *types.FuncDecl:
		// TODO(adonovan): FuncDecl.Comment?

		// Uniquely, FuncDecl breaks the invariant that
		// preorder traversal yields tokens in lexical order:
		// in fact, FuncDecl.Recv precedes FuncDecl.Type.Func.
		//
		// As a workaround, we inline the case for FuncType
		// here and order things correctly.
		//
		children = nil // discard types.Walk(FuncDecl) info subtrees
		children = append(children, tok(n.Type.Func, len("func")))
		if n.Recv != nil {
			children = append(children, n.Recv)
		}
		children = append(children, n.Name)
		if n.Type.Params != nil {
			children = append(children, n.Type.Params)
		}
		if n.Type.Results != nil {
			children = append(children, n.Type.Results)
		}
		if n.Body != nil {
			children = append(children, n.Body)
		}

	case *types.FuncLit:
		// nop

	case *types.FuncType:
		if n.Func != 0 {
			children = append(children,
				tok(n.Func, len("func")))
		}

	case *types.GenDecl:
		children = append(children,
			tok(n.TokPos, len(n.Tok.String())))
		if n.Lparen != 0 {
			children = append(children,
				tok(n.Lparen, len("(")),
				tok(n.Rparen, len(")")))
		}

	case *types.GoStmt:
		children = append(children,
			tok(n.Go, len("go")))

	case *types.Ident:
		children = append(children,
			tok(n.NamePos, len(n.Name)))

	case *types.IfStmt:
		children = append(children,
			tok(n.If, len("if")))

	case *types.ImportSpec:
		// TODO(adonovan): ImportSpec.{Doc,EndPos}?

	case *types.IncDecStmt:
		children = append(children,
			tok(n.TokPos, len(n.Tok.String())))

	case *types.IndexExpr:
		children = append(children,
			tok(n.Lbrack, len("{")),
			tok(n.Rbrack, len("}")))

	case *types.InterfaceType:
		children = append(children,
			tok(n.Interface, len("interface")))

	case *types.KeyValueExpr:
		children = append(children,
			tok(n.Colon, len(":")))

	case *types.LabeledStmt:
		children = append(children,
			tok(n.Colon, len(":")))

	case *types.MapType:
		children = append(children,
			tok(n.Map, len("map")))

	case *types.ParenExpr:
		children = append(children,
			tok(n.Lparen, len("(")),
			tok(n.Rparen, len(")")))

	case *types.RangeStmt:
		children = append(children,
			tok(n.For, len("for")),
			tok(n.TokPos, len(n.Tok.String())))

	case *types.ReturnStmt:
		children = append(children,
			tok(n.Return, len("return")))

	case *types.SelectStmt:
		children = append(children,
			tok(n.Select, len("select")))

	case *types.SelectorExpr:
		// nop

	case *types.SendStmt:
		children = append(children,
			tok(n.Arrow, len("<-")))

	case *types.SliceExpr:
		children = append(children,
			tok(n.Lbrack, len("[")),
			tok(n.Rbrack, len("]")))

	case *types.StarExpr:
		children = append(children, tok(n.Star, len("*")))

	case *types.StructType:
		children = append(children, tok(n.Struct, len("struct")))

	case *types.SwitchStmt:
		children = append(children, tok(n.Switch, len("switch")))

	case *types.TypeAssertExpr:
		children = append(children,
			tok(n.Lparen-1, len(".")),
			tok(n.Lparen, len("(")),
			tok(n.Rparen, len(")")))

	case *types.TypeSpec:
		// TODO(adonovan): TypeSpec.{Doc,Comment}?

	case *types.TypeSwitchStmt:
		children = append(children, tok(n.Switch, len("switch")))

	case *types.UnaryExpr:
		children = append(children, tok(n.OpPos, len(n.Op.String())))

	case *types.ValueSpec:
		// TODO(adonovan): ValueSpec.{Doc,Comment}?

	case *types.BadDecl, *types.BadExpr, *types.BadStmt:
		// nop
	}

	// TODO(adonovan): opt: merge the logic of types.Inspect() into
	// the switch above so we can make interleaved callbacks for
	// both Nodes and Tokens in the right order and avoid the need
	// to sort.
	sort.Sort(byPos(children))

	return children
}

type byPos []types.Node

func (sl byPos) Len() int {
	return len(sl)
}
func (sl byPos) Less(i, j int) bool {
	return sl[i].Pos() < sl[j].Pos()
}
func (sl byPos) Swap(i, j int) {
	sl[i], sl[j] = sl[j], sl[i]
}

// NodeDescription returns a description of the concrete type of n suitable
// for a user interface.
//
// TODO(adonovan): in some cases (e.g. Field, FieldList, Ident,
// StarExpr) we could be much more specific given the path to the AST
// root.  Perhaps we should do that.
//
func NodeDescription(n types.Node) string {
	switch n := n.(type) {
	case *types.ArrayType:
		return "array type"
	case *types.AssignStmt:
		return "assignment"
	case *types.BadDecl:
		return "bad declaration"
	case *types.BadExpr:
		return "bad expression"
	case *types.BadStmt:
		return "bad statement"
	case *types.BasicLit:
		return "basic literal"
	case *types.BinaryExpr:
		return fmt.Sprintf("binary %s operation", n.Op)
	case *types.BlockStmt:
		return "block"
	case *types.BranchStmt:
		switch n.Tok {
		case token.BREAK:
			return "break statement"
		case token.CONTINUE:
			return "continue statement"
		case token.GOTO:
			return "goto statement"
		case token.FALLTHROUGH:
			return "fall-through statement"
		}
	case *types.CallExpr:
		if len(n.Args) == 1 && !n.Ellipsis.IsValid() {
			return "function call (or conversion)"
		}
		return "function call"
	case *types.CaseClause:
		return "case clause"
	case *types.ChanType:
		return "channel type"
	case *types.CommClause:
		return "communication clause"
	case *types.Comment:
		return "comment"
	case *types.CommentGroup:
		return "comment group"
	case *types.CompositeLit:
		return "composite literal"
	case *types.DeclStmt:
		return NodeDescription(n.Decl) + " statement"
	case *types.DeferStmt:
		return "defer statement"
	case *types.Ellipsis:
		return "ellipsis"
	case *types.EmptyStmt:
		return "empty statement"
	case *types.ExprStmt:
		return "expression statement"
	case *types.Field:
		// Can be any of these:
		// struct {x, y int}  -- struct field(s)
		// struct {T}         -- anon struct field
		// interface {I}      -- interface embedding
		// interface {f()}    -- interface method
		// func (A) func(B) C -- receiver, param(s), result(s)
		return "field/method/parameter"
	case *types.FieldList:
		return "field/method/parameter list"
	case *types.File:
		return "source file"
	case *types.ForStmt:
		return "for loop"
	case *types.FuncDecl:
		return "function declaration"
	case *types.FuncLit:
		return "function literal"
	case *types.FuncType:
		return "function type"
	case *types.GenDecl:
		switch n.Tok {
		case token.IMPORT:
			return "import declaration"
		case token.CONST:
			return "constant declaration"
		case token.TYPE:
			return "type declaration"
		case token.VAR:
			return "variable declaration"
		}
	case *types.GoStmt:
		return "go statement"
	case *types.Ident:
		return "identifier"
	case *types.IfStmt:
		return "if statement"
	case *types.ImportSpec:
		return "import specification"
	case *types.IncDecStmt:
		if n.Tok == token.INC {
			return "increment statement"
		}
		return "decrement statement"
	case *types.IndexExpr:
		return "index expression"
	case *types.InterfaceType:
		return "interface type"
	case *types.KeyValueExpr:
		return "key/value association"
	case *types.LabeledStmt:
		return "statement label"
	case *types.MapType:
		return "map type"
	case *types.PackageA:
		return "package"
	case *types.ParenExpr:
		return "parenthesized " + NodeDescription(n.X)
	case *types.RangeStmt:
		return "range loop"
	case *types.ReturnStmt:
		return "return statement"
	case *types.SelectStmt:
		return "select statement"
	case *types.SelectorExpr:
		return "selector"
	case *types.SendStmt:
		return "channel send"
	case *types.SliceExpr:
		return "slice expression"
	case *types.StarExpr:
		return "*-operation" // load/store expr or pointer type
	case *types.StructType:
		return "struct type"
	case *types.SwitchStmt:
		return "switch statement"
	case *types.TypeAssertExpr:
		return "type assertion"
	case *types.TypeSpec:
		return "type specification"
	case *types.TypeSwitchStmt:
		return "type switch"
	case *types.UnaryExpr:
		return fmt.Sprintf("unary %s operation", n.Op)
	case *types.ValueSpec:
		return "value specification"

	}
	panic(fmt.Sprintf("unexpected node type: %T", n))
}
