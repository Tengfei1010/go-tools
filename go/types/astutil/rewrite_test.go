// Copyright 2017 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

// +build ignore

package astutil_test

import (
	"bytes"
	"testing"

	"honnef.co/go/tools/go/ast/astutil"
	"honnef.co/go/tools/go/go/format"
	"honnef.co/go/tools/go/parser"
	"honnef.co/go/tools/go/token"
	"honnef.co/go/tools/go/types"
)

var rewriteTests = [...]struct {
	name       string
	orig, want string
	pre, post  astutil.ApplyFunc
}{
	{name: "nop", orig: "package p\n", want: "package p\n"},

	{name: "replace",
		orig: `package p

var x int
`,
		want: `package p

var t T
`,
		post: func(c *astutil.Cursor) bool {
			if _, ok := c.Node().(*types.ValueSpec); ok {
				c.Replace(valspec("t", "T"))
				return false
			}
			return true
		},
	},

	{name: "set doc strings",
		orig: `package p

const z = 0

type T struct{}

var x int
`,
		want: `package p
// a foo is a foo
const z = 0
// a foo is a foo
type T struct{}
// a foo is a foo
var x int
`,
		post: func(c *astutil.Cursor) bool {
			if _, ok := c.Parent().(*types.GenDecl); ok && c.Name() == "Doc" && c.Node() == nil {
				c.Replace(&types.CommentGroup{List: []*types.Comment{{Text: "// a foo is a foo"}}})
			}
			return true
		},
	},

	{name: "insert names",
		orig: `package p

const a = 1
`,
		want: `package p

const a, b, c = 1, 2, 3
`,
		pre: func(c *astutil.Cursor) bool {
			if _, ok := c.Parent().(*types.ValueSpec); ok {
				switch c.Name() {
				case "Names":
					c.InsertAfter(types.NewIdent("c"))
					c.InsertAfter(types.NewIdent("b"))
				case "Values":
					c.InsertAfter(&types.BasicLit{Kind: token.INT, Value: "3"})
					c.InsertAfter(&types.BasicLit{Kind: token.INT, Value: "2"})
				}
			}
			return true
		},
	},

	{name: "insert",
		orig: `package p

var (
	x int
	y int
)
`,
		want: `package p

var before1 int
var before2 int

var (
	x int
	y int
)
var after2 int
var after1 int
`,
		pre: func(c *astutil.Cursor) bool {
			if _, ok := c.Node().(*types.GenDecl); ok {
				c.InsertBefore(vardecl("before1", "int"))
				c.InsertAfter(vardecl("after1", "int"))
				c.InsertAfter(vardecl("after2", "int"))
				c.InsertBefore(vardecl("before2", "int"))
			}
			return true
		},
	},

	{name: "delete",
		orig: `package p

var x int
var y int
var z int
`,
		want: `package p

var y int
var z int
`,
		pre: func(c *astutil.Cursor) bool {
			n := c.Node()
			if d, ok := n.(*types.GenDecl); ok && d.Specs[0].(*types.ValueSpec).Names[0].Name == "x" {
				c.Delete()
			}
			return true
		},
	},

	{name: "insertafter-delete",
		orig: `package p

var x int
var y int
var z int
`,
		want: `package p

var x1 int

var y int
var z int
`,
		pre: func(c *astutil.Cursor) bool {
			n := c.Node()
			if d, ok := n.(*types.GenDecl); ok && d.Specs[0].(*types.ValueSpec).Names[0].Name == "x" {
				c.InsertAfter(vardecl("x1", "int"))
				c.Delete()
			}
			return true
		},
	},

	{name: "delete-insertafter",
		orig: `package p

var x int
var y int
var z int
`,
		want: `package p

var y int
var x1 int
var z int
`,
		pre: func(c *astutil.Cursor) bool {
			n := c.Node()
			if d, ok := n.(*types.GenDecl); ok && d.Specs[0].(*types.ValueSpec).Names[0].Name == "x" {
				c.Delete()
				// The cursor is now effectively atop the 'var y int' node.
				c.InsertAfter(vardecl("x1", "int"))
			}
			return true
		},
	},
}

func valspec(name, typ string) *types.ValueSpec {
	return &types.ValueSpec{Names: []*types.Ident{types.NewIdent(name)},
		Type: types.NewIdent(typ),
	}
}

func vardecl(name, typ string) *types.GenDecl {
	return &types.GenDecl{
		Tok:   token.VAR,
		Specs: []types.Spec{valspec(name, typ)},
	}
}

func TestRewrite(t *testing.T) {
	t.Run("*", func(t *testing.T) {
		for _, test := range rewriteTests {
			test := test
			t.Run(test.name, func(t *testing.T) {
				t.Parallel()
				fset := token.NewFileSet()
				f, err := parser.ParseFile(fset, test.name, test.orig, parser.ParseComments)
				if err != nil {
					t.Fatal(err)
				}
				n := astutil.Apply(f, test.pre, test.post)
				var buf bytes.Buffer
				if err := format.Node(&buf, fset, n); err != nil {
					t.Fatal(err)
				}
				got := buf.String()
				if got != test.want {
					t.Errorf("got:\n\n%s\nwant:\n\n%s\n", got, test.want)
				}
			})
		}
	})
}

var sink types.Node

func BenchmarkRewrite(b *testing.B) {
	for _, test := range rewriteTests {
		b.Run(test.name, func(b *testing.B) {
			for i := 0; i < b.N; i++ {
				b.StopTimer()
				fset := token.NewFileSet()
				f, err := parser.ParseFile(fset, test.name, test.orig, parser.ParseComments)
				if err != nil {
					b.Fatal(err)
				}
				b.StartTimer()
				sink = astutil.Apply(f, test.pre, test.post)
			}
		})
	}
}
