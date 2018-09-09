// Copyright 2012 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

package types_test

import (
	"bytes"
	"fmt"

	"honnef.co/go/tools/go/format"
	"honnef.co/go/tools/go/parser"
	"honnef.co/go/tools/go/token"
	"honnef.co/go/tools/go/types"
)

// This example demonstrates how to inspect the AST of a Go program.
func ExampleInspect() {
	// src is the input for which we want to inspect the AST.
	src := `
package p
const c = 1.0
var X = f(3.14)*2 + c
`

	// Create the AST by parsing src.
	fset := token.NewFileSet() // positions are relative to fset
	f, err := parser.ParseFile(fset, "src.go", src, 0)
	if err != nil {
		panic(err)
	}

	// Inspect the AST and print all identifiers and literals.
	types.Inspect(f, func(n types.Node) bool {
		var s string
		switch x := n.(type) {
		case *types.BasicLit:
			s = x.Value
		case *types.Ident:
			s = x.Name
		}
		if s != "" {
			fmt.Printf("%s:\t%s\n", fset.Position(n.Pos()), s)
		}
		return true
	})

	// Output:
	// src.go:2:9:	p
	// src.go:3:7:	c
	// src.go:3:11:	1.0
	// src.go:4:5:	X
	// src.go:4:9:	f
	// src.go:4:11:	3.14
	// src.go:4:17:	2
	// src.go:4:21:	c
}

// This example shows what an AST looks like when printed for debugging.
func ExamplePrint() {
	// src is the input for which we want to print the AST.
	src := `
package main
func main() {
	println("Hello, World!")
}
`

	// Create the AST by parsing src.
	fset := token.NewFileSet() // positions are relative to fset
	f, err := parser.ParseFile(fset, "", src, 0)
	if err != nil {
		panic(err)
	}

	// Print the AST.
	types.Print(fset, f)

	// Output:
	//      0  *types.File {
	//      1  .  Package: 2:1
	//      2  .  Name: *types.Ident {
	//      3  .  .  NamePos: 2:9
	//      4  .  .  Name: "main"
	//      5  .  .  IsDef: false
	//      6  .  }
	//      7  .  Decls: []types.Decl (len = 1) {
	//      8  .  .  0: *types.FuncDecl {
	//      9  .  .  .  Name: *types.Ident {
	//     10  .  .  .  .  NamePos: 3:6
	//     11  .  .  .  .  Name: "main"
	//     12  .  .  .  .  IsDef: false
	//     13  .  .  .  }
	//     14  .  .  .  Type: *types.FuncType {
	//     15  .  .  .  .  Func: 3:1
	//     16  .  .  .  .  Params: *types.FieldList {
	//     17  .  .  .  .  .  Opening: 3:10
	//     18  .  .  .  .  .  Closing: 3:11
	//     19  .  .  .  .  }
	//     20  .  .  .  }
	//     21  .  .  .  Body: *types.BlockStmt {
	//     22  .  .  .  .  Lbrace: 3:13
	//     23  .  .  .  .  List: []types.Stmt (len = 1) {
	//     24  .  .  .  .  .  0: *types.ExprStmt {
	//     25  .  .  .  .  .  .  X: *types.CallExpr {
	//     26  .  .  .  .  .  .  .  Fun: *types.Ident {
	//     27  .  .  .  .  .  .  .  .  NamePos: 4:2
	//     28  .  .  .  .  .  .  .  .  Name: "println"
	//     29  .  .  .  .  .  .  .  .  IsDef: false
	//     30  .  .  .  .  .  .  .  }
	//     31  .  .  .  .  .  .  .  Lparen: 4:9
	//     32  .  .  .  .  .  .  .  Args: []types.Expr (len = 1) {
	//     33  .  .  .  .  .  .  .  .  0: *types.BasicLit {
	//     34  .  .  .  .  .  .  .  .  .  ValuePos: 4:10
	//     35  .  .  .  .  .  .  .  .  .  Kind: STRING
	//     36  .  .  .  .  .  .  .  .  .  Value: "\"Hello, World!\""
	//     37  .  .  .  .  .  .  .  .  }
	//     38  .  .  .  .  .  .  .  }
	//     39  .  .  .  .  .  .  .  Ellipsis: -
	//     40  .  .  .  .  .  .  .  Rparen: 4:25
	//     41  .  .  .  .  .  .  }
	//     42  .  .  .  .  .  }
	//     43  .  .  .  .  }
	//     44  .  .  .  .  Rbrace: 5:1
	//     45  .  .  .  }
	//     46  .  .  }
	//     47  .  }
	//     48  }

}

// This example illustrates how to remove a variable declaration
// in a Go program while maintaining correct comment association
// using an ast.CommentMap.
func ExampleCommentMap() {
	// src is the input for which we create the AST that we
	// are going to manipulate.
	src := `
// This is the package comment.
package main

// This comment is associated with the hello constant.
const hello = "Hello, World!" // line comment 1

// This comment is associated with the foo variable.
var foo = hello // line comment 2 

// This comment is associated with the main function.
func main() {
	fmt.Println(hello) // line comment 3
}
`

	// Create the AST by parsing src.
	fset := token.NewFileSet() // positions are relative to fset
	f, err := parser.ParseFile(fset, "src.go", src, parser.ParseComments)
	if err != nil {
		panic(err)
	}

	// Create an ast.CommentMap from the ast.File's comments.
	// This helps keeping the association between comments
	// and AST nodes.
	cmap := types.NewCommentMap(fset, f, f.Comments)

	// Remove the first variable declaration from the list of declarations.
	for i, decl := range f.Decls {
		if gen, ok := decl.(*types.GenDecl); ok && gen.Tok == token.VAR {
			copy(f.Decls[i:], f.Decls[i+1:])
			f.Decls = f.Decls[:len(f.Decls)-1]
		}
	}

	// Use the comment map to filter comments that don't belong anymore
	// (the comments associated with the variable declaration), and create
	// the new comments list.
	f.Comments = cmap.Filter(f).Comments()

	// Print the modified AST.
	var buf bytes.Buffer
	if err := format.Node(&buf, fset, f); err != nil {
		panic(err)
	}
	fmt.Printf("%s", buf.Bytes())

	// Output:
	// // This is the package comment.
	// package main
	//
	// // This comment is associated with the hello constant.
	// const hello = "Hello, World!" // line comment 1
	//
	// // This comment is associated with the main function.
	// func main() {
	// 	fmt.Println(hello) // line comment 3
	// }
}
