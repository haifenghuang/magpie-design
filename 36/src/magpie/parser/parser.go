package parser

import (
	"fmt"
	"io/ioutil"
	"magpie/ast"
	"magpie/lexer"
	"magpie/token"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"unicode/utf8"
)

const (
	_ int = iota
	LOWEST
	ASSIGN       //=
	CONDOR       // ||
	CONDAND      // &&
	EQUALS       //==, !=
	LESSGREATER  //<, <=, >, >=
	SUM          //+, -
	PRODUCT      //*, /, %, **
	REGEXP_MATCH // !~, ~=
	PREFIX       //!true, -10
	INCREMENT    //++, --
	CALL         //add(1,2), array[index], obj.add(1,2)
)

var precedences = map[token.TokenType]int{
	token.TOKEN_ASSIGN: ASSIGN,
	token.TOKEN_OR:     CONDOR,
	token.TOKEN_AND:    CONDAND,

	token.TOKEN_EQ:  EQUALS,
	token.TOKEN_NEQ: EQUALS,

	token.TOKEN_LT: LESSGREATER,
	token.TOKEN_LE: LESSGREATER,
	token.TOKEN_GT: LESSGREATER,
	token.TOKEN_GE: LESSGREATER,

	token.TOKEN_PLUS:     SUM,
	token.TOKEN_MINUS:    SUM,
	token.TOKEN_MULTIPLY: PRODUCT,
	token.TOKEN_DIVIDE:   PRODUCT,
	token.TOKEN_MOD:      PRODUCT,
	token.TOKEN_POWER:    PRODUCT,

	token.TOKEN_LPAREN:    CALL,
	token.TOKEN_DOT:       CALL,
	token.TOKEN_LBRACKET:  CALL,
	token.TOKEN_INCREMENT: INCREMENT,
	token.TOKEN_DECREMENT: INCREMENT,

	token.TOKEN_MATCH:    REGEXP_MATCH,
	token.TOKEN_NOTMATCH: REGEXP_MATCH,
}

type (
	prefixParseFn func() ast.Expression
	infixParseFn  func(ast.Expression) ast.Expression
)

type Parser struct {
	l          *lexer.Lexer
	errors     []string //error messages
	errorLines []string //for using with wasm communication.

	curToken  token.Token
	peekToken token.Token

	prefixParseFns map[token.TokenType]prefixParseFn
	infixParseFns  map[token.TokenType]infixParseFn

	loopDepth        int // current loop depth (0 if not in any loops)
	fallthroughDepth int //current fallthrough depth (0 if not in switch cases)
}

func (p *Parser) registerPrefix(tokenType token.TokenType, fn prefixParseFn) {
	p.prefixParseFns[tokenType] = fn
}

func (p *Parser) registerInfix(tokenType token.TokenType, fn infixParseFn) {
	p.infixParseFns[tokenType] = fn
}

func NewParser(l *lexer.Lexer) *Parser {
	p := &Parser{
		l:          l,
		errors:     []string{},
		errorLines: []string{},
	}

	p.registerAction()

	p.nextToken()
	p.nextToken()
	return p
}

func (p *Parser) registerAction() {
	p.prefixParseFns = make(map[token.TokenType]prefixParseFn)
	p.registerPrefix(token.TOKEN_ILLEGAL, p.parsePrefixIllegalExpression)
	p.registerPrefix(token.TOKEN_NUMBER, p.parseNumber)
	p.registerPrefix(token.TOKEN_IDENTIFIER, p.parseIdentifier)
	p.registerPrefix(token.TOKEN_STRING, p.parseStringLiteral)
	p.registerPrefix(token.TOKEN_FUNCTION, p.parseFunctionLiteral)
	p.registerPrefix(token.TOKEN_TRUE, p.parseBooleanLiteral)
	p.registerPrefix(token.TOKEN_FALSE, p.parseBooleanLiteral)
	p.registerPrefix(token.TOKEN_LBRACKET, p.parseArrayLiteral)
	p.registerPrefix(token.TOKEN_LBRACE, p.parseHashLiteral)
	p.registerPrefix(token.TOKEN_REGEX, p.parseRegexpLiteral)
	p.registerPrefix(token.TOKEN_NIL, p.parseNilExpression)
	p.registerPrefix(token.TOKEN_PLUS, p.parsePrefixExpression)
	p.registerPrefix(token.TOKEN_MINUS, p.parsePrefixExpression)
	p.registerPrefix(token.TOKEN_BANG, p.parsePrefixExpression)
	p.registerPrefix(token.TOKEN_LPAREN, p.parseGroupedExpression)
	p.registerPrefix(token.TOKEN_IF, p.parseIfExpression)
	p.registerPrefix(token.TOKEN_SWITCH, p.parseSwitchExpression)
	p.registerPrefix(token.TOKEN_FALLTHROUGH, p.parseFallThroughExpression)

	p.registerPrefix(token.TOKEN_DO, p.parseDoLoopExpression)
	p.registerPrefix(token.TOKEN_WHILE, p.parseWhileLoopExpression)
	p.registerPrefix(token.TOKEN_FOR, p.parseForLoopExpression)
	p.registerPrefix(token.TOKEN_BREAK, p.parseBreakExpression)
	p.registerPrefix(token.TOKEN_CONTINUE, p.parseContinueExpression)

	p.infixParseFns = make(map[token.TokenType]infixParseFn)
	p.registerPrefix(token.TOKEN_ILLEGAL, p.parseInfixIllegalExpression)
	p.registerInfix(token.TOKEN_PLUS, p.parseInfixExpression)
	p.registerInfix(token.TOKEN_MINUS, p.parseInfixExpression)
	p.registerInfix(token.TOKEN_MULTIPLY, p.parseInfixExpression)
	p.registerInfix(token.TOKEN_DIVIDE, p.parseInfixExpression)
	p.registerInfix(token.TOKEN_MOD, p.parseInfixExpression)
	p.registerInfix(token.TOKEN_POWER, p.parseInfixExpression)
	p.registerInfix(token.TOKEN_LPAREN, p.parseCallExpression)
	p.registerInfix(token.TOKEN_LBRACKET, p.parseIndexExpression)

	p.registerInfix(token.TOKEN_LT, p.parseInfixExpression)
	p.registerInfix(token.TOKEN_LE, p.parseInfixExpression)
	p.registerInfix(token.TOKEN_GT, p.parseInfixExpression)
	p.registerInfix(token.TOKEN_GE, p.parseInfixExpression)
	p.registerInfix(token.TOKEN_EQ, p.parseInfixExpression)
	p.registerInfix(token.TOKEN_NEQ, p.parseInfixExpression)

	p.registerInfix(token.TOKEN_AND, p.parseInfixExpression)
	p.registerInfix(token.TOKEN_OR, p.parseInfixExpression)

	p.registerInfix(token.TOKEN_MATCH, p.parseInfixExpression)
	p.registerInfix(token.TOKEN_NOTMATCH, p.parseInfixExpression)

	p.registerInfix(token.TOKEN_INCREMENT, p.parsePostfixExpression)
	p.registerInfix(token.TOKEN_DECREMENT, p.parsePostfixExpression)

	p.registerInfix(token.TOKEN_DOT, p.parseMethodCallExpression)

	p.registerInfix(token.TOKEN_ASSIGN, p.parseAssignExpression)
}

func (p *Parser) ParseProgram() *ast.Program {
	defer func() {
		if r := recover(); r != nil {
			fmt.Printf("Syntax Error:%v- %s\n", p.curToken.Pos, r)
		}
	}()

	program := &ast.Program{}

	program.Statements = []ast.Statement{}
	program.Imports = make(map[string]*ast.ImportStatement)

	for p.curToken.Type != token.TOKEN_EOF {
		stmt := p.parseStatement()
		if stmt != nil {
			if importStmt, ok := stmt.(*ast.ImportStatement); ok {
				importPath := importStmt.ImportPath
				_, ok := program.Imports[importPath]
				if !ok { //if not ok, we need to import it, or else we do not want to import twice
					program.Imports[importPath] = importStmt
				}
			} else {
				program.Statements = append(program.Statements, stmt)
			}
		}
		p.nextToken()
	}

	return program
}

func (p *Parser) parseStatement() ast.Statement {
	switch p.curToken.Type {
	case token.TOKEN_IMPORT:
		return p.parseImportStatement()
	case token.TOKEN_LET:
		return p.parseLetStatement(true)
	case token.TOKEN_RETURN:
		return p.parseReturnStatement()
	case token.TOKEN_LBRACE:
		return p.parseBlockStatement()
	case token.TOKEN_STRUCT:
		return p.parseStructStatement()
	case token.TOKEN_IDENTIFIER:
		//if the current token is an 'identifier' and next token is a ',',
		//then we assume it's a multiple assignment, but we treat it as a 'let' statement.
		//otherwise, we just fallthrough.
		if p.peekTokenIs(token.TOKEN_COMMA) {
			return p.parseLetStatement(false)
		}
		fallthrough
	default:
		return p.parseExpressionStatement()
	}
}

func (p *Parser) parseImportStatement() *ast.ImportStatement {
	stmt := &ast.ImportStatement{Token: p.curToken}

	p.nextToken()

	paths := []string{}
	paths = append(paths, p.curToken.Literal)

	for p.peekTokenIs(token.TOKEN_DOT) {
		p.nextToken() //skip current token
		p.nextToken() //skip '.'
		paths = append(paths, p.curToken.Literal)
	}

	path := strings.TrimSpace(strings.Join(paths, "/"))
	stmt.ImportPath = filepath.Base(path)

	program, err := p.getImportedStatements(path)
	if err != nil {
		p.errors = append(p.errors, err.Error())
		p.errorLines = append(p.errorLines, p.curToken.Pos.Sline())
		return stmt
	}

	if p.peekTokenIs(token.TOKEN_SEMICOLON) {
		p.nextToken()
	}

	stmt.Program = program
	return stmt
}

func (p *Parser) getImportedStatements(importpath string) (*ast.Program, error) {
	var path string
	if p.l.Filename == "" { //when we call lexer with string, not file
		path, _ = os.Getwd()
	} else {
		path, _ = filepath.Abs(p.l.Filename)
		path = filepath.Dir(path)
	}

	fn := filepath.Join(path, importpath+".mp")
	f, err := ioutil.ReadFile(fn)
	if err != nil { //error occurred, maybe the file do not exists.
		// Check for 'MAGPIE_ROOT' environment variable
		importRoot := os.Getenv("MAGPIE_ROOT")
		if len(importRoot) == 0 { //'MAGPIE_ROOT' environment variable is not set
			return nil, fmt.Errorf("Syntax Error:%v- no file or directory: %s.mp, %s", p.curToken.Pos, importpath, path)
		} else {
			fn = filepath.Join(importRoot, importpath+".mp")
			e, err := ioutil.ReadFile(fn)
			if err != nil {
				return nil, fmt.Errorf("Syntax Error:%v- no file or directory: %s.mp, %s", p.curToken.Pos, importpath, importRoot)
			}
			f = e
		}
	}

	l := lexer.NewLexer(string(f))
	l.Filename = fn

	ps := NewParser(l)
	parsed := ps.ParseProgram()
	if len(ps.errors) != 0 {
		p.errors = append(p.errors, ps.errors...)
		p.errorLines = append(p.errorLines, ps.errorLines...)
	}
	return parsed, nil
}

//let a,b,c = 1,2,3 (with assignment)
//let a; (without assignment, 'a' is assumed to be 'nil')
func (p *Parser) parseLetStatement(nextFlag bool) *ast.LetStatement {
	var stmt *ast.LetStatement
	if !nextFlag {
		//construct a dummy 'let' token
		tok := token.Token{Pos: p.curToken.Pos, Type: token.TOKEN_LET, Literal: "let"}
		stmt = &ast.LetStatement{Token: tok}
	} else {
		stmt = &ast.LetStatement{Token: p.curToken}
	}

	//parse left hand side of the assignment
	for {
		if nextFlag {
			p.nextToken()
		}
		nextFlag = true

		if !p.curTokenIs(token.TOKEN_IDENTIFIER) && p.curToken.Literal != "_" {
			msg := fmt.Sprintf("Syntax Error:%v- expected token to be identifier|underscore, got %s instead.", p.curToken.Pos, p.curToken.Type)
			p.errors = append(p.errors, msg)
			p.errorLines = append(p.errorLines, p.curToken.Pos.Sline())
			return stmt
		}
		name := &ast.Identifier{Token: p.curToken, Value: p.curToken.Literal}
		if p.curToken.Literal == "self" {
			msg := fmt.Sprintf("Syntax Error:%v- 'self' can not be assigned", p.curToken.Pos)
			p.errors = append(p.errors, msg)
			p.errorLines = append(p.errorLines, p.curToken.Pos.Sline())
			return nil
		}
		stmt.Names = append(stmt.Names, name)

		p.nextToken()
		if p.curTokenIs(token.TOKEN_ASSIGN) || p.curTokenIs(token.TOKEN_SEMICOLON) {
			break
		}
		if !p.curTokenIs(token.TOKEN_COMMA) {
			msg := fmt.Sprintf("Syntax Error:%v- expected token to be comma, got %s instead.", p.curToken.Pos, p.curToken.Type)
			p.errors = append(p.errors, msg)
			p.errorLines = append(p.errorLines, p.curToken.Pos.Sline())
			return stmt
		}
	}

	if p.curTokenIs(token.TOKEN_SEMICOLON) { //let x;
		return stmt
	}

	p.nextToken()
	for {
		v := p.parseExpressionStatement().Expression
		stmt.Values = append(stmt.Values, v)

		if !p.peekTokenIs(token.TOKEN_COMMA) {
			break
		}
		p.nextToken()
		p.nextToken()
	}

	return stmt
}

func (p *Parser) parseReturnStatement() *ast.ReturnStatement {
	stmt := &ast.ReturnStatement{Token: p.curToken, ReturnValues: []ast.Expression{}}
	if p.peekTokenIs(token.TOKEN_SEMICOLON) { //e.g.{ return; }
		p.nextToken()
		return stmt
	}
	if p.peekTokenIs(token.TOKEN_RBRACE) { //e.g. { return }
		return stmt
	}

	p.nextToken()
	for {
		v := p.parseExpressionStatement().Expression
		stmt.ReturnValues = append(stmt.ReturnValues, v)

		if !p.peekTokenIs(token.TOKEN_COMMA) {
			break
		}
		p.nextToken()
		p.nextToken()
	}

	if len(stmt.ReturnValues) > 0 {
		stmt.ReturnValue = stmt.ReturnValues[0]
	}
	return stmt
}

func (p *Parser) parseBlockStatement() *ast.BlockStatement {
	blockStmt := &ast.BlockStatement{Token: p.curToken}
	blockStmt.Statements = []ast.Statement{}
	p.nextToken()
	for !p.curTokenIs(token.TOKEN_RBRACE) {
		stmt := p.parseStatement()
		if stmt != nil {
			blockStmt.Statements = append(blockStmt.Statements, stmt)
		}
		if p.peekTokenIs(token.TOKEN_EOF) {
			break
		}
		p.nextToken()
	}

	blockStmt.RBraceToken = p.curToken
	return blockStmt
}

func (p *Parser) parseExpressionStatement() *ast.ExpressionStatement {
	stmt := &ast.ExpressionStatement{Token: p.curToken}

	stmt.Expression = p.parseExpression(LOWEST)

	if p.peekTokenIs(token.TOKEN_SEMICOLON) {
		p.nextToken()
	}
	return stmt
}

func (p *Parser) parseExpression(precedence int) ast.Expression {
	prefix := p.prefixParseFns[p.curToken.Type]
	if prefix == nil {
		p.noPrefixParseFnError(p.curToken.Type)
		return nil
	}
	leftExp := prefix()

	// Run the infix function until the next token has a higher precedence.
	for precedence < p.peekPrecedence() {
		infix := p.infixParseFns[p.peekToken.Type]
		if infix == nil {
			return leftExp
		}
		p.nextToken()
		leftExp = infix(leftExp)
	}

	return leftExp
}

func (p *Parser) parseAssignExpression(name ast.Expression) ast.Expression {
	if name.String() == "self" {
		msg := fmt.Sprintf("Syntax Error:%v- 'self' can not be assigned", p.curToken.Pos)
		p.errors = append(p.errors, msg)
		p.errorLines = append(p.errorLines, p.curToken.Pos.Sline())
		return nil
	}
	a := &ast.AssignExpression{Token: p.curToken, Name: name}

	p.nextToken()
	a.Value = p.parseExpression(LOWEST)

	return a
}

func (p *Parser) parsePrefixExpression() ast.Expression {
	expression := &ast.PrefixExpression{Token: p.curToken, Operator: p.curToken.Literal}
	p.nextToken()
	expression.Right = p.parseExpression(PREFIX)

	return expression
}

func (p *Parser) parseInfixExpression(left ast.Expression) ast.Expression {
	expression := &ast.InfixExpression{
		Token:    p.curToken,
		Operator: p.curToken.Literal,
		Left:     left,
	}
	precedence := p.curPrecedence()

	// if the token is '**', we process it specially. e.g. 3 ** 2 ** 3 = 3 ** (2 ** 3)
	// i.e. Exponent operator '**'' has right-to-left associativity
	if p.curTokenIs(token.TOKEN_POWER) {
		precedence--
	}

	p.nextToken()
	expression.Right = p.parseExpression(precedence)

	return expression
}

func (p *Parser) parseGroupedExpression() ast.Expression {
	savedToken := p.curToken
	p.nextToken()

	// NOTE: if previous token is token.TOKEN_LPAREN, and the current
	//       token is token.TOKEN_RPAREN, that is an empty parentheses,
	//       we need to return earlier.
	if savedToken.Type == token.TOKEN_LPAREN && p.curTokenIs(token.TOKEN_RPAREN) {
		//empty tuple, e.g. 'x = ()'
		return &ast.TupleLiteral{Token: savedToken, Members: []ast.Expression{}}
	}

	exp := p.parseExpression(LOWEST)

	if p.peekTokenIs(token.TOKEN_COMMA) {
		p.nextToken()
		ret := p.parseTupleExpression(savedToken, exp)
		return ret
	}

	if !p.expectPeek(token.TOKEN_RPAREN) {
		return nil
	}

	return exp
}

func (p *Parser) parsePrefixIllegalExpression() ast.Expression {
	msg := fmt.Sprintf("Syntax Error:%v - Illegal token found. Literal: '%s'", p.curToken.Pos, p.curToken.Literal)
	p.errors = append(p.errors, msg)
	p.errorLines = append(p.errorLines, p.curToken.Pos.Sline())
	return nil
}

func (p *Parser) parseInfixIllegalExpression() ast.Expression {
	msg := fmt.Sprintf("Syntax Error:%v - Illegal token found. Literal: '%s'", p.curToken.Pos, p.curToken.Literal)
	p.errors = append(p.errors, msg)
	p.errorLines = append(p.errorLines, p.curToken.Pos.Sline())
	return nil
}

func (p *Parser) parseNumber() ast.Expression {
	lit := &ast.NumberLiteral{Token: p.curToken}

	value, err := strconv.ParseFloat(p.curToken.Literal, 64)
	if err != nil {
		msg := fmt.Sprintf("Syntax Error:%v - could not parse %q as float", p.curToken.Pos, p.curToken.Literal)
		p.errors = append(p.errors, msg)
		p.errorLines = append(p.errorLines, p.curToken.Pos.Sline())
		return nil
	}
	lit.Value = value
	return lit
}

func (p *Parser) parseIdentifier() ast.Expression {
	return &ast.Identifier{Token: p.curToken, Value: p.curToken.Literal}
}

func (p *Parser) parseBooleanLiteral() ast.Expression {
	return &ast.BooleanLiteral{Token: p.curToken, Value: p.curTokenIs(token.TOKEN_TRUE)}
}

func (p *Parser) parseStringLiteral() ast.Expression {
	return &ast.StringLiteral{Token: p.curToken, Value: p.curToken.Literal}
}

func (p *Parser) parseArrayLiteral() ast.Expression {
	array := &ast.ArrayLiteral{Token: p.curToken}
	array.Members = p.parseExpressionList(token.TOKEN_RBRACKET)
	return array
}

func (p *Parser) parseExpressionList(end token.TokenType) []ast.Expression {
	list := []ast.Expression{}
	if p.peekTokenIs(end) {
		p.nextToken()
		return list
	}

	p.nextToken()
	list = append(list, p.parseExpression(LOWEST))
	for p.peekTokenIs(token.TOKEN_COMMA) {
		p.nextToken()
		p.nextToken()
		list = append(list, p.parseExpression(LOWEST))
	}

	if !p.expectPeek(end) {
		return nil
	}

	return list
}

func (p *Parser) parseHashLiteral() ast.Expression {
	hash := &ast.HashLiteral{Token: p.curToken}
	hash.Pairs = make(map[ast.Expression]ast.Expression)
	for !p.peekTokenIs(token.TOKEN_RBRACE) {
		p.nextToken()
		key := p.parseExpression(LOWEST)
		if !p.expectPeek(token.TOKEN_COLON) {
			return nil
		}

		p.nextToken()
		value := p.parseExpression(LOWEST)
		hash.Pairs[key] = value
		if !p.peekTokenIs(token.TOKEN_RBRACE) && !p.expectPeek(token.TOKEN_COMMA) {
			return nil
		}
	}

	if !p.expectPeek(token.TOKEN_RBRACE) {
		return nil
	}

	return hash
}

// parses a regular-expression
func (p *Parser) parseRegexpLiteral() ast.Expression {
	return &ast.RegExLiteral{Token: p.curToken, Value: p.curToken.Literal}
}

func (p *Parser) parseTupleExpression(tok token.Token, expr ast.Expression) ast.Expression {
	members := []ast.Expression{expr}

	oldToken := tok
	for {
		switch p.curToken.Type {
		case token.TOKEN_RPAREN:
			ret := &ast.TupleLiteral{Token: tok, Members: members}
			return ret
		case token.TOKEN_COMMA:
			p.nextToken()
			//For a 1-tuple: "(1,)", the trailing comma is necessary to distinguish it
			//from the parenthesized expression (1).
			if p.curTokenIs(token.TOKEN_RPAREN) { //e.g.  let x = (1,)
				ret := &ast.TupleLiteral{Token: tok, Members: members}
				return ret
			}
			members = append(members, p.parseExpression(LOWEST))
			oldToken = p.curToken
			p.nextToken()
		default:
			oldToken.Pos.Col = oldToken.Pos.Col + len(oldToken.Literal)
			msg := fmt.Sprintf("Syntax Error:%v- expected token to be ',' or ')', got %s instead", oldToken.Pos, p.curToken.Type)
			p.errors = append(p.errors, msg)
			p.errorLines = append(p.errorLines, oldToken.Pos.Sline())
			return nil
		}
	}
}

func (p *Parser) parseFunctionLiteral() ast.Expression {
	lit := &ast.FunctionLiteral{Token: p.curToken}

	if p.peekTokenIs(token.TOKEN_IDENTIFIER) {
		p.nextToken()
		lit.Name = p.curToken.Literal
	}

	if !p.expectPeek(token.TOKEN_LPAREN) {
		return nil
	}
	lit.Parameters = p.parseFunctionParameters()
	if !p.expectPeek(token.TOKEN_LBRACE) {
		return nil
	}
	lit.Body = p.parseBlockStatement()
	return lit
}

func (p *Parser) parseFunctionParameters() []*ast.Identifier {
	identifiers := []*ast.Identifier{}
	if p.peekTokenIs(token.TOKEN_RPAREN) {
		p.nextToken()
		return identifiers
	}
	p.nextToken()
	ident := &ast.Identifier{Token: p.curToken, Value: p.curToken.Literal}
	identifiers = append(identifiers, ident)
	for p.peekTokenIs(token.TOKEN_COMMA) {
		p.nextToken()
		p.nextToken()
		ident := &ast.Identifier{Token: p.curToken, Value: p.curToken.Literal}
		identifiers = append(identifiers, ident)
	}
	if !p.expectPeek(token.TOKEN_RPAREN) {
		return nil
	}
	return identifiers
}

func (p *Parser) parseCallExpression(function ast.Expression) ast.Expression {
	exp := &ast.CallExpression{Token: p.curToken, Function: function}
	exp.Arguments = p.parseExpressionList(token.TOKEN_RPAREN)
	return exp
}

/*
func (p *Parser) parseCallArguments() []ast.Expression {
	args := []ast.Expression{}
	if p.peekTokenIs(token.TOKEN_RPAREN) {
		p.nextToken()
		return args
	}
	p.nextToken()
	args = append(args, p.parseExpression(LOWEST))
	for p.peekTokenIs(token.TOKEN_COMMA) {
		p.nextToken()
		p.nextToken()
		args = append(args, p.parseExpression(LOWEST))
	}
	if !p.expectPeek(token.TOKEN_RPAREN) {
		return nil
	}
	return args
}
*/

func (p *Parser) parseIndexExpression(left ast.Expression) ast.Expression {
	exp := &ast.IndexExpression{Token: p.curToken, Left: left}
	p.nextToken()
	exp.Index = p.parseExpression(LOWEST)
	if !p.expectPeek(token.TOKEN_RBRACKET) {
		return nil
	}

	return exp
}

func (p *Parser) parseNilExpression() ast.Expression {
	return &ast.NilLiteral{Token: p.curToken}
}

func (p *Parser) parseIfExpression() ast.Expression {
	ie := &ast.IfExpression{Token: p.curToken}
	// parse if/else-if expressions
	ie.Conditions = p.parseConditionalExpressions(ie)
	return ie
}

func (p *Parser) parseConditionalExpressions(ie *ast.IfExpression) []*ast.IfConditionExpr {
	// if part
	ic := []*ast.IfConditionExpr{p.parseConditionalExpression()}

	//else-if
	for p.peekTokenIs(token.TOKEN_ELSE) {
		p.nextToken()

		if !p.peekTokenIs(token.TOKEN_IF) {
			if p.peekTokenIs(token.TOKEN_LBRACE) { //block statement. e.g. 'else {'
				p.nextToken()
				ie.Alternative = p.parseBlockStatement()
			} else {
				msg := fmt.Sprintf("Syntax Error:%v- 'else' part must be followed by a '{'.", p.curToken.Pos)
				p.errors = append(p.errors, msg)
				p.errorLines = append(p.errorLines, p.curToken.Pos.Sline())
				return nil
			}
			break
		} else { //'else if'
			p.nextToken()
			ic = append(ic, p.parseConditionalExpression())
		}
	}

	return ic
}

func (p *Parser) parseConditionalExpression() *ast.IfConditionExpr {
	ic := &ast.IfConditionExpr{Token: p.curToken}
	p.nextToken()

	ic.Cond = p.parseExpressionStatement().Expression

	if !p.peekTokenIs(token.TOKEN_LBRACE) {
		msg := fmt.Sprintf("Syntax Error:%v- 'if' expression must be followed by a '{'.", p.curToken.Pos)
		p.errors = append(p.errors, msg)
		p.errorLines = append(p.errorLines, p.curToken.Pos.Sline())
		return nil
	} else {
		p.nextToken()
		ic.Body = p.parseBlockStatement()
	}

	return ic
}

func (p *Parser) parseMethodCallExpression(obj ast.Expression) ast.Expression {
	methodCall := &ast.MethodCallExpression{Token: p.curToken, Object: obj}
	p.nextToken()

	name := p.parseIdentifier()
	if !p.peekTokenIs(token.TOKEN_LPAREN) {
		//methodCall.Call = p.parseExpression(LOWEST)
		//Note: here the precedence should not be `LOWEST`, or else when parsing below line:
		//     logger.LDATE + 1 ==> logger.(LDATE + 1)
		methodCall.Call = p.parseExpression(CALL)
	} else {
		p.nextToken()
		methodCall.Call = p.parseCallExpression(name)
	}

	return methodCall
}

func (p *Parser) parsePostfixExpression(left ast.Expression) ast.Expression {
	return &ast.PostfixExpression{Token: p.curToken, Left: left, Operator: p.curToken.Literal}
}

func (p *Parser) parseDoLoopExpression() ast.Expression {
	p.loopDepth++
	loop := &ast.DoLoop{Token: p.curToken}

	p.expectPeek(token.TOKEN_LBRACE)
	loop.Block = p.parseBlockStatement()

	p.loopDepth--
	return loop
}

func (p *Parser) parseWhileLoopExpression() ast.Expression {
	p.loopDepth++
	loop := &ast.WhileLoop{Token: p.curToken}

	p.nextToken()
	loop.Condition = p.parseExpressionStatement().Expression

	if p.peekTokenIs(token.TOKEN_RPAREN) {
		p.nextToken()
	}

	if p.peekTokenIs(token.TOKEN_LBRACE) {
		p.nextToken()
		loop.Block = p.parseBlockStatement()
	} else {
		msg := fmt.Sprintf("Syntax Error:%v- for loop must be followed by a '{'", p.curToken.Pos)
		p.errors = append(p.errors, msg)
		p.errorLines = append(p.errorLines, p.curToken.Pos.Sline())
		return nil
	}

	p.loopDepth--
	return loop
}

func (p *Parser) parseForLoopExpression() ast.Expression {
	p.loopDepth++
	curToken := p.curToken //save current token

	var r ast.Expression
	if p.peekTokenIs(token.TOKEN_LBRACE) { //for { block }
		r = p.parseForEverLoopExpression(curToken)
		p.loopDepth--
		return r
	}

	if p.peekTokenIs(token.TOKEN_LPAREN) { //for (init; cond; updater) { block }
		r = p.parseCForLoopExpression(curToken)
		p.loopDepth--
		return r
	}

	p.nextToken()                  //skip 'for'
	if p.curToken.Literal == "_" { //for _, value in xxx { block }
		r = p.parseForEachMapExpression(curToken, p.curToken.Literal)
	} else if p.curTokenIs(token.TOKEN_IDENTIFIER) {
		if p.peekTokenIs(token.TOKEN_COMMA) {
			r = p.parseForEachMapExpression(curToken, p.curToken.Literal)
		} else {
			r = p.parseForEachArrayExpression(curToken, p.curToken.Literal)
		}
	} else {
		msg := fmt.Sprintf("Syntax Error:%v- for loop must be followed by an underscore or identifier. got %s", p.curToken.Pos, p.curToken.Literal)
		p.errors = append(p.errors, msg)
		p.errorLines = append(p.errorLines, p.curToken.Pos.Sline())
		return nil
	}

	p.loopDepth--
	return r
}

//for (init; condition; update) {}
//for (; condition; update) {}  --- init is empty
//for (; condition;;) {}  --- init & update both empty
// for (;;;) {} --- init/condition/update all empty
func (p *Parser) parseCForLoopExpression(curToken token.Token) ast.Expression {
	var result ast.Expression

	if !p.expectPeek(token.TOKEN_LPAREN) {
		return nil
	}

	var init ast.Expression
	var cond ast.Expression
	var update ast.Expression

	p.nextToken()
	if !p.curTokenIs(token.TOKEN_SEMICOLON) {
		init = p.parseExpression(LOWEST)
		p.nextToken()
	}

	p.nextToken() //skip ';'
	if !p.curTokenIs(token.TOKEN_SEMICOLON) {
		cond = p.parseExpression(LOWEST)
		p.nextToken()
	}

	p.nextToken()
	if !p.curTokenIs(token.TOKEN_SEMICOLON) {
		update = p.parseExpression(LOWEST)
	}

	if !p.expectPeek(token.TOKEN_RPAREN) {
		return nil
	}

	if !p.peekTokenIs(token.TOKEN_LBRACE) {
		msg := fmt.Sprintf("Syntax Error:%v- for loop must be followed by a '{'.", p.curToken.Pos)
		p.errors = append(p.errors, msg)
		p.errorLines = append(p.errorLines, p.curToken.Pos.Sline())
		return nil
	}

	p.nextToken()

	if init == nil && cond == nil && update == nil {
		loop := &ast.ForEverLoop{Token: curToken}
		loop.Block = p.parseBlockStatement()
		result = loop
	} else {
		loop := &ast.CForLoop{Token: curToken, Init: init, Cond: cond, Update: update}
		loop.Block = p.parseBlockStatement()
		result = loop
	}

	return result
}

//for item in array {}
func (p *Parser) parseForEachArrayExpression(curToken token.Token, variable string) ast.Expression {
	if !p.expectPeek(token.TOKEN_IN) {
		return nil
	}
	p.nextToken()

	value := p.parseExpression(LOWEST)

	var block *ast.BlockStatement
	if p.peekTokenIs(token.TOKEN_LBRACE) {
		p.nextToken()
		block = p.parseBlockStatement()
	} else {
		msg := fmt.Sprintf("Syntax Error:%v- for loop must be followed by a '{' ", p.curToken.Pos)
		p.errors = append(p.errors, msg)
		p.errorLines = append(p.errorLines, p.curToken.Pos.Sline())
		return nil
	}

	result := &ast.ForEachArrayLoop{Token: curToken, Var: variable, Value: value, Block: block}
	return result
}

//for key, value in hash {}
//key & value could be '_' but not both
func (p *Parser) parseForEachMapExpression(curToken token.Token, key string) ast.Expression {
	loop := &ast.ForEachMapLoop{Token: curToken}
	loop.Key = key

	if !p.expectPeek(token.TOKEN_COMMA) {
		return nil
	}

	p.nextToken() //skip ','
	if p.curToken.Literal == "_" {
		//do nothing
	} else if !p.curTokenIs(token.TOKEN_IDENTIFIER) {
		msg := fmt.Sprintf("Syntax Error:%v- for loop must be followed by an identifier. got %s", p.curToken.Pos, p.curToken.Literal)
		p.errors = append(p.errors, msg)
		p.errorLines = append(p.errorLines, p.curToken.Pos.Sline())
		return nil
	}
	loop.Value = p.curToken.Literal

	if loop.Key == "_" && loop.Value == "_" { //for _, _ in xxx { block }
		msg := fmt.Sprintf("Syntax Error:%v- foreach map's key & map are both '_'", p.curToken.Pos)
		p.errors = append(p.errors, msg)
		p.errorLines = append(p.errorLines, p.curToken.Pos.Sline())
		return nil
	}

	if !p.expectPeek(token.TOKEN_IN) {
		return nil
	}

	p.nextToken()
	loop.X = p.parseExpression(LOWEST)

	if p.peekTokenIs(token.TOKEN_LBRACE) {
		p.nextToken()
		loop.Block = p.parseBlockStatement()
	} else {
		msg := fmt.Sprintf("Syntax Error:%v- for loop must be followed by a '{'.", p.curToken.Pos)
		p.errors = append(p.errors, msg)
		p.errorLines = append(p.errorLines, p.curToken.Pos.Sline())
		return nil
	}

	return loop
}

//Almost same with parseDoLoopExpression()
func (p *Parser) parseForEverLoopExpression(curToken token.Token) ast.Expression {
	loop := &ast.ForEverLoop{Token: curToken}

	p.expectPeek(token.TOKEN_LBRACE)
	loop.Block = p.parseBlockStatement()

	return loop
}

func (p *Parser) parseBreakExpression() ast.Expression {
	if p.loopDepth == 0 {
		msg := fmt.Sprintf("Syntax Error:%v- 'break' outside of loop context", p.curToken.Pos)
		p.errors = append(p.errors, msg)
		p.errorLines = append(p.errorLines, p.curToken.Pos.Sline())

		return nil
	}

	return &ast.BreakExpression{Token: p.curToken}

}

func (p *Parser) parseContinueExpression() ast.Expression {
	if p.loopDepth == 0 {
		msg := fmt.Sprintf("Syntax Error:%v- 'continue' outside of loop context", p.curToken.Pos)
		p.errors = append(p.errors, msg)
		p.errorLines = append(p.errorLines, p.curToken.Pos.Sline())

		return nil
	}

	return &ast.ContinueExpression{Token: p.curToken}
}

func (p *Parser) parseStructStatement() ast.Statement {
	st := &ast.StructStatement{
		Token: p.curToken,
	}

	p.nextToken()
	st.Name = p.curToken.Literal

	if !p.expectPeek(token.TOKEN_LBRACE) {
		return nil
	}

	st.Block = p.parseBlockStatement()
	st.RBraceToken = p.curToken

	return st
}

func (p *Parser) parseSwitchExpression() ast.Expression {
	p.fallthroughDepth++
	switchExpr := &ast.SwitchExpression{Token: p.curToken}

	p.nextToken() //skip 'switch'
	switchExpr.Expr = p.parseExpression(LOWEST)
	if switchExpr.Expr == nil {
		return nil
	}

	if !p.expectPeek(token.TOKEN_LBRACE) {
		return nil
	}
	p.nextToken()

	default_cnt := 0
	var defaultToken token.Token

	for !p.curTokenIs(token.TOKEN_RBRACE) {
		if p.curTokenIs(token.TOKEN_EOF) {
			msg := fmt.Sprintf("Syntax Error:%v- unterminated switch statement", p.curToken.Pos)
			p.errors = append(p.errors, msg)
			p.errorLines = append(p.errorLines, p.curToken.Pos.Sline())
			return nil
		}

		if !p.curTokenIs(token.TOKEN_CASE) && !p.curTokenIs(token.TOKEN_DEFAULT) {
			msg := fmt.Sprintf("Syntax Error:%v- expected 'case' or 'default'. got %s instead", p.curToken.Pos, p.curToken.Type)
			p.errors = append(p.errors, msg)
			p.errorLines = append(p.errorLines, p.curToken.Pos.Sline())
			return nil
		}

		caseExpr := &ast.CaseExpression{Token: p.curToken}
		if p.curTokenIs(token.TOKEN_CASE) {
			p.nextToken() //skip 'case'

			caseExpr.Exprs = append(caseExpr.Exprs, p.parseExpression(LOWEST))
			for p.peekTokenIs(token.TOKEN_COMMA) {
				p.nextToken() //skip current token
				p.nextToken() //skip comma
				caseExpr.Exprs = append(caseExpr.Exprs, p.parseExpression(LOWEST))
			}
		} else if p.curTokenIs(token.TOKEN_DEFAULT) {
			default_cnt++
			if default_cnt > 1 {
				defaultToken = p.curToken //remember the second default token for error report use.
			}
			caseExpr.Default = true
		}

		//are there more than one default?
		if default_cnt > 1 {
			msg := fmt.Sprintf("Syntax Error:%v- more than one default are not allowed", defaultToken.Pos)
			p.errors = append(p.errors, msg)
			p.errorLines = append(p.errorLines, switchExpr.Token.Pos.Sline())
			return nil
		}

		if !p.expectPeek(token.TOKEN_LBRACE) {
			return nil
		}

		caseExpr.Block = p.parseBlockStatement()
		if !p.curTokenIs(token.TOKEN_RBRACE) {
			msg := fmt.Sprintf("Syntax Error:%v- expected token to be '}', got %s instead", p.curToken.Pos, p.curToken.Type)
			p.errors = append(p.errors, msg)
			p.errorLines = append(p.errorLines, p.curToken.Pos.Sline())
			return nil

		}
		caseExpr.RBraceToken = p.curToken

		p.nextToken() //skip '}'
		switchExpr.Cases = append(switchExpr.Cases, caseExpr)
	}
	switchExpr.RBraceToken = p.curToken

	//check 'fallthrough' position
	for i, cse := range switchExpr.Cases {
		lastCase := i == len(switchExpr.Cases)-1
		for j, stmt := range cse.Block.Statements {
			lastStmt := j == len(cse.Block.Statements)-1
			switch stmt := stmt.(type) {
			case *ast.ExpressionStatement:
				if _, ok := stmt.Expression.(*ast.FallthroughExpression); !ok {
					continue
				}

				if !lastStmt {
					msg := fmt.Sprintf("Syntax Error:%v- fallthrough can be used only as a last statement inside case clause", stmt.Pos().Line)
					p.errors = append(p.errors, msg)
					p.errorLines = append(p.errorLines, p.curToken.Pos.Sline())
					return nil
				}
				if lastCase {
					msg := fmt.Sprintf("Syntax Error:%v- cannot fallthrough final case in switch", stmt.Pos().Line)
					p.errors = append(p.errors, msg)
					p.errorLines = append(p.errorLines, stmt.Pos().Sline())
					return nil
				}
			}
		}
	}

	p.fallthroughDepth--

	switchExpr.RBraceToken = p.curToken
	return switchExpr
}

func (p *Parser) parseFallThroughExpression() ast.Expression {
	if p.fallthroughDepth == 0 {
		msg := fmt.Sprintf("Syntax Error:%v- 'fallthrough' outside of switch context", p.curToken.Pos)
		p.errors = append(p.errors, msg)
		p.errorLines = append(p.errorLines, p.curToken.Pos.Sline())

		return nil
	}

	return &ast.FallthroughExpression{Token: p.curToken}

}

func (p *Parser) noPrefixParseFnError(t token.TokenType) {
	if t != token.TOKEN_EOF {
		msg := fmt.Sprintf("Syntax Error:%v- no prefix parse functions for '%s' found", p.curToken.Pos, t)
		p.errors = append(p.errors, msg)
		p.errorLines = append(p.errorLines, p.curToken.Pos.Sline())
	}
}

func (p *Parser) curTokenIs(t token.TokenType) bool {
	return p.curToken.Type == t
}

func (p *Parser) peekTokenIs(t token.TokenType) bool {
	return p.peekToken.Type == t
}

func (p *Parser) peekPrecedence() int {
	if p, ok := precedences[p.peekToken.Type]; ok {
		return p
	}
	return LOWEST
}

func (p *Parser) curPrecedence() int {
	if p, ok := precedences[p.curToken.Type]; ok {
		return p
	}
	return LOWEST
}

func (p *Parser) nextToken() {
	p.curToken = p.peekToken
	p.peekToken = p.l.NextToken()
}

func (p *Parser) expectPeek(t token.TokenType) bool {
	if p.peekTokenIs(t) {
		p.nextToken()
		return true
	}
	p.peekError(t)
	return false
}

func (p *Parser) peekError(t token.TokenType) {
	newPos := p.curToken.Pos
	newPos.Col = newPos.Col + utf8.RuneCountInString(p.curToken.Literal)

	msg := fmt.Sprintf("Syntax Error:%v- expected next token to be %s, got %s instead", newPos, t, p.peekToken.Type)
	p.errors = append(p.errors, msg)
	p.errorLines = append(p.errorLines, p.curToken.Pos.Sline())
}

func (p *Parser) Errors() []string {
	return p.errors
}

func (p *Parser) ErrorLines() []string {
	return p.errorLines
}

//DEBUG ONLY
func (p *Parser) debugToken(message string) {
	fmt.Printf("%s, curToken = %s, curToken.Pos = %d, peekToken = %s, peekToken.Pos=%d\n", message, p.curToken.Literal, p.curToken.Pos.Line, p.peekToken.Literal, p.peekToken.Pos.Line)
}

func (p *Parser) debugNode(message string, node ast.Node) {
	fmt.Printf("%s, Node = %s\n", message, node.String())
}
