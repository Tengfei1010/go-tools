// Copyright 2013 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

// This file implements isTerminating.

package types

import (
	
	"honnef.co/go/tools/go/token"
)

// isTerminating reports if s is a terminating statement.
// If s is labeled, label is the label name; otherwise s
// is "".
func (check *Checker) isTerminating(s Stmt, label string) bool {
	switch s := s.(type) {
	default:
		unreachable()

	case *BadStmt, *DeclStmt, *EmptyStmt, *SendStmt,
		*IncDecStmt, *AssignStmt, *GoStmt, *DeferStmt,
		*RangeStmt:
		// no chance

	case *LabeledStmt:
		return check.isTerminating(s.Stmt, s.Label.Name)

	case *ExprStmt:
		// calling the predeclared (possibly parenthesized) panic() function is terminating
		if call, ok := unparen(s.X).(*CallExpr); ok && check.isPanic[call] {
			return true
		}

	case *ReturnStmt:
		return true

	case *BranchStmt:
		if s.Tok == token.GOTO || s.Tok == token.FALLTHROUGH {
			return true
		}

	case *BlockStmt:
		return check.isTerminatingList(s.List, "")

	case *IfStmt:
		if s.Else != nil &&
			check.isTerminating(s.Body, "") &&
			check.isTerminating(s.Else, "") {
			return true
		}

	case *SwitchStmt:
		return check.isTerminatingSwitch(s.Body, label)

	case *TypeSwitchStmt:
		return check.isTerminatingSwitch(s.Body, label)

	case *SelectStmt:
		for _, s := range s.Body.List {
			cc := s.(*CommClause)
			if !check.isTerminatingList(cc.Body, "") || hasBreakList(cc.Body, label, true) {
				return false
			}

		}
		return true

	case *ForStmt:
		if s.Cond == nil && !hasBreak(s.Body, label, true) {
			return true
		}
	}

	return false
}

func (check *Checker) isTerminatingList(list []Stmt, label string) bool {
	// trailing empty statements are permitted - skip them
	for i := len(list) - 1; i >= 0; i-- {
		if _, ok := list[i].(*EmptyStmt); !ok {
			return check.isTerminating(list[i], label)
		}
	}
	return false // all statements are empty
}

func (check *Checker) isTerminatingSwitch(body *BlockStmt, label string) bool {
	hasDefault := false
	for _, s := range body.List {
		cc := s.(*CaseClause)
		if cc.List == nil {
			hasDefault = true
		}
		if !check.isTerminatingList(cc.Body, "") || hasBreakList(cc.Body, label, true) {
			return false
		}
	}
	return hasDefault
}

// TODO(gri) For nested breakable statements, the current implementation of hasBreak
//	     will traverse the same subtree repeatedly, once for each label. Replace
//           with a single-pass label/break matching phase.

// hasBreak reports if s is or contains a break statement
// referring to the label-ed statement or implicit-ly the
// closest outer breakable statement.
func hasBreak(s Stmt, label string, implicit bool) bool {
	switch s := s.(type) {
	default:
		unreachable()

	case *BadStmt, *DeclStmt, *EmptyStmt, *ExprStmt,
		*SendStmt, *IncDecStmt, *AssignStmt, *GoStmt,
		*DeferStmt, *ReturnStmt:
		// no chance

	case *LabeledStmt:
		return hasBreak(s.Stmt, label, implicit)

	case *BranchStmt:
		if s.Tok == token.BREAK {
			if s.Label == nil {
				return implicit
			}
			if s.Label.Name == label {
				return true
			}
		}

	case *BlockStmt:
		return hasBreakList(s.List, label, implicit)

	case *IfStmt:
		if hasBreak(s.Body, label, implicit) ||
			s.Else != nil && hasBreak(s.Else, label, implicit) {
			return true
		}

	case *CaseClause:
		return hasBreakList(s.Body, label, implicit)

	case *SwitchStmt:
		if label != "" && hasBreak(s.Body, label, false) {
			return true
		}

	case *TypeSwitchStmt:
		if label != "" && hasBreak(s.Body, label, false) {
			return true
		}

	case *CommClause:
		return hasBreakList(s.Body, label, implicit)

	case *SelectStmt:
		if label != "" && hasBreak(s.Body, label, false) {
			return true
		}

	case *ForStmt:
		if label != "" && hasBreak(s.Body, label, false) {
			return true
		}

	case *RangeStmt:
		if label != "" && hasBreak(s.Body, label, false) {
			return true
		}
	}

	return false
}

func hasBreakList(list []Stmt, label string, implicit bool) bool {
	for _, s := range list {
		if hasBreak(s, label, implicit) {
			return true
		}
	}
	return false
}
