package astutil

import "honnef.co/go/tools/go/types"

// Unparen returns e with any enclosing parentheses stripped.
func Unparen(e types.Expr) types.Expr {
	for {
		p, ok := e.(*types.ParenExpr)
		if !ok {
			return e
		}
		e = p.X
	}
}
