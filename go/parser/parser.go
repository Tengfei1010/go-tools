// Copyright 2009 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

// Package parser implements a parser for Go source files. Input may be
// provided in a variety of forms (see the various Parse* functions); the
// output is an abstract syntax tree (AST) representing the Go source. The
// parser is invoked through one of the Parse* functions.
//
// The parser accepts a larger language than is syntactically permitted by
// the Go spec, for simplicity, and for improved robustness in the presence
// of syntax errors. For instance, in method declarations, the receiver is
// treated like an ordinary parameter list and thus may contain multiple
// entries where the spec permits exactly one. Consequently, the corresponding
// field in the AST (types.FuncDecl.Recv) field is not restricted to one entry.
//
package parser

import (
	"fmt"
	"strconv"
	"strings"
	"unicode"

	"honnef.co/go/tools/go/scanner"
	"honnef.co/go/tools/go/token"
	"honnef.co/go/tools/go/types"
)

// The parser structure holds the parser's internal state.
type parser struct {
	file    *token.File
	errors  scanner.ErrorList
	scanner scanner.Scanner

	// Tracing/debugging
	mode   Mode // parsing mode
	trace  bool // == (mode & Trace != 0)
	indent int  // indentation used for tracing output

	// Comments
	comments    []*types.CommentGroup
	leadComment *types.CommentGroup // last lead comment
	lineComment *types.CommentGroup // last line comment

	// Next token
	pos token.Pos   // token position
	tok token.Token // one token look-ahead
	lit string      // token literal

	// Error recovery
	// (used to limit the number of calls to parser.advance
	// w/o making scanning progress - avoids potential endless
	// loops across multiple parser functions during error recovery)
	syncPos token.Pos // last synchronization position
	syncCnt int       // number of parser.advance calls without progress

	// Non-syntactic parser control
	exprLev int  // < 0: in control clause, >= 0: in expression
	inRhs   bool // if set, the parser is parsing a rhs expression

	// Ordinary identifier scopes
	imports []*types.ImportSpec // list of imports
}

func (p *parser) init(fset *token.FileSet, filename string, src []byte, mode Mode) {
	p.file = fset.AddFile(filename, -1, len(src))
	var m scanner.Mode
	if mode&ParseComments != 0 {
		m = scanner.ScanComments
	}
	eh := func(pos token.Position, msg string) { p.errors.Add(pos, msg) }
	p.scanner.Init(p.file, src, eh, m)

	p.mode = mode
	p.trace = mode&Trace != 0 // for convenience (p.trace is used frequently)

	p.next()
}

func (p *parser) shortVarDecl(decl *types.AssignStmt, list []types.Expr) {
	// Go spec: A short variable declaration may redeclare variables
	// provided they were originally declared in the same block with
	// the same type, and at least one of the non-blank variables is new.
	for _, x := range list {
		if _, isIdent := x.(*types.Ident); !isIdent {
			p.errorExpected(x.Pos(), "identifier on left side of :=")
		}
	}
}

// ----------------------------------------------------------------------------
// Parsing support

func (p *parser) printTrace(a ...interface{}) {
	const dots = ". . . . . . . . . . . . . . . . . . . . . . . . . . . . . . . . "
	const n = len(dots)
	pos := p.file.Position(p.pos)
	fmt.Printf("%5d:%3d: ", pos.Line, pos.Column)
	i := 2 * p.indent
	for i > n {
		fmt.Print(dots)
		i -= n
	}
	// i <= n
	fmt.Print(dots[0:i])
	fmt.Println(a...)
}

func trace(p *parser, msg string) *parser {
	p.printTrace(msg, "(")
	p.indent++
	return p
}

// Usage pattern: defer un(trace(p, "..."))
func un(p *parser) {
	p.indent--
	p.printTrace(")")
}

// Advance to the next token.
func (p *parser) next0() {
	// Because of one-token look-ahead, print the previous token
	// when tracing as it provides a more readable output. The
	// very first token (!p.pos.IsValid()) is not initialized
	// (it is token.ILLEGAL), so don't print it .
	if p.trace && p.pos.IsValid() {
		s := p.tok.String()
		switch {
		case p.tok.IsLiteral():
			p.printTrace(s, p.lit)
		case p.tok.IsOperator(), p.tok.IsKeyword():
			p.printTrace("\"" + s + "\"")
		default:
			p.printTrace(s)
		}
	}

	p.pos, p.tok, p.lit = p.scanner.Scan()
}

// Consume a comment and return it and the line on which it ends.
func (p *parser) consumeComment() (comment *types.Comment, endline int) {
	// /*-style comments may end on a different line than where they start.
	// Scan the comment for '\n' chars and adjust endline accordingly.
	endline = p.file.Line(p.pos)
	if p.lit[1] == '*' {
		// don't use range here - no need to decode Unicode code points
		for i := 0; i < len(p.lit); i++ {
			if p.lit[i] == '\n' {
				endline++
			}
		}
	}

	comment = &types.Comment{Slash: p.pos, Text: p.lit}
	p.next0()

	return
}

// Consume a group of adjacent comments, add it to the parser's
// comments list, and return it together with the line at which
// the last comment in the group ends. A non-comment token or n
// empty lines terminate a comment group.
//
func (p *parser) consumeCommentGroup(n int) (comments *types.CommentGroup, endline int) {
	var list []*types.Comment
	endline = p.file.Line(p.pos)
	for p.tok == token.COMMENT && p.file.Line(p.pos) <= endline+n {
		var comment *types.Comment
		comment, endline = p.consumeComment()
		list = append(list, comment)
	}

	// add comment group to the comments list
	comments = &types.CommentGroup{List: list}
	p.comments = append(p.comments, comments)

	return
}

// Advance to the next non-comment token. In the process, collect
// any comment groups encountered, and remember the last lead and
// and line comments.
//
// A lead comment is a comment group that starts and ends in a
// line without any other tokens and that is followed by a non-comment
// token on the line immediately after the comment group.
//
// A line comment is a comment group that follows a non-comment
// token on the same line, and that has no tokens after it on the line
// where it ends.
//
// Lead and line comments may be considered documentation that is
// stored in the AST.
//
func (p *parser) next() {
	p.leadComment = nil
	p.lineComment = nil
	prev := p.pos
	p.next0()

	if p.tok == token.COMMENT {
		var comment *types.CommentGroup
		var endline int

		if p.file.Line(p.pos) == p.file.Line(prev) {
			// The comment is on same line as the previous token; it
			// cannot be a lead comment but may be a line comment.
			comment, endline = p.consumeCommentGroup(0)
			if p.file.Line(p.pos) != endline || p.tok == token.EOF {
				// The next token is on a different line, thus
				// the last comment group is a line comment.
				p.lineComment = comment
			}
		}

		// consume successor comments, if any
		endline = -1
		for p.tok == token.COMMENT {
			comment, endline = p.consumeCommentGroup(1)
		}

		if endline+1 == p.file.Line(p.pos) {
			// The next token is following on the line immediately after the
			// comment group, thus the last comment group is a lead comment.
			p.leadComment = comment
		}
	}
}

// A bailout panic is raised to indicate early termination.
type bailout struct{}

func (p *parser) error(pos token.Pos, msg string) {
	epos := p.file.Position(pos)

	// If AllErrors is not set, discard errors reported on the same line
	// as the last recorded error and stop parsing if there are more than
	// 10 errors.
	if p.mode&AllErrors == 0 {
		n := len(p.errors)
		if n > 0 && p.errors[n-1].Pos.Line == epos.Line {
			return // discard - likely a spurious error
		}
		if n > 10 {
			panic(bailout{})
		}
	}

	p.errors.Add(epos, msg)
}

func (p *parser) errorExpected(pos token.Pos, msg string) {
	msg = "expected " + msg
	if pos == p.pos {
		// the error happened at the current position;
		// make the error message more specific
		switch {
		case p.tok == token.SEMICOLON && p.lit == "\n":
			msg += ", found newline"
		case p.tok.IsLiteral():
			// print 123 rather than 'INT', etc.
			msg += ", found " + p.lit
		default:
			msg += ", found '" + p.tok.String() + "'"
		}
	}
	p.error(pos, msg)
}

func (p *parser) expect(tok token.Token) token.Pos {
	pos := p.pos
	if p.tok != tok {
		p.errorExpected(pos, "'"+tok.String()+"'")
	}
	p.next() // make progress
	return pos
}

// expectClosing is like expect but provides a better error message
// for the common case of a missing comma before a newline.
//
func (p *parser) expectClosing(tok token.Token, context string) token.Pos {
	if p.tok != tok && p.tok == token.SEMICOLON && p.lit == "\n" {
		p.error(p.pos, "missing ',' before newline in "+context)
		p.next()
	}
	return p.expect(tok)
}

func (p *parser) expectSemi() {
	// semicolon is optional before a closing ')' or '}'
	if p.tok != token.RPAREN && p.tok != token.RBRACE {
		switch p.tok {
		case token.COMMA:
			// permit a ',' instead of a ';' but complain
			p.errorExpected(p.pos, "';'")
			fallthrough
		case token.SEMICOLON:
			p.next()
		default:
			p.errorExpected(p.pos, "';'")
			p.advance(stmtStart)
		}
	}
}

func (p *parser) atComma(context string, follow token.Token) bool {
	if p.tok == token.COMMA {
		return true
	}
	if p.tok != follow {
		msg := "missing ','"
		if p.tok == token.SEMICOLON && p.lit == "\n" {
			msg += " before newline"
		}
		p.error(p.pos, msg+" in "+context)
		return true // "insert" comma and continue
	}
	return false
}

func assert(cond bool, msg string) {
	if !cond {
		panic("go/parser internal error: " + msg)
	}
}

// advance consumes tokens until the current token p.tok
// is in the 'to' set, or token.EOF. For error recovery.
func (p *parser) advance(to map[token.Token]bool) {
	for ; p.tok != token.EOF; p.next() {
		if to[p.tok] {
			// Return only if parser made some progress since last
			// sync or if it has not reached 10 advance calls without
			// progress. Otherwise consume at least one token to
			// avoid an endless parser loop (it is possible that
			// both parseOperand and parseStmt call advance and
			// correctly do not advance, thus the need for the
			// invocation limit p.syncCnt).
			if p.pos == p.syncPos && p.syncCnt < 10 {
				p.syncCnt++
				return
			}
			if p.pos > p.syncPos {
				p.syncPos = p.pos
				p.syncCnt = 0
				return
			}
			// Reaching here indicates a parser bug, likely an
			// incorrect token list in this function, but it only
			// leads to skipping of possibly correct code if a
			// previous error is present, and thus is preferred
			// over a non-terminating parse.
		}
	}
}

var stmtStart = map[token.Token]bool{
	token.BREAK:       true,
	token.CONST:       true,
	token.CONTINUE:    true,
	token.DEFER:       true,
	token.FALLTHROUGH: true,
	token.FOR:         true,
	token.GO:          true,
	token.GOTO:        true,
	token.IF:          true,
	token.RETURN:      true,
	token.SELECT:      true,
	token.SWITCH:      true,
	token.TYPE:        true,
	token.VAR:         true,
}

var declStart = map[token.Token]bool{
	token.CONST: true,
	token.TYPE:  true,
	token.VAR:   true,
}

var exprEnd = map[token.Token]bool{
	token.COMMA:     true,
	token.COLON:     true,
	token.SEMICOLON: true,
	token.RPAREN:    true,
	token.RBRACK:    true,
	token.RBRACE:    true,
}

// safePos returns a valid file position for a given position: If pos
// is valid to begin with, safePos returns pos. If pos is out-of-range,
// safePos returns the EOF position.
//
// This is hack to work around "artificial" end positions in the AST which
// are computed by adding 1 to (presumably valid) token positions. If the
// token positions are invalid due to parse errors, the resulting end position
// may be past the file's EOF position, which would lead to panics if used
// later on.
//
func (p *parser) safePos(pos token.Pos) (res token.Pos) {
	defer func() {
		if recover() != nil {
			res = token.Pos(p.file.Base() + p.file.Size()) // EOF position
		}
	}()
	_ = p.file.Offset(pos) // trigger a panic if position is out-of-range
	return pos
}

// ----------------------------------------------------------------------------
// Identifiers

func (p *parser) parseIdent() *types.Ident {
	pos := p.pos
	name := "_"
	if p.tok == token.IDENT {
		name = p.lit
		p.next()
	} else {
		p.expect(token.IDENT) // use expect() error handling
	}
	return &types.Ident{NamePos: pos, Name: name}
}

func (p *parser) parseIdentList() (list []*types.Ident) {
	if p.trace {
		defer un(trace(p, "IdentList"))
	}

	list = append(list, p.parseIdent())
	for p.tok == token.COMMA {
		p.next()
		list = append(list, p.parseIdent())
	}

	return
}

// ----------------------------------------------------------------------------
// Common productions

// If lhs is set, result list elements which are identifiers are not resolved.
func (p *parser) parseExprList(lhs bool) (list []types.Expr) {
	if p.trace {
		defer un(trace(p, "ExpressionList"))
	}

	list = append(list, p.checkExpr(p.parseExpr(lhs)))
	for p.tok == token.COMMA {
		p.next()
		list = append(list, p.checkExpr(p.parseExpr(lhs)))
	}

	return
}

func (p *parser) parseLhsList() []types.Expr {
	old := p.inRhs
	p.inRhs = false
	list := p.parseExprList(true)
	switch p.tok {
	case token.DEFINE:
		// lhs of a short variable declaration
		// but doesn't enter scope until later:
		// caller must call p.shortVarDecl(p.makeIdentList(list))
		// at appropriate time.
	case token.COLON:
		// lhs of a label declaration or a communication clause of a select
		// statement (parseLhsList is not called when parsing the case clause
		// of a switch statement):
		// - labels are declared by the caller of parseLhsList
		// - for communication clauses, if there is a stand-alone identifier
		//   followed by a colon, we have a syntax error; there is no need
		//   to resolve the identifier in that case
	default:
		// identifiers must be declared elsewhere
	}
	p.inRhs = old
	return list
}

func (p *parser) parseRhsList() []types.Expr {
	old := p.inRhs
	p.inRhs = true
	list := p.parseExprList(false)
	p.inRhs = old
	return list
}

// ----------------------------------------------------------------------------
// Types

func (p *parser) parseType() types.Expr {
	if p.trace {
		defer un(trace(p, "Type"))
	}

	typ := p.tryType()

	if typ == nil {
		pos := p.pos
		p.errorExpected(pos, "type")
		p.advance(exprEnd)
		return &types.BadExpr{From: pos, To: p.pos}
	}

	return typ
}

// If the result is an identifier, it is not resolved.
func (p *parser) parseTypeName() types.Expr {
	if p.trace {
		defer un(trace(p, "TypeName"))
	}

	ident := p.parseIdent()
	// don't resolve ident yet - it may be a parameter or field name

	if p.tok == token.PERIOD {
		// ident is a package name
		p.next()
		sel := p.parseIdent()
		return &types.SelectorExpr{X: ident, Sel: sel}
	}

	return ident
}

func (p *parser) parseArrayType() types.Expr {
	if p.trace {
		defer un(trace(p, "ArrayType"))
	}

	lbrack := p.expect(token.LBRACK)
	p.exprLev++
	var len types.Expr
	// always permit ellipsis for more fault-tolerant parsing
	if p.tok == token.ELLIPSIS {
		len = &types.Ellipsis{Ellipsis: p.pos}
		p.next()
	} else if p.tok != token.RBRACK {
		len = p.parseRhs()
	}
	p.exprLev--
	p.expect(token.RBRACK)
	elt := p.parseType()

	return &types.ArrayType{Lbrack: lbrack, Len: len, Elt: elt}
}

func (p *parser) makeIdentList(list []types.Expr) []*types.Ident {
	idents := make([]*types.Ident, len(list))
	for i, x := range list {
		ident, isIdent := x.(*types.Ident)
		if !isIdent {
			if _, isBad := x.(*types.BadExpr); !isBad {
				// only report error if it's a new one
				p.errorExpected(x.Pos(), "identifier")
			}
			ident = &types.Ident{NamePos: x.Pos(), Name: "_"}
		}
		idents[i] = ident
	}
	return idents
}

func (p *parser) parseFieldDecl() *types.Field {
	if p.trace {
		defer un(trace(p, "FieldDecl"))
	}

	doc := p.leadComment

	// 1st FieldDecl
	// A type name used as an anonymous field looks like a field identifier.
	var list []types.Expr
	for {
		list = append(list, p.parseVarType(false))
		if p.tok != token.COMMA {
			break
		}
		p.next()
	}

	typ := p.tryVarType(false)

	// analyze case
	var idents []*types.Ident
	if typ != nil {
		// IdentifierList Type
		idents = p.makeIdentList(list)
	} else {
		// ["*"] TypeName (AnonymousField)
		typ = list[0] // we always have at least one element
		if n := len(list); n > 1 {
			p.errorExpected(p.pos, "type")
			typ = &types.BadExpr{From: p.pos, To: p.pos}
		} else if !isTypeName(deref(typ)) {
			p.errorExpected(typ.Pos(), "anonymous field")
			typ = &types.BadExpr{From: typ.Pos(), To: p.safePos(typ.End())}
		}
	}

	// Tag
	var tag *types.BasicLit
	if p.tok == token.STRING {
		tag = &types.BasicLit{ValuePos: p.pos, Kind: p.tok, Value: p.lit}
		p.next()
	}

	p.expectSemi() // call before accessing p.linecomment

	field := &types.Field{Doc: doc, Names: idents, Type: typ, Tag: tag, Comment: p.lineComment}

	return field
}

func (p *parser) parseStructType() *types.StructType {
	if p.trace {
		defer un(trace(p, "StructType"))
	}

	pos := p.expect(token.STRUCT)
	lbrace := p.expect(token.LBRACE)
	var list []*types.Field
	for p.tok == token.IDENT || p.tok == token.MUL || p.tok == token.LPAREN {
		// a field declaration cannot start with a '(' but we accept
		// it here for more robust parsing and better error messages
		// (parseFieldDecl will check and complain if necessary)
		list = append(list, p.parseFieldDecl())
	}
	rbrace := p.expect(token.RBRACE)

	return &types.StructType{
		Struct: pos,
		Fields: &types.FieldList{
			Opening: lbrace,
			List:    list,
			Closing: rbrace,
		},
	}
}

func (p *parser) parsePointerType() *types.StarExpr {
	if p.trace {
		defer un(trace(p, "PointerType"))
	}

	star := p.expect(token.MUL)
	base := p.parseType()

	return &types.StarExpr{Star: star, X: base}
}

// If the result is an identifier, it is not resolved.
func (p *parser) tryVarType(isParam bool) types.Expr {
	if isParam && p.tok == token.ELLIPSIS {
		pos := p.pos
		p.next()
		typ := p.tryIdentOrType() // don't use parseType so we can provide better error message
		if typ != nil {
		} else {
			p.error(pos, "'...' parameter is missing type")
			typ = &types.BadExpr{From: pos, To: p.pos}
		}
		return &types.Ellipsis{Ellipsis: pos, Elt: typ}
	}
	return p.tryIdentOrType()
}

// If the result is an identifier, it is not resolved.
func (p *parser) parseVarType(isParam bool) types.Expr {
	typ := p.tryVarType(isParam)
	if typ == nil {
		pos := p.pos
		p.errorExpected(pos, "type")
		p.next() // make progress
		typ = &types.BadExpr{From: pos, To: p.pos}
	}
	return typ
}

func (p *parser) parseParameterList(ellipsisOk bool) (params []*types.Field) {
	if p.trace {
		defer un(trace(p, "ParameterList"))
	}

	// 1st ParameterDecl
	// A list of identifiers looks like a list of type names.
	var list []types.Expr
	for {
		list = append(list, p.parseVarType(ellipsisOk))
		if p.tok != token.COMMA {
			break
		}
		p.next()
		if p.tok == token.RPAREN {
			break
		}
	}

	// analyze case
	if typ := p.tryVarType(ellipsisOk); typ != nil {
		// IdentifierList Type
		idents := p.makeIdentList(list)
		field := &types.Field{Names: idents, Type: typ}
		params = append(params, field)
		if !p.atComma("parameter list", token.RPAREN) {
			return
		}
		p.next()
		for p.tok != token.RPAREN && p.tok != token.EOF {
			idents := p.parseIdentList()
			typ := p.parseVarType(ellipsisOk)
			field := &types.Field{Names: idents, Type: typ}
			params = append(params, field)
			if !p.atComma("parameter list", token.RPAREN) {
				break
			}
			p.next()
		}
		return
	}

	// Type { "," Type } (anonymous parameters)
	params = make([]*types.Field, len(list))
	for i, typ := range list {
		params[i] = &types.Field{Type: typ}
	}
	return
}

func (p *parser) parseParameters(ellipsisOk bool) *types.FieldList {
	if p.trace {
		defer un(trace(p, "Parameters"))
	}

	var params []*types.Field
	lparen := p.expect(token.LPAREN)
	if p.tok != token.RPAREN {
		params = p.parseParameterList(ellipsisOk)
	}
	rparen := p.expect(token.RPAREN)

	return &types.FieldList{Opening: lparen, List: params, Closing: rparen}
}

func (p *parser) parseResult() *types.FieldList {
	if p.trace {
		defer un(trace(p, "Result"))
	}

	if p.tok == token.LPAREN {
		return p.parseParameters(false)
	}

	typ := p.tryType()
	if typ != nil {
		list := make([]*types.Field, 1)
		list[0] = &types.Field{Type: typ}
		return &types.FieldList{List: list}
	}

	return nil
}

func (p *parser) parseSignature() (params, results *types.FieldList) {
	if p.trace {
		defer un(trace(p, "Signature"))
	}

	params = p.parseParameters(true)
	results = p.parseResult()

	return
}

func (p *parser) parseFuncType() *types.FuncType {
	if p.trace {
		defer un(trace(p, "FuncType"))
	}

	pos := p.expect(token.FUNC)
	params, results := p.parseSignature()

	return &types.FuncType{Func: pos, Params: params, Results: results}
}

func (p *parser) parseMethodSpec() *types.Field {
	if p.trace {
		defer un(trace(p, "MethodSpec"))
	}

	doc := p.leadComment
	var idents []*types.Ident
	var typ types.Expr
	x := p.parseTypeName()
	if ident, isIdent := x.(*types.Ident); isIdent && p.tok == token.LPAREN {
		// method
		idents = []*types.Ident{ident}
		params, results := p.parseSignature()
		typ = &types.FuncType{Func: token.NoPos, Params: params, Results: results}
	} else {
		// embedded interface
		typ = x
	}
	p.expectSemi() // call before accessing p.linecomment

	spec := &types.Field{Doc: doc, Names: idents, Type: typ, Comment: p.lineComment}

	return spec
}

func (p *parser) parseInterfaceType() *types.InterfaceType {
	if p.trace {
		defer un(trace(p, "InterfaceType"))
	}

	pos := p.expect(token.INTERFACE)
	lbrace := p.expect(token.LBRACE)
	var list []*types.Field
	for p.tok == token.IDENT {
		list = append(list, p.parseMethodSpec())
	}
	rbrace := p.expect(token.RBRACE)

	return &types.InterfaceType{
		Interface: pos,
		Methods: &types.FieldList{
			Opening: lbrace,
			List:    list,
			Closing: rbrace,
		},
	}
}

func (p *parser) parseMapType() *types.MapType {
	if p.trace {
		defer un(trace(p, "MapType"))
	}

	pos := p.expect(token.MAP)
	p.expect(token.LBRACK)
	key := p.parseType()
	p.expect(token.RBRACK)
	value := p.parseType()

	return &types.MapType{Map: pos, Key: key, Value: value}
}

func (p *parser) parseChanType() *types.ChanType {
	if p.trace {
		defer un(trace(p, "ChanType"))
	}

	pos := p.pos
	dir := types.SendRecv
	var arrow token.Pos
	if p.tok == token.CHAN {
		p.next()
		if p.tok == token.ARROW {
			arrow = p.pos
			p.next()
			dir = types.SendOnly
		}
	} else {
		arrow = p.expect(token.ARROW)
		p.expect(token.CHAN)
		dir = types.RecvOnly
	}
	value := p.parseType()

	return &types.ChanType{Begin: pos, Arrow: arrow, Dir: dir, Value: value}
}

// If the result is an identifier, it is not resolved.
func (p *parser) tryIdentOrType() types.Expr {
	switch p.tok {
	case token.IDENT:
		return p.parseTypeName()
	case token.LBRACK:
		return p.parseArrayType()
	case token.STRUCT:
		return p.parseStructType()
	case token.MUL:
		return p.parsePointerType()
	case token.FUNC:
		typ := p.parseFuncType()
		return typ
	case token.INTERFACE:
		return p.parseInterfaceType()
	case token.MAP:
		return p.parseMapType()
	case token.CHAN, token.ARROW:
		return p.parseChanType()
	case token.LPAREN:
		lparen := p.pos
		p.next()
		typ := p.parseType()
		rparen := p.expect(token.RPAREN)
		return &types.ParenExpr{Lparen: lparen, X: typ, Rparen: rparen}
	}

	// no type found
	return nil
}

func (p *parser) tryType() types.Expr {
	typ := p.tryIdentOrType()
	return typ
}

// ----------------------------------------------------------------------------
// Blocks

func (p *parser) parseStmtList() (list []types.Stmt) {
	if p.trace {
		defer un(trace(p, "StatementList"))
	}

	for p.tok != token.CASE && p.tok != token.DEFAULT && p.tok != token.RBRACE && p.tok != token.EOF {
		list = append(list, p.parseStmt())
	}

	return
}

func (p *parser) parseBody() *types.BlockStmt {
	if p.trace {
		defer un(trace(p, "Body"))
	}

	lbrace := p.expect(token.LBRACE)
	list := p.parseStmtList()
	rbrace := p.expect(token.RBRACE)

	return &types.BlockStmt{Lbrace: lbrace, List: list, Rbrace: rbrace}
}

func (p *parser) parseBlockStmt() *types.BlockStmt {
	if p.trace {
		defer un(trace(p, "BlockStmt"))
	}

	lbrace := p.expect(token.LBRACE)
	list := p.parseStmtList()
	rbrace := p.expect(token.RBRACE)

	return &types.BlockStmt{Lbrace: lbrace, List: list, Rbrace: rbrace}
}

// ----------------------------------------------------------------------------
// Expressions

func (p *parser) parseFuncTypeOrLit() types.Expr {
	if p.trace {
		defer un(trace(p, "FuncTypeOrLit"))
	}

	typ := p.parseFuncType()
	if p.tok != token.LBRACE {
		// function type only
		return typ
	}

	p.exprLev++
	body := p.parseBody()
	p.exprLev--

	return &types.FuncLit{Typ: typ, Body: body}
}

// parseOperand may return an expression or a raw type (incl. array
// types of the form [...]T. Callers must verify the result.
// If lhs is set and the result is an identifier, it is not resolved.
//
func (p *parser) parseOperand(lhs bool) types.Expr {
	if p.trace {
		defer un(trace(p, "Operand"))
	}

	switch p.tok {
	case token.IDENT:
		x := p.parseIdent()
		return x

	case token.INT, token.FLOAT, token.IMAG, token.CHAR, token.STRING:
		x := &types.BasicLit{ValuePos: p.pos, Kind: p.tok, Value: p.lit}
		p.next()
		return x

	case token.LPAREN:
		lparen := p.pos
		p.next()
		p.exprLev++
		x := p.parseRhsOrType() // types may be parenthesized: (some type)
		p.exprLev--
		rparen := p.expect(token.RPAREN)
		return &types.ParenExpr{Lparen: lparen, X: x, Rparen: rparen}

	case token.FUNC:
		return p.parseFuncTypeOrLit()
	}

	if typ := p.tryIdentOrType(); typ != nil {
		// could be type for composite literal or conversion
		_, isIdent := typ.(*types.Ident)
		assert(!isIdent, "type cannot be identifier")
		return typ
	}

	// we have an error
	pos := p.pos
	p.errorExpected(pos, "operand")
	p.advance(stmtStart)
	return &types.BadExpr{From: pos, To: p.pos}
}

func (p *parser) parseSelector(x types.Expr) types.Expr {
	if p.trace {
		defer un(trace(p, "Selector"))
	}

	sel := p.parseIdent()

	return &types.SelectorExpr{X: x, Sel: sel}
}

func (p *parser) parseTypeAssertion(x types.Expr) types.Expr {
	if p.trace {
		defer un(trace(p, "TypeAssertion"))
	}

	lparen := p.expect(token.LPAREN)
	var typ types.Expr
	if p.tok == token.TYPE {
		// type switch: typ == nil
		p.next()
	} else {
		typ = p.parseType()
	}
	rparen := p.expect(token.RPAREN)

	return &types.TypeAssertExpr{X: x, Typ: typ, Lparen: lparen, Rparen: rparen}
}

func (p *parser) parseIndexOrSlice(x types.Expr) types.Expr {
	if p.trace {
		defer un(trace(p, "IndexOrSlice"))
	}

	const N = 3 // change the 3 to 2 to disable 3-index slices
	lbrack := p.expect(token.LBRACK)
	p.exprLev++
	var index [N]types.Expr
	var colons [N - 1]token.Pos
	if p.tok != token.COLON {
		index[0] = p.parseRhs()
	}
	ncolons := 0
	for p.tok == token.COLON && ncolons < len(colons) {
		colons[ncolons] = p.pos
		ncolons++
		p.next()
		if p.tok != token.COLON && p.tok != token.RBRACK && p.tok != token.EOF {
			index[ncolons] = p.parseRhs()
		}
	}
	p.exprLev--
	rbrack := p.expect(token.RBRACK)

	if ncolons > 0 {
		// slice expression
		slice3 := false
		if ncolons == 2 {
			slice3 = true
			// Check presence of 2nd and 3rd index here rather than during type-checking
			// to prevent erroneous programs from passing through gofmt (was issue 7305).
			if index[1] == nil {
				p.error(colons[0], "2nd index required in 3-index slice")
				index[1] = &types.BadExpr{From: colons[0] + 1, To: colons[1]}
			}
			if index[2] == nil {
				p.error(colons[1], "3rd index required in 3-index slice")
				index[2] = &types.BadExpr{From: colons[1] + 1, To: rbrack}
			}
		}
		return &types.SliceExpr{X: x, Lbrack: lbrack, Low: index[0], High: index[1], Max: index[2], Slice3: slice3, Rbrack: rbrack}
	}

	return &types.IndexExpr{X: x, Lbrack: lbrack, Index: index[0], Rbrack: rbrack}
}

func (p *parser) parseCallOrConversion(fun types.Expr) *types.CallExpr {
	if p.trace {
		defer un(trace(p, "CallOrConversion"))
	}

	lparen := p.expect(token.LPAREN)
	p.exprLev++
	var list []types.Expr
	var ellipsis token.Pos
	for p.tok != token.RPAREN && p.tok != token.EOF && !ellipsis.IsValid() {
		list = append(list, p.parseRhsOrType()) // builtins may expect a type: make(some type, ...)
		if p.tok == token.ELLIPSIS {
			ellipsis = p.pos
			p.next()
		}
		if !p.atComma("argument list", token.RPAREN) {
			break
		}
		p.next()
	}
	p.exprLev--
	rparen := p.expectClosing(token.RPAREN, "argument list")

	return &types.CallExpr{Fun: fun, Lparen: lparen, Args: list, Ellipsis: ellipsis, Rparen: rparen}
}

func (p *parser) parseValue(keyOk bool) types.Expr {
	if p.trace {
		defer un(trace(p, "Element"))
	}

	if p.tok == token.LBRACE {
		return p.parseLiteralValue(nil)
	}

	// Because the parser doesn't know the composite literal type, it cannot
	// know if a key that's an identifier is a struct field name or a name
	// denoting a value. The former is not resolved by the parser or the
	// resolver.
	//
	// Instead, _try_ to resolve such a key if possible. If it resolves,
	// it a) has correctly resolved, or b) incorrectly resolved because
	// the key is a struct field with a name matching another identifier.
	// In the former case we are done, and in the latter case we don't
	// care because the type checker will do a separate field lookup.
	//
	// If the key does not resolve, it a) must be defined at the top
	// level in another file of the same package, the universe scope, or be
	// undeclared; or b) it is a struct field. In the former case, the type
	// checker can do a top-level lookup, and in the latter case it will do
	// a separate field lookup.
	x := p.checkExpr(p.parseExpr(keyOk))
	if keyOk {
		if p.tok == token.COLON {
			// Try to resolve the key but don't collect it
			// as unresolved identifier if it fails so that
			// we don't get (possibly false) errors about
			// undeclared names.
		} else {
			// not a key
		}
	}

	return x
}

func (p *parser) parseElement() types.Expr {
	if p.trace {
		defer un(trace(p, "Element"))
	}

	x := p.parseValue(true)
	if p.tok == token.COLON {
		colon := p.pos
		p.next()
		x = &types.KeyValueExpr{Key: x, Colon: colon, Value: p.parseValue(false)}
	}

	return x
}

func (p *parser) parseElementList() (list []types.Expr) {
	if p.trace {
		defer un(trace(p, "ElementList"))
	}

	for p.tok != token.RBRACE && p.tok != token.EOF {
		list = append(list, p.parseElement())
		if !p.atComma("composite literal", token.RBRACE) {
			break
		}
		p.next()
	}

	return
}

func (p *parser) parseLiteralValue(typ types.Expr) types.Expr {
	if p.trace {
		defer un(trace(p, "LiteralValue"))
	}

	lbrace := p.expect(token.LBRACE)
	var elts []types.Expr
	p.exprLev++
	if p.tok != token.RBRACE {
		elts = p.parseElementList()
	}
	p.exprLev--
	rbrace := p.expectClosing(token.RBRACE, "composite literal")
	return &types.CompositeLit{Typ: typ, Lbrace: lbrace, Elts: elts, Rbrace: rbrace}
}

// checkExpr checks that x is an expression (and not a type).
func (p *parser) checkExpr(x types.Expr) types.Expr {
	switch unparen(x).(type) {
	case *types.BadExpr:
	case *types.Ident:
	case *types.BasicLit:
	case *types.FuncLit:
	case *types.CompositeLit:
	case *types.ParenExpr:
		panic("unreachable")
	case *types.SelectorExpr:
	case *types.IndexExpr:
	case *types.SliceExpr:
	case *types.TypeAssertExpr:
		// If t.Type == nil we have a type assertion of the form
		// y.(type), which is only allowed in type switch expressions.
		// It's hard to exclude those but for the case where we are in
		// a type switch. Instead be lenient and test this in the type
		// checker.
	case *types.CallExpr:
	case *types.StarExpr:
	case *types.UnaryExpr:
	case *types.BinaryExpr:
	default:
		// all other nodes are not proper expressions
		p.errorExpected(x.Pos(), "expression")
		x = &types.BadExpr{From: x.Pos(), To: p.safePos(x.End())}
	}
	return x
}

// isTypeName reports whether x is a (qualified) TypeName.
func isTypeName(x types.Expr) bool {
	switch t := x.(type) {
	case *types.BadExpr:
	case *types.Ident:
	case *types.SelectorExpr:
		_, isIdent := t.X.(*types.Ident)
		return isIdent
	default:
		return false // all other nodes are not type names
	}
	return true
}

// isLiteralType reports whether x is a legal composite literal type.
func isLiteralType(x types.Expr) bool {
	switch t := x.(type) {
	case *types.BadExpr:
	case *types.Ident:
	case *types.SelectorExpr:
		_, isIdent := t.X.(*types.Ident)
		return isIdent
	case *types.ArrayType:
	case *types.StructType:
	case *types.MapType:
	default:
		return false // all other nodes are not legal composite literal types
	}
	return true
}

// If x is of the form *T, deref returns T, otherwise it returns x.
func deref(x types.Expr) types.Expr {
	if p, isPtr := x.(*types.StarExpr); isPtr {
		x = p.X
	}
	return x
}

// If x is of the form (T), unparen returns unparen(T), otherwise it returns x.
func unparen(x types.Expr) types.Expr {
	if p, isParen := x.(*types.ParenExpr); isParen {
		x = unparen(p.X)
	}
	return x
}

// checkExprOrType checks that x is an expression or a type
// (and not a raw type such as [...]T).
//
func (p *parser) checkExprOrType(x types.Expr) types.Expr {
	switch t := unparen(x).(type) {
	case *types.ParenExpr:
		panic("unreachable")
	case *types.UnaryExpr:
	case *types.ArrayType:
		if len, isEllipsis := t.Len.(*types.Ellipsis); isEllipsis {
			p.error(len.Pos(), "expected array length, found '...'")
			x = &types.BadExpr{From: x.Pos(), To: p.safePos(x.End())}
		}
	}

	// all other nodes are expressions or types
	return x
}

// If lhs is set and the result is an identifier, it is not resolved.
func (p *parser) parsePrimaryExpr(lhs bool) types.Expr {
	if p.trace {
		defer un(trace(p, "PrimaryExpr"))
	}

	x := p.parseOperand(lhs)
L:
	for {
		switch p.tok {
		case token.PERIOD:
			p.next()
			if lhs {
			}
			switch p.tok {
			case token.IDENT:
				x = p.parseSelector(p.checkExprOrType(x))
			case token.LPAREN:
				x = p.parseTypeAssertion(p.checkExpr(x))
			default:
				pos := p.pos
				p.errorExpected(pos, "selector or type assertion")
				p.next() // make progress
				sel := &types.Ident{NamePos: pos, Name: "_"}
				x = &types.SelectorExpr{X: x, Sel: sel}
			}
		case token.LBRACK:
			if lhs {
			}
			x = p.parseIndexOrSlice(p.checkExpr(x))
		case token.LPAREN:
			if lhs {
			}
			x = p.parseCallOrConversion(p.checkExprOrType(x))
		case token.LBRACE:
			if isLiteralType(x) && (p.exprLev >= 0 || !isTypeName(x)) {
				if lhs {
				}
				x = p.parseLiteralValue(x)
			} else {
				break L
			}
		default:
			break L
		}
		lhs = false // no need to try to resolve again
	}

	return x
}

// If lhs is set and the result is an identifier, it is not resolved.
func (p *parser) parseUnaryExpr(lhs bool) types.Expr {
	if p.trace {
		defer un(trace(p, "UnaryExpr"))
	}

	switch p.tok {
	case token.ADD, token.SUB, token.NOT, token.XOR, token.AND:
		pos, op := p.pos, p.tok
		p.next()
		x := p.parseUnaryExpr(false)
		return &types.UnaryExpr{OpPos: pos, Op: op, X: p.checkExpr(x)}

	case token.ARROW:
		// channel type or receive expression
		arrow := p.pos
		p.next()

		// If the next token is token.CHAN we still don't know if it
		// is a channel type or a receive operation - we only know
		// once we have found the end of the unary expression. There
		// are two cases:
		//
		//   <- type  => (<-type) must be channel type
		//   <- expr  => <-(expr) is a receive from an expression
		//
		// In the first case, the arrow must be re-associated with
		// the channel type parsed already:
		//
		//   <- (chan type)    =>  (<-chan type)
		//   <- (chan<- type)  =>  (<-chan (<-type))

		x := p.parseUnaryExpr(false)

		// determine which case we have
		if typ, ok := x.(*types.ChanType); ok {
			// (<-type)

			// re-associate position info and <-
			dir := types.SendOnly
			for ok && dir == types.SendOnly {
				if typ.Dir == types.RecvOnly {
					// error: (<-type) is (<-(<-chan T))
					p.errorExpected(typ.Arrow, "'chan'")
				}
				arrow, typ.Begin, typ.Arrow = typ.Arrow, arrow, arrow
				dir, typ.Dir = typ.Dir, types.RecvOnly
				typ, ok = typ.Value.(*types.ChanType)
			}
			if dir == types.SendOnly {
				p.errorExpected(arrow, "channel type")
			}

			return x
		}

		// <-(expr)
		return &types.UnaryExpr{OpPos: arrow, Op: token.ARROW, X: p.checkExpr(x)}

	case token.MUL:
		// pointer type or unary "*" expression
		pos := p.pos
		p.next()
		x := p.parseUnaryExpr(false)
		return &types.StarExpr{Star: pos, X: p.checkExprOrType(x)}
	}

	return p.parsePrimaryExpr(lhs)
}

func (p *parser) tokPrec() (token.Token, int) {
	tok := p.tok
	if p.inRhs && tok == token.ASSIGN {
		tok = token.EQL
	}
	return tok, tok.Precedence()
}

// If lhs is set and the result is an identifier, it is not resolved.
func (p *parser) parseBinaryExpr(lhs bool, prec1 int) types.Expr {
	if p.trace {
		defer un(trace(p, "BinaryExpr"))
	}

	x := p.parseUnaryExpr(lhs)
	for {
		op, oprec := p.tokPrec()
		if oprec < prec1 {
			return x
		}
		pos := p.expect(op)
		if lhs {
			lhs = false
		}
		y := p.parseBinaryExpr(false, oprec+1)
		x = &types.BinaryExpr{X: p.checkExpr(x), OpPos: pos, Op: op, Y: p.checkExpr(y)}
	}
}

// If lhs is set and the result is an identifier, it is not resolved.
// The result may be a type or even a raw type ([...]int). Callers must
// check the result (using checkExpr or checkExprOrType), depending on
// context.
func (p *parser) parseExpr(lhs bool) types.Expr {
	if p.trace {
		defer un(trace(p, "Expression"))
	}

	return p.parseBinaryExpr(lhs, token.LowestPrec+1)
}

func (p *parser) parseRhs() types.Expr {
	old := p.inRhs
	p.inRhs = true
	x := p.checkExpr(p.parseExpr(false))
	p.inRhs = old
	return x
}

func (p *parser) parseRhsOrType() types.Expr {
	old := p.inRhs
	p.inRhs = true
	x := p.checkExprOrType(p.parseExpr(false))
	p.inRhs = old
	return x
}

// ----------------------------------------------------------------------------
// Statements

// Parsing modes for parseSimpleStmt.
const (
	basic = iota
	labelOk
	rangeOk
)

// parseSimpleStmt returns true as 2nd result if it parsed the assignment
// of a range clause (with mode == rangeOk). The returned statement is an
// assignment with a right-hand side that is a single unary expression of
// the form "range x". No guarantees are given for the left-hand side.
func (p *parser) parseSimpleStmt(mode int) (types.Stmt, bool) {
	if p.trace {
		defer un(trace(p, "SimpleStmt"))
	}

	x := p.parseLhsList()

	switch p.tok {
	case
		token.DEFINE, token.ASSIGN, token.ADD_ASSIGN,
		token.SUB_ASSIGN, token.MUL_ASSIGN, token.QUO_ASSIGN,
		token.REM_ASSIGN, token.AND_ASSIGN, token.OR_ASSIGN,
		token.XOR_ASSIGN, token.SHL_ASSIGN, token.SHR_ASSIGN, token.AND_NOT_ASSIGN:
		// assignment statement, possibly part of a range clause
		pos, tok := p.pos, p.tok
		p.next()
		var y []types.Expr
		isRange := false
		if mode == rangeOk && p.tok == token.RANGE && (tok == token.DEFINE || tok == token.ASSIGN) {
			pos := p.pos
			p.next()
			y = []types.Expr{&types.UnaryExpr{OpPos: pos, Op: token.RANGE, X: p.parseRhs()}}
			isRange = true
		} else {
			y = p.parseRhsList()
		}
		as := &types.AssignStmt{Lhs: x, TokPos: pos, Tok: tok, Rhs: y}
		if tok == token.DEFINE {
			p.shortVarDecl(as, x)
		}
		return as, isRange
	}

	if len(x) > 1 {
		p.errorExpected(x[0].Pos(), "1 expression")
		// continue with first expression
	}

	switch p.tok {
	case token.COLON:
		// labeled statement
		colon := p.pos
		p.next()
		if label, isIdent := x[0].(*types.Ident); mode == labelOk && isIdent {
			// Go spec: The scope of a label is the body of the function
			// in which it is declared and excludes the body of any nested
			// function.
			stmt := &types.LabeledStmt{Label: label, Colon: colon, Stmt: p.parseStmt()}
			return stmt, false
		}
		// The label declaration typically starts at x[0].Pos(), but the label
		// declaration may be erroneous due to a token after that position (and
		// before the ':'). If SpuriousErrors is not set, the (only) error
		// reported for the line is the illegal label error instead of the token
		// before the ':' that caused the problem. Thus, use the (latest) colon
		// position for error reporting.
		p.error(colon, "illegal label declaration")
		return &types.BadStmt{From: x[0].Pos(), To: colon + 1}, false

	case token.ARROW:
		// send statement
		arrow := p.pos
		p.next()
		y := p.parseRhs()
		return &types.SendStmt{Chan: x[0], Arrow: arrow, Value: y}, false

	case token.INC, token.DEC:
		// increment or decrement
		s := &types.IncDecStmt{X: x[0], TokPos: p.pos, Tok: p.tok}
		p.next()
		return s, false
	}

	// expression
	return &types.ExprStmt{X: x[0]}, false
}

func (p *parser) parseCallExpr(callType string) *types.CallExpr {
	x := p.parseRhsOrType() // could be a conversion: (some type)(x)
	if call, isCall := x.(*types.CallExpr); isCall {
		return call
	}
	if _, isBad := x.(*types.BadExpr); !isBad {
		// only report error if it's a new one
		p.error(p.safePos(x.End()), fmt.Sprintf("function must be invoked in %s statement", callType))
	}
	return nil
}

func (p *parser) parseGoStmt() types.Stmt {
	if p.trace {
		defer un(trace(p, "GoStmt"))
	}

	pos := p.expect(token.GO)
	call := p.parseCallExpr("go")
	p.expectSemi()
	if call == nil {
		return &types.BadStmt{From: pos, To: pos + 2} // len("go")
	}

	return &types.GoStmt{Go: pos, Call: call}
}

func (p *parser) parseDeferStmt() types.Stmt {
	if p.trace {
		defer un(trace(p, "DeferStmt"))
	}

	pos := p.expect(token.DEFER)
	call := p.parseCallExpr("defer")
	p.expectSemi()
	if call == nil {
		return &types.BadStmt{From: pos, To: pos + 5} // len("defer")
	}

	return &types.DeferStmt{Defer: pos, Call: call}
}

func (p *parser) parseReturnStmt() *types.ReturnStmt {
	if p.trace {
		defer un(trace(p, "ReturnStmt"))
	}

	pos := p.pos
	p.expect(token.RETURN)
	var x []types.Expr
	if p.tok != token.SEMICOLON && p.tok != token.RBRACE {
		x = p.parseRhsList()
	}
	p.expectSemi()

	return &types.ReturnStmt{Return: pos, Results: x}
}

func (p *parser) parseBranchStmt(tok token.Token) *types.BranchStmt {
	if p.trace {
		defer un(trace(p, "BranchStmt"))
	}

	pos := p.expect(tok)
	var label *types.Ident
	if tok != token.FALLTHROUGH && p.tok == token.IDENT {
		label = p.parseIdent()
	}
	p.expectSemi()

	return &types.BranchStmt{TokPos: pos, Tok: tok, Label: label}
}

func (p *parser) makeExpr(s types.Stmt, want string) types.Expr {
	if s == nil {
		return nil
	}
	if es, isExpr := s.(*types.ExprStmt); isExpr {
		return p.checkExpr(es.X)
	}
	found := "simple statement"
	if _, isAss := s.(*types.AssignStmt); isAss {
		found = "assignment"
	}
	p.error(s.Pos(), fmt.Sprintf("expected %s, found %s (missing parentheses around composite literal?)", want, found))
	return &types.BadExpr{From: s.Pos(), To: p.safePos(s.End())}
}

// parseIfHeader is an adjusted version of parser.header
// in cmd/compile/internal/syntax/parser.go, which has
// been tuned for better error handling.
func (p *parser) parseIfHeader() (init types.Stmt, cond types.Expr) {
	if p.tok == token.LBRACE {
		p.error(p.pos, "missing condition in if statement")
		cond = &types.BadExpr{From: p.pos, To: p.pos}
		return
	}
	// p.tok != token.LBRACE

	outer := p.exprLev
	p.exprLev = -1

	if p.tok != token.SEMICOLON {
		// accept potential variable declaration but complain
		if p.tok == token.VAR {
			p.next()
			p.error(p.pos, fmt.Sprintf("var declaration not allowed in 'IF' initializer"))
		}
		init, _ = p.parseSimpleStmt(basic)
	}

	var condStmt types.Stmt
	var semi struct {
		pos token.Pos
		lit string // ";" or "\n"; valid if pos.IsValid()
	}
	if p.tok != token.LBRACE {
		if p.tok == token.SEMICOLON {
			semi.pos = p.pos
			semi.lit = p.lit
			p.next()
		} else {
			p.expect(token.SEMICOLON)
		}
		if p.tok != token.LBRACE {
			condStmt, _ = p.parseSimpleStmt(basic)
		}
	} else {
		condStmt = init
		init = nil
	}

	if condStmt != nil {
		cond = p.makeExpr(condStmt, "boolean expression")
	} else if semi.pos.IsValid() {
		if semi.lit == "\n" {
			p.error(semi.pos, "unexpected newline, expecting { after if clause")
		} else {
			p.error(semi.pos, "missing condition in if statement")
		}
	}

	// make sure we have a valid AST
	if cond == nil {
		cond = &types.BadExpr{From: p.pos, To: p.pos}
	}

	p.exprLev = outer
	return
}

func (p *parser) parseIfStmt() *types.IfStmt {
	if p.trace {
		defer un(trace(p, "IfStmt"))
	}

	pos := p.expect(token.IF)

	init, cond := p.parseIfHeader()
	body := p.parseBlockStmt()

	var else_ types.Stmt
	if p.tok == token.ELSE {
		p.next()
		switch p.tok {
		case token.IF:
			else_ = p.parseIfStmt()
		case token.LBRACE:
			else_ = p.parseBlockStmt()
			p.expectSemi()
		default:
			p.errorExpected(p.pos, "if statement or block")
			else_ = &types.BadStmt{From: p.pos, To: p.pos}
		}
	} else {
		p.expectSemi()
	}

	return &types.IfStmt{If: pos, Init: init, Cond: cond, Body: body, Else: else_}
}

func (p *parser) parseTypeList() (list []types.Expr) {
	if p.trace {
		defer un(trace(p, "TypeList"))
	}

	list = append(list, p.parseType())
	for p.tok == token.COMMA {
		p.next()
		list = append(list, p.parseType())
	}

	return
}

func (p *parser) parseCaseClause(typeSwitch bool) *types.CaseClause {
	if p.trace {
		defer un(trace(p, "CaseClause"))
	}

	pos := p.pos
	var list []types.Expr
	if p.tok == token.CASE {
		p.next()
		if typeSwitch {
			list = p.parseTypeList()
		} else {
			list = p.parseRhsList()
		}
	} else {
		p.expect(token.DEFAULT)
	}

	colon := p.expect(token.COLON)
	body := p.parseStmtList()

	return &types.CaseClause{Case: pos, List: list, Colon: colon, Body: body}
}

func isTypeSwitchAssert(x types.Expr) bool {
	a, ok := x.(*types.TypeAssertExpr)
	return ok && a.Typ == nil
}

func (p *parser) isTypeSwitchGuard(s types.Stmt) bool {
	switch t := s.(type) {
	case *types.ExprStmt:
		// x.(type)
		return isTypeSwitchAssert(t.X)
	case *types.AssignStmt:
		// v := x.(type)
		if len(t.Lhs) == 1 && len(t.Rhs) == 1 && isTypeSwitchAssert(t.Rhs[0]) {
			switch t.Tok {
			case token.ASSIGN:
				// permit v = x.(type) but complain
				p.error(t.TokPos, "expected ':=', found '='")
				fallthrough
			case token.DEFINE:
				return true
			}
		}
	}
	return false
}

func (p *parser) parseSwitchStmt() types.Stmt {
	if p.trace {
		defer un(trace(p, "SwitchStmt"))
	}

	pos := p.expect(token.SWITCH)

	var s1, s2 types.Stmt
	if p.tok != token.LBRACE {
		prevLev := p.exprLev
		p.exprLev = -1
		if p.tok != token.SEMICOLON {
			s2, _ = p.parseSimpleStmt(basic)
		}
		if p.tok == token.SEMICOLON {
			p.next()
			s1 = s2
			s2 = nil
			if p.tok != token.LBRACE {
				// A TypeSwitchGuard may declare a variable in addition
				// to the variable declared in the initial SimpleStmt.
				// Introduce extra scope to avoid redeclaration errors:
				//
				//	switch t := 0; t := x.(T) { ... }
				//
				// (this code is not valid Go because the first t
				// cannot be accessed and thus is never used, the extra
				// scope is needed for the correct error message).
				//
				// If we don't have a type switch, s2 must be an expression.
				// Having the extra nested but empty scope won't affect it.
				s2, _ = p.parseSimpleStmt(basic)
			}
		}
		p.exprLev = prevLev
	}

	typeSwitch := p.isTypeSwitchGuard(s2)
	lbrace := p.expect(token.LBRACE)
	var list []types.Stmt
	for p.tok == token.CASE || p.tok == token.DEFAULT {
		list = append(list, p.parseCaseClause(typeSwitch))
	}
	rbrace := p.expect(token.RBRACE)
	p.expectSemi()
	body := &types.BlockStmt{Lbrace: lbrace, List: list, Rbrace: rbrace}

	if typeSwitch {
		return &types.TypeSwitchStmt{Switch: pos, Init: s1, Assign: s2, Body: body}
	}

	return &types.SwitchStmt{Switch: pos, Init: s1, Tag: p.makeExpr(s2, "switch expression"), Body: body}
}

func (p *parser) parseCommClause() *types.CommClause {
	if p.trace {
		defer un(trace(p, "CommClause"))
	}

	pos := p.pos
	var comm types.Stmt
	if p.tok == token.CASE {
		p.next()
		lhs := p.parseLhsList()
		if p.tok == token.ARROW {
			// SendStmt
			if len(lhs) > 1 {
				p.errorExpected(lhs[0].Pos(), "1 expression")
				// continue with first expression
			}
			arrow := p.pos
			p.next()
			rhs := p.parseRhs()
			comm = &types.SendStmt{Chan: lhs[0], Arrow: arrow, Value: rhs}
		} else {
			// RecvStmt
			if tok := p.tok; tok == token.ASSIGN || tok == token.DEFINE {
				// RecvStmt with assignment
				if len(lhs) > 2 {
					p.errorExpected(lhs[0].Pos(), "1 or 2 expressions")
					// continue with first two expressions
					lhs = lhs[0:2]
				}
				pos := p.pos
				p.next()
				rhs := p.parseRhs()
				as := &types.AssignStmt{Lhs: lhs, TokPos: pos, Tok: tok, Rhs: []types.Expr{rhs}}
				if tok == token.DEFINE {
					p.shortVarDecl(as, lhs)
				}
				comm = as
			} else {
				// lhs must be single receive operation
				if len(lhs) > 1 {
					p.errorExpected(lhs[0].Pos(), "1 expression")
					// continue with first expression
				}
				comm = &types.ExprStmt{X: lhs[0]}
			}
		}
	} else {
		p.expect(token.DEFAULT)
	}

	colon := p.expect(token.COLON)
	body := p.parseStmtList()

	return &types.CommClause{Case: pos, Comm: comm, Colon: colon, Body: body}
}

func (p *parser) parseSelectStmt() *types.SelectStmt {
	if p.trace {
		defer un(trace(p, "SelectStmt"))
	}

	pos := p.expect(token.SELECT)
	lbrace := p.expect(token.LBRACE)
	var list []types.Stmt
	for p.tok == token.CASE || p.tok == token.DEFAULT {
		list = append(list, p.parseCommClause())
	}
	rbrace := p.expect(token.RBRACE)
	p.expectSemi()
	body := &types.BlockStmt{Lbrace: lbrace, List: list, Rbrace: rbrace}

	return &types.SelectStmt{Select: pos, Body: body}
}

func (p *parser) parseForStmt() types.Stmt {
	if p.trace {
		defer un(trace(p, "ForStmt"))
	}

	pos := p.expect(token.FOR)

	var s1, s2, s3 types.Stmt
	var isRange bool
	if p.tok != token.LBRACE {
		prevLev := p.exprLev
		p.exprLev = -1
		if p.tok != token.SEMICOLON {
			if p.tok == token.RANGE {
				// "for range x" (nil lhs in assignment)
				pos := p.pos
				p.next()
				y := []types.Expr{&types.UnaryExpr{OpPos: pos, Op: token.RANGE, X: p.parseRhs()}}
				s2 = &types.AssignStmt{Rhs: y}
				isRange = true
			} else {
				s2, isRange = p.parseSimpleStmt(rangeOk)
			}
		}
		if !isRange && p.tok == token.SEMICOLON {
			p.next()
			s1 = s2
			s2 = nil
			if p.tok != token.SEMICOLON {
				s2, _ = p.parseSimpleStmt(basic)
			}
			p.expectSemi()
			if p.tok != token.LBRACE {
				s3, _ = p.parseSimpleStmt(basic)
			}
		}
		p.exprLev = prevLev
	}

	body := p.parseBlockStmt()
	p.expectSemi()

	if isRange {
		as := s2.(*types.AssignStmt)
		// check lhs
		var key, value types.Expr
		switch len(as.Lhs) {
		case 0:
			// nothing to do
		case 1:
			key = as.Lhs[0]
		case 2:
			key, value = as.Lhs[0], as.Lhs[1]
		default:
			p.errorExpected(as.Lhs[len(as.Lhs)-1].Pos(), "at most 2 expressions")
			return &types.BadStmt{From: pos, To: p.safePos(body.End())}
		}
		// parseSimpleStmt returned a right-hand side that
		// is a single unary expression of the form "range x"
		x := as.Rhs[0].(*types.UnaryExpr).X
		return &types.RangeStmt{
			For:    pos,
			Key:    key,
			Value:  value,
			TokPos: as.TokPos,
			Tok:    as.Tok,
			X:      x,
			Body:   body,
		}
	}

	// regular for statement
	return &types.ForStmt{
		For:  pos,
		Init: s1,
		Cond: p.makeExpr(s2, "boolean or range expression"),
		Post: s3,
		Body: body,
	}
}

func (p *parser) parseStmt() (s types.Stmt) {
	if p.trace {
		defer un(trace(p, "Statement"))
	}

	switch p.tok {
	case token.CONST, token.TYPE, token.VAR:
		s = &types.DeclStmt{Decl: p.parseDecl(stmtStart)}
	case
		// tokens that may start an expression
		token.IDENT, token.INT, token.FLOAT, token.IMAG, token.CHAR, token.STRING, token.FUNC, token.LPAREN, // operands
		token.LBRACK, token.STRUCT, token.MAP, token.CHAN, token.INTERFACE, // composite types
		token.ADD, token.SUB, token.MUL, token.AND, token.XOR, token.ARROW, token.NOT: // unary operators
		s, _ = p.parseSimpleStmt(labelOk)
		// because of the required look-ahead, labeled statements are
		// parsed by parseSimpleStmt - don't expect a semicolon after
		// them
		if _, isLabeledStmt := s.(*types.LabeledStmt); !isLabeledStmt {
			p.expectSemi()
		}
	case token.GO:
		s = p.parseGoStmt()
	case token.DEFER:
		s = p.parseDeferStmt()
	case token.RETURN:
		s = p.parseReturnStmt()
	case token.BREAK, token.CONTINUE, token.GOTO, token.FALLTHROUGH:
		s = p.parseBranchStmt(p.tok)
	case token.LBRACE:
		s = p.parseBlockStmt()
		p.expectSemi()
	case token.IF:
		s = p.parseIfStmt()
	case token.SWITCH:
		s = p.parseSwitchStmt()
	case token.SELECT:
		s = p.parseSelectStmt()
	case token.FOR:
		s = p.parseForStmt()
	case token.SEMICOLON:
		// Is it ever possible to have an implicit semicolon
		// producing an empty statement in a valid program?
		// (handle correctly anyway)
		s = &types.EmptyStmt{Semicolon: p.pos, Implicit: p.lit == "\n"}
		p.next()
	case token.RBRACE:
		// a semicolon may be omitted before a closing "}"
		s = &types.EmptyStmt{Semicolon: p.pos, Implicit: true}
	default:
		// no statement found
		pos := p.pos
		p.errorExpected(pos, "statement")
		p.advance(stmtStart)
		s = &types.BadStmt{From: pos, To: p.pos}
	}

	return
}

// ----------------------------------------------------------------------------
// Declarations

type parseSpecFunction func(doc *types.CommentGroup, keyword token.Token, iota int) types.Spec

func isValidImport(lit string) bool {
	const illegalChars = `!"#$%&'()*,:;<=>?[\]^{|}` + "`\uFFFD"
	s, _ := strconv.Unquote(lit) // go/scanner returns a legal string literal
	for _, r := range s {
		if !unicode.IsGraphic(r) || unicode.IsSpace(r) || strings.ContainsRune(illegalChars, r) {
			return false
		}
	}
	return s != ""
}

func (p *parser) parseImportSpec(doc *types.CommentGroup, _ token.Token, _ int) types.Spec {
	if p.trace {
		defer un(trace(p, "ImportSpec"))
	}

	var ident *types.Ident
	switch p.tok {
	case token.PERIOD:
		ident = &types.Ident{NamePos: p.pos, Name: "."}
		p.next()
	case token.IDENT:
		ident = p.parseIdent()
	}

	pos := p.pos
	var path string
	if p.tok == token.STRING {
		path = p.lit
		if !isValidImport(path) {
			p.error(pos, "invalid import path: "+path)
		}
		p.next()
	} else {
		p.expect(token.STRING) // use expect() error handling
	}
	p.expectSemi() // call before accessing p.linecomment

	// collect imports
	spec := &types.ImportSpec{
		Doc:     doc,
		Name:    ident,
		Path:    &types.BasicLit{ValuePos: pos, Kind: token.STRING, Value: path},
		Comment: p.lineComment,
	}
	p.imports = append(p.imports, spec)

	return spec
}

func (p *parser) parseValueSpec(doc *types.CommentGroup, keyword token.Token, iota int) types.Spec {
	if p.trace {
		defer un(trace(p, keyword.String()+"Spec"))
	}

	pos := p.pos
	idents := p.parseIdentList()
	typ := p.tryType()
	var values []types.Expr
	// always permit optional initialization for more tolerant parsing
	if p.tok == token.ASSIGN {
		p.next()
		values = p.parseRhsList()
	}
	p.expectSemi() // call before accessing p.linecomment

	switch keyword {
	case token.VAR:
		if typ == nil && values == nil {
			p.error(pos, "missing variable type or initialization")
		}
	case token.CONST:
		if values == nil && (iota == 0 || typ != nil) {
			p.error(pos, "missing constant value")
		}
	}

	// Go spec: The scope of a constant or variable identifier declared inside
	// a function begins at the end of the ConstSpec or VarSpec and ends at
	// the end of the innermost containing block.
	// (Global identifiers are resolved in a separate phase after parsing.)
	spec := &types.ValueSpec{
		Doc:     doc,
		Names:   idents,
		Type:    typ,
		Values:  values,
		Comment: p.lineComment,
	}

	return spec
}

func (p *parser) parseTypeSpec(doc *types.CommentGroup, _ token.Token, _ int) types.Spec {
	if p.trace {
		defer un(trace(p, "TypeSpec"))
	}

	ident := p.parseIdent()

	// Go spec: The scope of a type identifier declared inside a function begins
	// at the identifier in the TypeSpec and ends at the end of the innermost
	// containing block.
	// (Global identifiers are resolved in a separate phase after parsing.)
	spec := &types.TypeSpec{Doc: doc, Name: ident}
	if p.tok == token.ASSIGN {
		spec.Assign = p.pos
		p.next()
	}
	spec.Type = p.parseType()
	p.expectSemi() // call before accessing p.linecomment
	spec.Comment = p.lineComment

	return spec
}

func (p *parser) parseGenDecl(keyword token.Token, f parseSpecFunction) *types.GenDecl {
	if p.trace {
		defer un(trace(p, "GenDecl("+keyword.String()+")"))
	}

	doc := p.leadComment
	pos := p.expect(keyword)
	var lparen, rparen token.Pos
	var list []types.Spec
	if p.tok == token.LPAREN {
		lparen = p.pos
		p.next()
		for iota := 0; p.tok != token.RPAREN && p.tok != token.EOF; iota++ {
			list = append(list, f(p.leadComment, keyword, iota))
		}
		rparen = p.expect(token.RPAREN)
		p.expectSemi()
	} else {
		list = append(list, f(nil, keyword, 0))
	}

	return &types.GenDecl{
		Doc:    doc,
		TokPos: pos,
		Tok:    keyword,
		Lparen: lparen,
		Specs:  list,
		Rparen: rparen,
	}
}

func (p *parser) parseFuncDecl() *types.FuncDecl {
	if p.trace {
		defer un(trace(p, "FunctionDecl"))
	}

	doc := p.leadComment
	pos := p.expect(token.FUNC)

	var recv *types.FieldList
	if p.tok == token.LPAREN {
		recv = p.parseParameters(false)
	}

	ident := p.parseIdent()

	params, results := p.parseSignature()

	var body *types.BlockStmt
	if p.tok == token.LBRACE {
		body = p.parseBody()
	}
	p.expectSemi()

	decl := &types.FuncDecl{
		Doc:  doc,
		Recv: recv,
		Name: ident,
		Type: &types.FuncType{
			Func:    pos,
			Params:  params,
			Results: results,
		},
		Body: body,
	}

	return decl
}

func (p *parser) parseDecl(sync map[token.Token]bool) types.Decl {
	if p.trace {
		defer un(trace(p, "Declaration"))
	}

	var f parseSpecFunction
	switch p.tok {
	case token.CONST, token.VAR:
		f = p.parseValueSpec

	case token.TYPE:
		f = p.parseTypeSpec

	case token.FUNC:
		return p.parseFuncDecl()

	default:
		pos := p.pos
		p.errorExpected(pos, "declaration")
		p.advance(sync)
		return &types.BadDecl{From: pos, To: p.pos}
	}

	return p.parseGenDecl(p.tok, f)
}

// ----------------------------------------------------------------------------
// Source files

func (p *parser) parseFile() *types.File {
	if p.trace {
		defer un(trace(p, "File"))
	}

	// Don't bother parsing the rest if we had errors scanning the first token.
	// Likely not a Go source file at all.
	if p.errors.Len() != 0 {
		return nil
	}

	// package clause
	doc := p.leadComment
	pos := p.expect(token.PACKAGE)
	// Go spec: The package clause is not a declaration;
	// the package name does not appear in any scope.
	ident := p.parseIdent()
	if ident.Name == "_" && p.mode&DeclarationErrors != 0 {
		p.error(p.pos, "invalid package name _")
	}
	p.expectSemi()

	// Don't bother parsing the rest if we had errors parsing the package clause.
	// Likely not a Go source file at all.
	if p.errors.Len() != 0 {
		return nil
	}

	var decls []types.Decl
	if p.mode&PackageClauseOnly == 0 {
		// import decls
		for p.tok == token.IMPORT {
			decls = append(decls, p.parseGenDecl(token.IMPORT, p.parseImportSpec))
		}

		if p.mode&ImportsOnly == 0 {
			// rest of package body
			for p.tok != token.EOF {
				decls = append(decls, p.parseDecl(declStart))
			}
		}
	}
	return &types.File{
		Doc:      doc,
		Package:  pos,
		Name:     ident,
		Decls:    decls,
		Imports:  p.imports,
		Comments: p.comments,
	}
}
