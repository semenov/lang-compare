package main

import (
	"fmt"
	"os"
	"strconv"
	"unsafe"
)

const (
	OpConstant byte = iota
	OpNil
	OpTrue
	OpFalse
	OpPop
	OpGetLocal
	OpSetLocal
	OpGetGlobal
	OpDefineGlobal
	OpSetGlobal
	OpGetUpvalue
	OpSetUpvalue
	OpGetProperty
	OpSetProperty
	OpGetSuper
	OpEqual
	OpNotEqual
	OpGreater
	OpGreaterEqual
	OpLess
	OpLessEqual
	OpAdd
	OpSubtract
	OpMultiply
	OpDivide
	OpNot
	OpNegate
	OpPrint
	OpJump
	OpJumpIfFalse
	OpLoop
	OpCall
	OpInvoke
	OpSuperInvoke
	OpClosure
	OpCloseUpvalue
	OpReturn
	OpClass
	OpInherit
	OpMethod
	// fused instructions
	OpJumpIfFalsePop
	OpSetLocalPop
	OpSetGlobalPop
	OpSetPropertyPop
	OpGetLocalProperty
	OpEqualJump
	OpNotEqualJump
	OpGreaterJump
	OpGreaterEqualJump
	OpLessJump
	OpLessEqualJump
	// constant right operand: op k
	OpAddC
	OpSubC
	OpGreaterC
	OpGreaterEqualC
	OpLessC
	OpLessEqualC
	// constant right operand + conditional jump: op k off16
	OpGreaterCJump
	OpGreaterEqualCJump
	OpLessCJump
	OpLessEqualCJump
)

type Precedence int

const (
	PrecNone Precedence = iota
	PrecAssignment
	PrecOr
	PrecAnd
	PrecEquality
	PrecComparison
	PrecTerm
	PrecFactor
	PrecUnary
	PrecCall
	PrecPrimary
)

type FunctionType int

const (
	TypeFunction FunctionType = iota
	TypeInitializer
	TypeMethod
	TypeScript
)

type Local struct {
	name       string
	depth      int
	isCaptured bool
}

type UpvalueRef struct {
	index   uint8
	isLocal bool
}

type Compiler struct {
	enclosing  *Compiler
	function   *ObjFunction
	ftype      FunctionType
	locals     []Local
	upvalues   []UpvalueRef
	scopeDepth int
	lastOp     int // position of the last emitted opcode
	jumpTarget int // position most recently targeted by a forward jump
}

type ClassCompiler struct {
	enclosing     *ClassCompiler
	hasSuperclass bool
}

type Parser struct {
	scanner   *Scanner
	current   Token
	previous  Token
	hadError  bool
	panicMode bool
	compiler  *Compiler
	class     *ClassCompiler
	vm        *VM
}

type parseFn func(p *Parser, canAssign bool)

type ParseRule struct {
	prefix parseFn
	infix  parseFn
	prec   Precedence
}

var rules [tokenCount]ParseRule

func init() {
	rules[TLeftParen] = ParseRule{(*Parser).grouping, (*Parser).call, PrecCall}
	rules[TDot] = ParseRule{nil, (*Parser).dot, PrecCall}
	rules[TMinus] = ParseRule{(*Parser).unary, (*Parser).binary, PrecTerm}
	rules[TPlus] = ParseRule{nil, (*Parser).binary, PrecTerm}
	rules[TSlash] = ParseRule{nil, (*Parser).binary, PrecFactor}
	rules[TStar] = ParseRule{nil, (*Parser).binary, PrecFactor}
	rules[TBang] = ParseRule{(*Parser).unary, nil, PrecNone}
	rules[TBangEqual] = ParseRule{nil, (*Parser).binary, PrecEquality}
	rules[TEqualEqual] = ParseRule{nil, (*Parser).binary, PrecEquality}
	rules[TGreater] = ParseRule{nil, (*Parser).binary, PrecComparison}
	rules[TGreaterEqual] = ParseRule{nil, (*Parser).binary, PrecComparison}
	rules[TLess] = ParseRule{nil, (*Parser).binary, PrecComparison}
	rules[TLessEqual] = ParseRule{nil, (*Parser).binary, PrecComparison}
	rules[TIdentifier] = ParseRule{(*Parser).variable, nil, PrecNone}
	rules[TString] = ParseRule{(*Parser).str, nil, PrecNone}
	rules[TNumber] = ParseRule{(*Parser).number, nil, PrecNone}
	rules[TAnd] = ParseRule{nil, (*Parser).and, PrecAnd}
	rules[TOr] = ParseRule{nil, (*Parser).or, PrecOr}
	rules[TFalse] = ParseRule{(*Parser).literal, nil, PrecNone}
	rules[TNil] = ParseRule{(*Parser).literal, nil, PrecNone}
	rules[TTrue] = ParseRule{(*Parser).literal, nil, PrecNone}
	rules[TSuper] = ParseRule{(*Parser).super, nil, PrecNone}
	rules[TThis] = ParseRule{(*Parser).this, nil, PrecNone}
}

func compile(vm *VM, src string) *ObjFunction {
	p := &Parser{scanner: newScanner(src), vm: vm}
	p.initCompiler(&Compiler{}, TypeScript)
	p.advance()
	for !p.match(TEOF) {
		p.declaration()
	}
	fn := p.endCompiler()
	if p.hadError {
		return nil
	}
	return fn
}

// ---- error handling ----

func (p *Parser) errorAt(tok Token, msg string) {
	if p.panicMode {
		return
	}
	p.panicMode = true
	fmt.Fprintf(os.Stderr, "[line %d] Error", tok.line)
	switch tok.typ {
	case TEOF:
		fmt.Fprint(os.Stderr, " at end")
	case TError:
	default:
		fmt.Fprintf(os.Stderr, " at '%s'", tok.lexeme)
	}
	fmt.Fprintf(os.Stderr, ": %s\n", msg)
	p.hadError = true
}

func (p *Parser) error(msg string)          { p.errorAt(p.previous, msg) }
func (p *Parser) errorAtCurrent(msg string) { p.errorAt(p.current, msg) }

func (p *Parser) advance() {
	p.previous = p.current
	for {
		p.current = p.scanner.scanToken()
		if p.current.typ != TError {
			break
		}
		p.errorAtCurrent(p.current.lexeme)
	}
}

func (p *Parser) consume(t TokenType, msg string) {
	if p.current.typ == t {
		p.advance()
		return
	}
	p.errorAtCurrent(msg)
}

func (p *Parser) check(t TokenType) bool { return p.current.typ == t }

func (p *Parser) match(t TokenType) bool {
	if !p.check(t) {
		return false
	}
	p.advance()
	return true
}

// ---- emission ----

func (p *Parser) chunk() *Chunk { return &p.compiler.function.chunk }

func (p *Parser) emitByte(b byte) {
	c := p.chunk()
	c.code = append(c.code, b)
	c.lines = append(c.lines, int32(p.previous.line))
}

func (p *Parser) emitOp(op byte) {
	p.compiler.lastOp = len(p.chunk().code)
	p.emitByte(op)
}

func (p *Parser) emitBytes(op, b byte) {
	p.emitOp(op)
	p.emitByte(b)
}

// fusable reports whether the last emitted instruction is op and can be
// rewritten in place (no jump lands right after it).
func (p *Parser) fusable(op byte) bool {
	c := p.compiler
	code := p.chunk().code
	return c.lastOp >= 0 && code[c.lastOp] == op && c.jumpTarget != len(code)
}

// emitPop emits a POP, fusing it with a preceding assignment when possible.
func (p *Parser) emitPop() {
	code := p.chunk().code
	switch {
	case p.fusable(OpSetLocal):
		code[p.compiler.lastOp] = OpSetLocalPop
	case p.fusable(OpSetGlobal):
		code[p.compiler.lastOp] = OpSetGlobalPop
	case p.fusable(OpSetProperty):
		code[p.compiler.lastOp] = OpSetPropertyPop
	default:
		p.emitOp(OpPop)
		return
	}
	p.compiler.lastOp = -1
}

var constOps = map[byte]byte{
	OpAdd: OpAddC, OpSubtract: OpSubC,
	OpGreater: OpGreaterC, OpGreaterEqual: OpGreaterEqualC,
	OpLess: OpLessC, OpLessEqual: OpLessEqualC,
}

// emitBinary emits a binary operator, fusing a numeric constant right operand.
func (p *Parser) emitBinary(op byte) {
	c := p.compiler
	code := p.chunk().code
	if fop, ok := constOps[op]; ok && p.fusable(OpConstant) && c.lastOp == len(code)-2 {
		if p.chunk().consts[code[c.lastOp+1]].isNum() {
			code[c.lastOp] = fop
			return
		}
	}
	p.emitOp(op)
}

var compareJumps = map[byte]byte{
	OpGreaterC: OpGreaterCJump, OpGreaterEqualC: OpGreaterEqualCJump,
	OpLessC: OpLessCJump, OpLessEqualC: OpLessEqualCJump,
	OpEqual: OpEqualJump, OpNotEqual: OpNotEqualJump,
	OpGreater: OpGreaterJump, OpGreaterEqual: OpGreaterEqualJump,
	OpLess: OpLessJump, OpLessEqual: OpLessEqualJump,
}

// emitCondJump emits a jump taken when the condition is falsey, popping the
// condition in both cases.
func (p *Parser) emitCondJump() int {
	c := p.compiler
	code := p.chunk().code
	if c.lastOp >= 0 && c.lastOp >= len(code)-2 && c.jumpTarget != len(code) {
		if fused, ok := compareJumps[code[c.lastOp]]; ok {
			code[c.lastOp] = fused
			p.emitByte(0xff)
			p.emitByte(0xff)
			return len(p.chunk().code) - 2
		}
	}
	return p.emitJump(OpJumpIfFalsePop)
}

func (p *Parser) emitShort(v int) {
	p.emitByte(byte(v >> 8))
	p.emitByte(byte(v))
}

func (p *Parser) emitLoop(loopStart int) {
	p.emitOp(OpLoop)
	offset := len(p.chunk().code) - loopStart + 2
	if offset > 0xffff {
		p.error("Loop body too large.")
	}
	p.emitShort(offset)
}

func (p *Parser) emitJump(op byte) int {
	p.emitOp(op)
	p.emitByte(0xff)
	p.emitByte(0xff)
	return len(p.chunk().code) - 2
}

func (p *Parser) patchJump(offset int) {
	c := p.chunk()
	jump := len(c.code) - offset - 2
	if jump > 0xffff {
		p.error("Too much code to jump over.")
	}
	c.code[offset] = byte(jump >> 8)
	c.code[offset+1] = byte(jump)
	p.compiler.jumpTarget = len(c.code)
}

func (p *Parser) emitReturn() {
	if p.compiler.ftype == TypeInitializer {
		p.emitBytes(OpGetLocal, 0)
	} else {
		p.emitOp(OpNil)
	}
	p.emitOp(OpReturn)
}

func (p *Parser) makeConstant(v Value) byte {
	c := p.chunk()
	if len(c.consts) >= 256 {
		p.error("Too many constants in one chunk.")
		return 0
	}
	c.consts = append(c.consts, v)
	return byte(len(c.consts) - 1)
}

func (p *Parser) emitConstant(v Value) {
	p.emitBytes(OpConstant, p.makeConstant(v))
}

func (p *Parser) newCache() int {
	f := p.compiler.function
	f.caches = append(f.caches, PropCache{})
	return len(f.caches) - 1
}

func (p *Parser) initCompiler(c *Compiler, t FunctionType) {
	c.enclosing = p.compiler
	c.ftype = t
	c.function = &ObjFunction{Obj: Obj{kFunction}}
	c.lastOp, c.jumpTarget = -1, -1
	c.locals = make([]Local, 0, 8)
	p.compiler = c
	if t != TypeScript {
		c.function.name = p.vm.intern(p.previous.lexeme)
	}
	name := ""
	if t != TypeFunction {
		name = "this"
	}
	c.locals = append(c.locals, Local{name: name, depth: 0})
}

func (p *Parser) endCompiler() *ObjFunction {
	p.emitReturn()
	fn := p.compiler.function
	fn.upvalueCount = len(p.compiler.upvalues)
	fn.codep = unsafe.Pointer(unsafe.SliceData(fn.chunk.code))
	fn.constp = unsafe.Pointer(unsafe.SliceData(fn.chunk.consts))
	fn.cachep = unsafe.Pointer(unsafe.SliceData(fn.caches))
	p.compiler = p.compiler.enclosing
	return fn
}

func (p *Parser) beginScope() { p.compiler.scopeDepth++ }

func (p *Parser) endScope() {
	c := p.compiler
	c.scopeDepth--
	for len(c.locals) > 0 && c.locals[len(c.locals)-1].depth > c.scopeDepth {
		if c.locals[len(c.locals)-1].isCaptured {
			p.emitOp(OpCloseUpvalue)
		} else {
			p.emitOp(OpPop)
		}
		c.locals = c.locals[:len(c.locals)-1]
	}
}

// ---- variables ----

func (p *Parser) identifierConstant(name string) byte {
	return p.makeConstant(objV(p.vm.intern(name)))
}

func resolveLocal(p *Parser, c *Compiler, name string) int {
	for i := len(c.locals) - 1; i >= 0; i-- {
		if c.locals[i].name == name {
			if c.locals[i].depth == -1 {
				p.error("Can't read local variable in its own initializer.")
			}
			return i
		}
	}
	return -1
}

func addUpvalue(p *Parser, c *Compiler, index uint8, isLocal bool) int {
	for i, u := range c.upvalues {
		if u.index == index && u.isLocal == isLocal {
			return i
		}
	}
	if len(c.upvalues) == 256 {
		p.error("Too many closure variables in function.")
		return 0
	}
	c.upvalues = append(c.upvalues, UpvalueRef{index, isLocal})
	return len(c.upvalues) - 1
}

func resolveUpvalue(p *Parser, c *Compiler, name string) int {
	if c.enclosing == nil {
		return -1
	}
	if local := resolveLocal(p, c.enclosing, name); local != -1 {
		c.enclosing.locals[local].isCaptured = true
		return addUpvalue(p, c, uint8(local), true)
	}
	if up := resolveUpvalue(p, c.enclosing, name); up != -1 {
		return addUpvalue(p, c, uint8(up), false)
	}
	return -1
}

func (p *Parser) addLocal(name string) {
	c := p.compiler
	if len(c.locals) == 256 {
		p.error("Too many local variables in function.")
		return
	}
	c.locals = append(c.locals, Local{name: name, depth: -1})
}

func (p *Parser) declareVariable() {
	c := p.compiler
	if c.scopeDepth == 0 {
		return
	}
	name := p.previous.lexeme
	for i := len(c.locals) - 1; i >= 0; i-- {
		l := c.locals[i]
		if l.depth != -1 && l.depth < c.scopeDepth {
			break
		}
		if l.name == name {
			p.error("Already a variable with this name in this scope.")
		}
	}
	p.addLocal(name)
}

// parseVariable returns the global slot index for globals (or 0 for locals).
func (p *Parser) parseVariable(msg string) int {
	p.consume(TIdentifier, msg)
	p.declareVariable()
	if p.compiler.scopeDepth > 0 {
		return 0
	}
	p.identifierConstant(p.previous.lexeme)
	return p.vm.globalSlot(p.previous.lexeme)
}

func (p *Parser) markInitialized() {
	c := p.compiler
	if c.scopeDepth == 0 {
		return
	}
	c.locals[len(c.locals)-1].depth = c.scopeDepth
}

func (p *Parser) defineVariable(global int) {
	if p.compiler.scopeDepth > 0 {
		p.markInitialized()
		return
	}
	p.emitOp(OpDefineGlobal)
	p.emitShort(global)
}

func (p *Parser) namedVariable(name string, canAssign bool) {
	var getOp, setOp byte
	arg := resolveLocal(p, p.compiler, name)
	wide := false
	if arg != -1 {
		getOp, setOp = OpGetLocal, OpSetLocal
	} else if arg = resolveUpvalue(p, p.compiler, name); arg != -1 {
		getOp, setOp = OpGetUpvalue, OpSetUpvalue
	} else {
		p.identifierConstant(name)
		arg = p.vm.globalSlot(name)
		getOp, setOp = OpGetGlobal, OpSetGlobal
		wide = true
	}
	op := getOp
	if canAssign && p.match(TEqual) {
		p.expression()
		op = setOp
	}
	p.emitOp(op)
	if wide {
		p.emitShort(arg)
	} else {
		p.emitByte(byte(arg))
	}
}

// ---- expressions ----

func (p *Parser) expression() { p.parsePrecedence(PrecAssignment) }

func (p *Parser) parsePrecedence(prec Precedence) {
	p.advance()
	prefix := rules[p.previous.typ].prefix
	if prefix == nil {
		p.error("Expect expression.")
		return
	}
	canAssign := prec <= PrecAssignment
	prefix(p, canAssign)
	for prec <= rules[p.current.typ].prec {
		p.advance()
		rules[p.previous.typ].infix(p, canAssign)
	}
	if canAssign && p.match(TEqual) {
		p.error("Invalid assignment target.")
	}
}

func (p *Parser) grouping(bool) {
	p.expression()
	p.consume(TRightParen, "Expect ')' after expression.")
}

func (p *Parser) number(bool) {
	f, _ := strconv.ParseFloat(p.previous.lexeme, 64)
	p.emitConstant(numV(f))
}

func (p *Parser) str(bool) {
	l := p.previous.lexeme
	p.emitConstant(objV(p.vm.intern(l[1 : len(l)-1])))
}

func (p *Parser) literal(bool) {
	switch p.previous.typ {
	case TFalse:
		p.emitOp(OpFalse)
	case TTrue:
		p.emitOp(OpTrue)
	case TNil:
		p.emitOp(OpNil)
	}
}

func (p *Parser) variable(canAssign bool) { p.namedVariable(p.previous.lexeme, canAssign) }

func (p *Parser) unary(bool) {
	op := p.previous.typ
	p.parsePrecedence(PrecUnary)
	if op == TBang {
		p.emitOp(OpNot)
	} else {
		p.emitOp(OpNegate)
	}
}

func (p *Parser) binary(bool) {
	op := p.previous.typ
	p.parsePrecedence(rules[op].prec + 1)
	switch op {
	case TBangEqual:
		p.emitBinary(OpNotEqual)
	case TEqualEqual:
		p.emitBinary(OpEqual)
	case TGreater:
		p.emitBinary(OpGreater)
	case TGreaterEqual:
		p.emitBinary(OpGreaterEqual)
	case TLess:
		p.emitBinary(OpLess)
	case TLessEqual:
		p.emitBinary(OpLessEqual)
	case TPlus:
		p.emitBinary(OpAdd)
	case TMinus:
		p.emitBinary(OpSubtract)
	case TStar:
		p.emitBinary(OpMultiply)
	case TSlash:
		p.emitBinary(OpDivide)
	}
}

func (p *Parser) and(bool) {
	endJump := p.emitJump(OpJumpIfFalse)
	p.emitOp(OpPop)
	p.parsePrecedence(PrecAnd)
	p.patchJump(endJump)
}

func (p *Parser) or(bool) {
	elseJump := p.emitJump(OpJumpIfFalse)
	endJump := p.emitJump(OpJump)
	p.patchJump(elseJump)
	p.emitOp(OpPop)
	p.parsePrecedence(PrecOr)
	p.patchJump(endJump)
}

func (p *Parser) argumentList() byte {
	argc := 0
	if !p.check(TRightParen) {
		for {
			p.expression()
			if argc == 255 {
				p.error("Can't have more than 255 arguments.")
			}
			argc++
			if !p.match(TComma) {
				break
			}
		}
	}
	p.consume(TRightParen, "Expect ')' after arguments.")
	return byte(argc)
}

func (p *Parser) call(bool) {
	argc := p.argumentList()
	p.emitBytes(OpCall, argc)
}

func (p *Parser) dot(canAssign bool) {
	p.consume(TIdentifier, "Expect property name after '.'.")
	name := p.identifierConstant(p.previous.lexeme)
	if canAssign && p.match(TEqual) {
		p.expression()
		p.emitBytes(OpSetProperty, name)
		p.emitShort(p.newCache())
	} else if p.match(TLeftParen) {
		argc := p.argumentList()
		p.emitBytes(OpInvoke, name)
		p.emitByte(argc)
		p.emitShort(p.newCache())
	} else if p.fusable(OpGetLocal) && p.compiler.lastOp == len(p.chunk().code)-2 {
		p.chunk().code[p.compiler.lastOp] = OpGetLocalProperty
		p.emitByte(name)
		p.emitShort(p.newCache())
	} else {
		p.emitBytes(OpGetProperty, name)
		p.emitShort(p.newCache())
	}
}

func (p *Parser) this(bool) {
	if p.class == nil {
		p.error("Can't use 'this' outside of a class.")
		return
	}
	p.variable(false)
}

func (p *Parser) super(bool) {
	if p.class == nil {
		p.error("Can't use 'super' outside of a class.")
	} else if !p.class.hasSuperclass {
		p.error("Can't use 'super' in a class with no superclass.")
	}
	p.consume(TDot, "Expect '.' after 'super'.")
	p.consume(TIdentifier, "Expect superclass method name.")
	name := p.identifierConstant(p.previous.lexeme)
	p.namedVariable("this", false)
	if p.match(TLeftParen) {
		argc := p.argumentList()
		p.namedVariable("super", false)
		p.emitBytes(OpSuperInvoke, name)
		p.emitByte(argc)
	} else {
		p.namedVariable("super", false)
		p.emitBytes(OpGetSuper, name)
	}
}

// ---- statements ----

func (p *Parser) block() {
	for !p.check(TRightBrace) && !p.check(TEOF) {
		p.declaration()
	}
	p.consume(TRightBrace, "Expect '}' after block.")
}

func (p *Parser) function(t FunctionType) {
	c := &Compiler{}
	p.initCompiler(c, t)
	p.beginScope()
	p.consume(TLeftParen, "Expect '(' after function name.")
	if !p.check(TRightParen) {
		for {
			c.function.arity++
			if c.function.arity > 255 {
				p.errorAtCurrent("Can't have more than 255 parameters.")
			}
			g := p.parseVariable("Expect parameter name.")
			p.defineVariable(g)
			if !p.match(TComma) {
				break
			}
		}
	}
	p.consume(TRightParen, "Expect ')' after parameters.")
	p.consume(TLeftBrace, "Expect '{' before function body.")
	p.block()
	fn := p.endCompiler()
	p.emitBytes(OpClosure, p.makeConstant(objV(fn)))
	for _, u := range c.upvalues {
		if u.isLocal {
			p.emitByte(1)
		} else {
			p.emitByte(0)
		}
		p.emitByte(u.index)
	}
}

func (p *Parser) method() {
	p.consume(TIdentifier, "Expect method name.")
	name := p.identifierConstant(p.previous.lexeme)
	t := TypeMethod
	if p.previous.lexeme == "init" {
		t = TypeInitializer
	}
	p.function(t)
	p.emitBytes(OpMethod, name)
}

func (p *Parser) classDeclaration() {
	p.consume(TIdentifier, "Expect class name.")
	className := p.previous.lexeme
	nameConst := p.identifierConstant(className)
	p.declareVariable()
	p.emitBytes(OpClass, nameConst)
	global := 0
	if p.compiler.scopeDepth == 0 {
		global = p.vm.globalSlot(className)
	}
	p.defineVariable(global)

	cc := &ClassCompiler{enclosing: p.class}
	p.class = cc

	if p.match(TLess) {
		p.consume(TIdentifier, "Expect superclass name.")
		p.variable(false)
		if className == p.previous.lexeme {
			p.error("A class can't inherit from itself.")
		}
		p.beginScope()
		p.addLocal("super")
		p.defineVariable(0)
		p.namedVariable(className, false)
		p.emitOp(OpInherit)
		cc.hasSuperclass = true
	}

	p.namedVariable(className, false)
	p.consume(TLeftBrace, "Expect '{' before class body.")
	for !p.check(TRightBrace) && !p.check(TEOF) {
		p.method()
	}
	p.consume(TRightBrace, "Expect '}' after class body.")
	p.emitOp(OpPop)
	if cc.hasSuperclass {
		p.endScope()
	}
	p.class = cc.enclosing
}

func (p *Parser) funDeclaration() {
	global := p.parseVariable("Expect function name.")
	p.markInitialized()
	p.function(TypeFunction)
	p.defineVariable(global)
}

func (p *Parser) varDeclaration() {
	global := p.parseVariable("Expect variable name.")
	if p.match(TEqual) {
		p.expression()
	} else {
		p.emitOp(OpNil)
	}
	p.consume(TSemicolon, "Expect ';' after variable declaration.")
	p.defineVariable(global)
}

func (p *Parser) expressionStatement() {
	p.expression()
	p.consume(TSemicolon, "Expect ';' after expression.")
	p.emitPop()
}

func (p *Parser) forStatement() {
	p.beginScope()
	p.consume(TLeftParen, "Expect '(' after 'for'.")
	if p.match(TSemicolon) {
	} else if p.match(TVar) {
		p.varDeclaration()
	} else {
		p.expressionStatement()
	}
	loopStart := len(p.chunk().code)
	exitJump := -1
	if !p.match(TSemicolon) {
		p.expression()
		p.consume(TSemicolon, "Expect ';' after loop condition.")
		exitJump = p.emitCondJump()
	}
	if !p.match(TRightParen) {
		bodyJump := p.emitJump(OpJump)
		incStart := len(p.chunk().code)
		p.expression()
		p.emitPop()
		p.consume(TRightParen, "Expect ')' after for clauses.")
		p.emitLoop(loopStart)
		loopStart = incStart
		p.patchJump(bodyJump)
	}
	p.statement()
	p.emitLoop(loopStart)
	if exitJump != -1 {
		p.patchJump(exitJump)
	}
	p.endScope()
}

func (p *Parser) ifStatement() {
	p.consume(TLeftParen, "Expect '(' after 'if'.")
	p.expression()
	p.consume(TRightParen, "Expect ')' after condition.")
	thenJump := p.emitCondJump()
	p.statement()
	if p.match(TElse) {
		elseJump := p.emitJump(OpJump)
		p.patchJump(thenJump)
		p.statement()
		p.patchJump(elseJump)
	} else {
		p.patchJump(thenJump)
	}
}

func (p *Parser) printStatement() {
	p.expression()
	p.consume(TSemicolon, "Expect ';' after value.")
	p.emitOp(OpPrint)
}

func (p *Parser) returnStatement() {
	if p.compiler.ftype == TypeScript {
		p.error("Can't return from top-level code.")
	}
	if p.match(TSemicolon) {
		p.emitReturn()
	} else {
		if p.compiler.ftype == TypeInitializer {
			p.error("Can't return a value from an initializer.")
		}
		p.expression()
		p.consume(TSemicolon, "Expect ';' after return value.")
		p.emitOp(OpReturn)
	}
}

func (p *Parser) whileStatement() {
	loopStart := len(p.chunk().code)
	p.consume(TLeftParen, "Expect '(' after 'while'.")
	p.expression()
	p.consume(TRightParen, "Expect ')' after condition.")
	exitJump := p.emitCondJump()
	p.statement()
	p.emitLoop(loopStart)
	p.patchJump(exitJump)
}

func (p *Parser) synchronize() {
	p.panicMode = false
	for p.current.typ != TEOF {
		if p.previous.typ == TSemicolon {
			return
		}
		switch p.current.typ {
		case TClass, TFun, TVar, TFor, TIf, TWhile, TPrint, TReturn:
			return
		}
		p.advance()
	}
}

func (p *Parser) declaration() {
	switch {
	case p.match(TClass):
		p.classDeclaration()
	case p.match(TFun):
		p.funDeclaration()
	case p.match(TVar):
		p.varDeclaration()
	default:
		p.statement()
	}
	if p.panicMode {
		p.synchronize()
	}
}

func (p *Parser) statement() {
	switch {
	case p.match(TPrint):
		p.printStatement()
	case p.match(TFor):
		p.forStatement()
	case p.match(TIf):
		p.ifStatement()
	case p.match(TReturn):
		p.returnStatement()
	case p.match(TWhile):
		p.whileStatement()
	case p.match(TLeftBrace):
		p.beginScope()
		p.block()
		p.endScope()
	default:
		p.expressionStatement()
	}
}
