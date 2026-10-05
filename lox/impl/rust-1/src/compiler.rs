use crate::object::{Chunk, ObjFunction, ObjString};
use crate::scanner::{Scanner, Token, TokenType};
use crate::value::Value;
use crate::vm::{Vm, op};

#[derive(Clone, Copy, PartialEq, Eq, PartialOrd, Ord)]
enum Prec {
    None,
    Assignment,
    Or,
    And,
    Equality,
    Comparison,
    Term,
    Factor,
    Unary,
    Call,
    Primary,
}

impl Prec {
    fn next(self) -> Prec {
        use Prec::*;
        match self {
            None => Assignment,
            Assignment => Or,
            Or => And,
            And => Equality,
            Equality => Comparison,
            Comparison => Term,
            Term => Factor,
            Factor => Unary,
            Unary => Call,
            Call => Primary,
            Primary => Primary,
        }
    }
}

fn infix_prec(t: TokenType) -> Prec {
    use TokenType::*;
    match t {
        LeftParen | Dot => Prec::Call,
        Minus | Plus => Prec::Term,
        Slash | Star => Prec::Factor,
        BangEqual | EqualEqual => Prec::Equality,
        Greater | GreaterEqual | Less | LessEqual => Prec::Comparison,
        And => Prec::And,
        Or => Prec::Or,
        _ => Prec::None,
    }
}

#[derive(Clone, Copy, PartialEq, Eq)]
enum FunctionType {
    Function,
    Initializer,
    Method,
    Script,
}

struct Local<'a> {
    name: &'a str,
    depth: i32,
    is_captured: bool,
}

struct FnCompiler<'a> {
    chunk: Chunk,
    arity: usize,
    name: *mut ObjString,
    ty: FunctionType,
    locals: Vec<Local<'a>>,
    upvalues: Vec<(u8, bool)>,
    scope_depth: i32,
    cache_count: usize,
    /// Start offsets of emitted instructions (most recent last).
    op_starts: Vec<usize>,
    /// Bytes of removed dead code; still counted toward jump-size limits so
    /// they match the reference implementation.
    phantom: usize,
    /// Most recent jump target; instructions are never fused across it.
    barrier: usize,
}

/// A code position, plus the dead-code byte count at that point.
#[derive(Clone, Copy)]
struct Mark {
    pos: usize,
    phantom: usize,
}

struct ClassCompiler {
    has_superclass: bool,
}

pub struct Compiler<'a, 'v> {
    vm: &'v mut Vm,
    scanner: Scanner<'a>,
    current: Token<'a>,
    previous: Token<'a>,
    had_error: bool,
    panic_mode: bool,
    fns: Vec<FnCompiler<'a>>,
    classes: Vec<ClassCompiler>,
}

const UINT8_COUNT: usize = 256;

pub fn compile(vm: &mut Vm, source: &str) -> Option<*mut ObjFunction> {
    let mut c = Compiler {
        vm,
        scanner: Scanner::new(source),
        current: Token { ty: TokenType::Eof, lexeme: "", line: 1 },
        previous: Token { ty: TokenType::Eof, lexeme: "", line: 1 },
        had_error: false,
        panic_mode: false,
        fns: Vec::new(),
        classes: Vec::new(),
    };
    c.init_compiler(FunctionType::Script);
    c.advance();
    while !c.matches(TokenType::Eof) {
        c.declaration();
    }
    let f = c.end_compiler();
    if c.had_error { None } else { Some(f) }
}

impl<'a, 'v> Compiler<'a, 'v> {
    // ---- error handling -------------------------------------------------

    fn error_at(&mut self, tok: Token<'a>, msg: &str) {
        if self.panic_mode {
            return;
        }
        self.panic_mode = true;
        let mut s = format!("[line {}] Error", tok.line);
        match tok.ty {
            TokenType::Eof => s.push_str(" at end"),
            TokenType::Error => {}
            _ => {
                s.push_str(" at '");
                s.push_str(tok.lexeme);
                s.push('\'');
            }
        }
        eprintln!("{}: {}", s, msg);
        self.had_error = true;
    }

    fn error(&mut self, msg: &str) {
        self.error_at(self.previous, msg);
    }

    fn error_at_current(&mut self, msg: &str) {
        self.error_at(self.current, msg);
    }

    // ---- token stream ---------------------------------------------------

    fn advance(&mut self) {
        self.previous = self.current;
        loop {
            self.current = self.scanner.scan_token();
            if self.current.ty != TokenType::Error {
                break;
            }
            let msg = self.current.lexeme;
            self.error_at_current(msg);
        }
    }

    fn consume(&mut self, ty: TokenType, msg: &str) {
        if self.current.ty == ty {
            self.advance();
            return;
        }
        self.error_at_current(msg);
    }

    #[inline]
    fn check(&self, ty: TokenType) -> bool {
        self.current.ty == ty
    }

    fn matches(&mut self, ty: TokenType) -> bool {
        if !self.check(ty) {
            return false;
        }
        self.advance();
        true
    }

    // ---- emitting -------------------------------------------------------

    fn cur(&mut self) -> &mut FnCompiler<'a> {
        self.fns.last_mut().unwrap()
    }

    fn chunk(&mut self) -> &mut Chunk {
        &mut self.fns.last_mut().unwrap().chunk
    }

    fn emit_byte(&mut self, b: u8) {
        let line = self.previous.line;
        let chunk = self.chunk();
        chunk.code.push(b);
        chunk.lines.push(line);
    }

    fn emit_op(&mut self, o: u8) {
        let fc = self.cur();
        let at = fc.chunk.code.len();
        fc.op_starts.push(at);
        self.emit_byte(o);
    }

    fn emit_bytes(&mut self, a: u8, b: u8) {
        self.emit_op(a);
        self.emit_byte(b);
    }

    /// Returns the opcode of the previous instruction if it may be fused
    /// with the instruction about to be emitted.
    fn fusable_op(&mut self) -> Option<u8> {
        let fc = self.cur();
        let len = fc.chunk.code.len();
        match fc.op_starts.last() {
            Some(&at) if fc.barrier != len => Some(fc.chunk.code[at]),
            _ => None,
        }
    }

    fn mark_target(&mut self) -> Mark {
        let fc = self.cur();
        fc.barrier = fc.chunk.code.len();
        Mark { pos: fc.barrier, phantom: fc.phantom }
    }

    fn last_op(&mut self) -> usize {
        *self.cur().op_starts.last().unwrap()
    }

    /// Deletes the most recent instruction.
    fn remove_last_op(&mut self) {
        let fc = self.cur();
        let at = fc.op_starts.pop().unwrap();
        fc.phantom += fc.chunk.code.len() - at;
        fc.chunk.code.truncate(at);
        fc.chunk.lines.truncate(at);
    }

    fn set_last_op(&mut self, o: u8) {
        let at = self.last_op();
        let fc = self.cur();
        fc.chunk.code[at] = o;
    }

    fn emit_binary(&mut self, o: u8) {
        if self.fusable_op() == Some(op::CONSTANT) {
            let fc = self.cur();
            let k = fc.chunk.code[*fc.op_starts.last().unwrap() + 1] as usize;
            let is_num = fc.chunk.constants[k].is_number();
            let fused = match o {
                op::ADD if is_num => Some(op::ADD_C),
                op::SUBTRACT if is_num => Some(op::SUBTRACT_C),
                op::MULTIPLY if is_num => Some(op::MULTIPLY_C),
                op::DIVIDE if is_num => Some(op::DIVIDE_C),
                op::LESS if is_num => Some(op::LESS_C),
                op::GREATER if is_num => Some(op::GREATER_C),
                op::EQUAL => Some(op::EQUAL_C),
                _ => None,
            };
            if let Some(f) = fused {
                self.set_last_op(f);
                return;
            }
        }
        self.emit_op(o);
    }

    fn emit_pop(&mut self) {
        // Discard side-effect-free computations whose result is unused.
        match self.fusable_op() {
            Some(op::CONSTANT | op::NIL | op::TRUE | op::FALSE | op::GET_LOCAL | op::GET_UPVALUE) => {
                self.remove_last_op();
                self.cur().phantom += 1;
                return;
            }
            Some(op::GET_LOCAL2) => {
                // Keep the first push only.
                self.set_last_op(op::GET_LOCAL);
                let fc = self.cur();
                fc.chunk.code.pop();
                fc.chunk.lines.pop();
                fc.phantom += 3;
                return;
            }
            Some(op::EQUAL_C | op::NOT) => {
                self.remove_last_op();
                self.cur().phantom += 1;
                self.emit_pop();
                return;
            }
            Some(op::EQUAL) => {
                self.remove_last_op();
                self.cur().phantom += 1;
                self.emit_pop();
                self.emit_pop();
                return;
            }
            _ => {}
        }
        let fused = match self.fusable_op() {
            Some(op::SET_LOCAL) => Some(op::SET_LOCAL_POP),
            Some(op::SET_GLOBAL) => Some(op::SET_GLOBAL_POP),
            Some(op::SET_PROPERTY) => Some(op::SET_PROPERTY_POP),
            _ => None,
        };
        match fused {
            Some(f) => self.set_last_op(f),
            None => self.emit_op(op::POP),
        }
    }

    /// Emits a jump that pops the condition and jumps if it is falsey,
    /// fusing it with a preceding comparison where possible.
    fn emit_cond_jump(&mut self) -> Mark {
        let fused = match self.fusable_op() {
            Some(op::LESS) => Some(op::LESS_JIF),
            Some(op::GREATER) => Some(op::GREATER_JIF),
            Some(op::EQUAL) => Some(op::EQUAL_JIF),
            Some(op::LESS_C) => Some(op::LESS_C_JIF),
            Some(op::GREATER_C) => Some(op::GREATER_C_JIF),
            Some(op::EQUAL_C) => Some(op::EQUAL_C_JIF),
            Some(op::NOT) => Some(op::JUMP_IF_TRUE_POP),
            _ => None,
        };
        match fused {
            Some(f) => {
                self.set_last_op(f);
                self.emit_byte(0xff);
                self.emit_byte(0xff);
                let fc = self.cur();
                Mark { pos: fc.chunk.code.len() - 2, phantom: fc.phantom }
            }
            None => self.emit_jump(op::JUMP_IF_FALSE_POP),
        }
    }

    fn emit_u16(&mut self, v: u16) {
        self.emit_byte((v >> 8) as u8);
        self.emit_byte(v as u8);
    }

    /// Emits a fresh inline-cache index. Sites beyond u16 range share the last
    /// slot, which is still correct since caches are validated on use.
    fn emit_cache(&mut self) {
        let fc = self.cur();
        let idx = fc.cache_count.min(u16::MAX as usize);
        if fc.cache_count <= u16::MAX as usize {
            fc.cache_count += 1;
        }
        self.emit_u16(idx as u16);
    }

    fn emit_loop(&mut self, loop_start: Mark) {
        self.emit_op(op::LOOP);
        let fc = self.cur();
        let offset = fc.chunk.code.len() - loop_start.pos + 2;
        if offset + (fc.phantom - loop_start.phantom) > u16::MAX as usize {
            self.error("Loop body too large.");
        }
        self.emit_u16(offset as u16);
    }

    fn emit_jump(&mut self, instr: u8) -> Mark {
        self.emit_op(instr);
        self.emit_byte(0xff);
        self.emit_byte(0xff);
        let fc = self.cur();
        Mark { pos: fc.chunk.code.len() - 2, phantom: fc.phantom }
    }

    fn patch_jump(&mut self, jump_mark: Mark) {
        let offset = jump_mark.pos;
        let target = self.mark_target();
        let jump = target.pos - offset - 2;
        if jump + (target.phantom - jump_mark.phantom) > u16::MAX as usize {
            self.error("Too much code to jump over.");
        }
        let code = &mut self.chunk().code;
        code[offset] = (jump >> 8) as u8;
        code[offset + 1] = jump as u8;
    }

    fn emit_return(&mut self) {
        if self.cur().ty == FunctionType::Initializer {
            self.emit_bytes(op::GET_LOCAL, 0);
        } else {
            self.emit_op(op::NIL);
        }
        self.emit_op(op::RETURN);
    }

    fn make_constant(&mut self, value: Value) -> u8 {
        let consts = &mut self.chunk().constants;
        consts.push(value);
        let idx = consts.len() - 1;
        if idx > u8::MAX as usize {
            self.error("Too many constants in one chunk.");
            return 0;
        }
        idx as u8
    }

    fn emit_constant(&mut self, value: Value) {
        let c = self.make_constant(value);
        self.emit_bytes(op::CONSTANT, c);
    }

    // ---- compiler state -------------------------------------------------

    fn init_compiler(&mut self, ty: FunctionType) {
        let name = if ty != FunctionType::Script {
            self.vm.intern(self.previous.lexeme)
        } else {
            std::ptr::null_mut()
        };
        let mut locals = Vec::with_capacity(8);
        locals.push(Local {
            name: if ty != FunctionType::Function { "this" } else { "" },
            depth: 0,
            is_captured: false,
        });
        self.fns.push(FnCompiler {
            chunk: Chunk::new(),
            arity: 0,
            name,
            ty,
            locals,
            upvalues: Vec::new(),
            scope_depth: 0,
            cache_count: 0,
            op_starts: Vec::new(),
            phantom: 0,
            barrier: 0,
        });
    }

    fn end_compiler(&mut self) -> *mut ObjFunction {
        self.emit_return();
        let fc = self.fns.pop().unwrap();
        let upvalue_count = fc.upvalues.len();
        let f = self.vm.new_function(fc.chunk, fc.arity, upvalue_count, fc.name, fc.cache_count);
        if !self.fns.is_empty() {
            let c = self.make_constant(Value::obj(f));
            self.emit_bytes(op::CLOSURE, c);
            for &(index, is_local) in fc.upvalues.iter() {
                self.emit_byte(if is_local { 1 } else { 0 });
                self.emit_byte(index);
            }
        }
        f
    }

    fn begin_scope(&mut self) {
        self.cur().scope_depth += 1;
    }

    fn end_scope(&mut self) {
        self.cur().scope_depth -= 1;
        loop {
            let fc = self.cur();
            let Some(l) = fc.locals.last() else { break };
            if l.depth <= fc.scope_depth {
                break;
            }
            let captured = l.is_captured;
            fc.locals.pop();
            if captured {
                self.emit_op(op::CLOSE_UPVALUE);
            } else {
                self.emit_pop();
            }
        }
    }

    fn identifier_constant(&mut self, name: &str) -> u8 {
        let s = self.vm.intern(name);
        self.make_constant(Value::obj(s))
    }

    fn global_slot(&mut self, name: &str) -> u16 {
        let s = self.vm.intern(name);
        self.vm.global_slot(s)
    }

    fn resolve_local(&mut self, depth: usize, name: &str) -> Option<u8> {
        let fc = &self.fns[depth];
        for (i, l) in fc.locals.iter().enumerate().rev() {
            if l.name == name {
                if l.depth == -1 {
                    self.error("Can't read local variable in its own initializer.");
                }
                return Some(i as u8);
            }
        }
        None
    }

    fn add_upvalue(&mut self, depth: usize, index: u8, is_local: bool) -> u8 {
        let fc = &self.fns[depth];
        for (i, &uv) in fc.upvalues.iter().enumerate() {
            if uv == (index, is_local) {
                return i as u8;
            }
        }
        if fc.upvalues.len() == UINT8_COUNT {
            self.error("Too many closure variables in function.");
            return 0;
        }
        let fc = &mut self.fns[depth];
        fc.upvalues.push((index, is_local));
        (fc.upvalues.len() - 1) as u8
    }

    fn resolve_upvalue(&mut self, depth: usize, name: &str) -> Option<u8> {
        if depth == 0 {
            return None;
        }
        if let Some(local) = self.resolve_local(depth - 1, name) {
            self.fns[depth - 1].locals[local as usize].is_captured = true;
            return Some(self.add_upvalue(depth, local, true));
        }
        if let Some(uv) = self.resolve_upvalue(depth - 1, name) {
            return Some(self.add_upvalue(depth, uv, false));
        }
        None
    }

    fn add_local(&mut self, name: &'a str) {
        if self.cur().locals.len() == UINT8_COUNT {
            self.error("Too many local variables in function.");
            return;
        }
        self.cur().locals.push(Local { name, depth: -1, is_captured: false });
    }

    fn declare_variable(&mut self) {
        if self.cur().scope_depth == 0 {
            return;
        }
        let name = self.previous.lexeme;
        let fc = self.fns.last().unwrap();
        let mut dup = false;
        for l in fc.locals.iter().rev() {
            if l.depth != -1 && l.depth < fc.scope_depth {
                break;
            }
            if l.name == name {
                dup = true;
                break;
            }
        }
        if dup {
            self.error("Already a variable with this name in this scope.");
        }
        self.add_local(name);
    }

    /// Returns the global slot (only meaningful at top-level scope).
    fn parse_variable(&mut self, msg: &str) -> u16 {
        self.consume(TokenType::Identifier, msg);
        self.declare_variable();
        if self.cur().scope_depth > 0 {
            return 0;
        }
        let name = self.previous.lexeme;
        self.global_slot(name)
    }

    fn mark_initialized(&mut self) {
        let fc = self.cur();
        if fc.scope_depth == 0 {
            return;
        }
        let d = fc.scope_depth;
        fc.locals.last_mut().unwrap().depth = d;
    }

    fn define_variable(&mut self, global: u16) {
        if self.cur().scope_depth > 0 {
            self.mark_initialized();
            return;
        }
        self.emit_op(op::DEFINE_GLOBAL);
        self.emit_u16(global);
    }

    fn argument_list(&mut self) -> u8 {
        let mut count: usize = 0;
        if !self.check(TokenType::RightParen) {
            loop {
                self.expression();
                if count == 255 {
                    self.error("Can't have more than 255 arguments.");
                }
                count += 1;
                if !self.matches(TokenType::Comma) {
                    break;
                }
            }
        }
        self.consume(TokenType::RightParen, "Expect ')' after arguments.");
        count.min(255) as u8
    }

    // ---- expressions ----------------------------------------------------

    fn expression(&mut self) {
        self.parse_precedence(Prec::Assignment);
    }

    fn parse_precedence(&mut self, prec: Prec) {
        self.advance();
        let can_assign = prec <= Prec::Assignment;
        if !self.prefix(self.previous.ty, can_assign) {
            self.error("Expect expression.");
            return;
        }
        while prec <= infix_prec(self.current.ty) {
            self.advance();
            self.infix(self.previous.ty, can_assign);
        }
        if can_assign && self.matches(TokenType::Equal) {
            self.error("Invalid assignment target.");
        }
    }

    fn prefix(&mut self, ty: TokenType, can_assign: bool) -> bool {
        use TokenType::*;
        match ty {
            LeftParen => {
                self.expression();
                self.consume(RightParen, "Expect ')' after expression.");
            }
            Minus | Bang => {
                self.parse_precedence(Prec::Unary);
                self.emit_op(if ty == Minus { op::NEGATE } else { op::NOT });
            }
            Identifier => {
                let name = self.previous.lexeme;
                self.named_variable(name, can_assign);
            }
            String => {
                let lex = self.previous.lexeme;
                let s = self.vm.intern(&lex[1..lex.len() - 1]);
                self.emit_constant(Value::obj(s));
            }
            Number => {
                let n: f64 = self.previous.lexeme.parse().unwrap_or(0.0);
                self.emit_constant(Value::number(n));
            }
            False => self.emit_op(op::FALSE),
            True => self.emit_op(op::TRUE),
            Nil => self.emit_op(op::NIL),
            Super => self.super_(),
            This => {
                if self.classes.is_empty() {
                    self.error("Can't use 'this' outside of a class.");
                } else {
                    self.named_variable("this", false);
                }
            }
            _ => return false,
        }
        true
    }

    fn infix(&mut self, ty: TokenType, can_assign: bool) {
        use TokenType::*;
        match ty {
            LeftParen => {
                let argc = self.argument_list();
                self.emit_bytes(op::CALL, argc);
            }
            Dot => {
                self.consume(Identifier, "Expect property name after '.'.");
                let name = self.previous.lexeme;
                let c = self.identifier_constant(name);
                if can_assign && self.matches(Equal) {
                    self.expression();
                    self.emit_bytes(op::SET_PROPERTY, c);
                    self.emit_cache();
                } else if self.matches(LeftParen) {
                    let argc = self.argument_list();
                    self.emit_bytes(op::INVOKE, c);
                    self.emit_byte(argc);
                    self.emit_cache();
                } else {
                    if self.fusable_op() == Some(op::GET_LOCAL) {
                        self.set_last_op(op::GET_LOCAL_PROPERTY);
                        self.emit_byte(c);
                    } else {
                        self.emit_bytes(op::GET_PROPERTY, c);
                    }
                    self.emit_cache();
                }
            }
            And => {
                let end_jump = self.emit_jump(op::JUMP_IF_FALSE);
                self.emit_op(op::POP);
                self.parse_precedence(Prec::And);
                self.patch_jump(end_jump);
            }
            Or => {
                let else_jump = self.emit_jump(op::JUMP_IF_FALSE);
                let end_jump = self.emit_jump(op::JUMP);
                self.patch_jump(else_jump);
                self.emit_op(op::POP);
                self.parse_precedence(Prec::Or);
                self.patch_jump(end_jump);
            }
            _ => {
                self.parse_precedence(infix_prec(ty).next());
                match ty {
                    BangEqual => {
                        self.emit_binary(op::EQUAL);
                        self.emit_op(op::NOT);
                    }
                    EqualEqual => self.emit_binary(op::EQUAL),
                    Greater => self.emit_binary(op::GREATER),
                    GreaterEqual => {
                        self.emit_binary(op::LESS);
                        self.emit_op(op::NOT);
                    }
                    Less => self.emit_binary(op::LESS),
                    LessEqual => {
                        self.emit_binary(op::GREATER);
                        self.emit_op(op::NOT);
                    }
                    Plus => self.emit_binary(op::ADD),
                    Minus => self.emit_binary(op::SUBTRACT),
                    Star => self.emit_binary(op::MULTIPLY),
                    Slash => self.emit_binary(op::DIVIDE),
                    _ => unreachable!(),
                }
            }
        }
    }

    fn named_variable(&mut self, name: &str, can_assign: bool) {
        let depth = self.fns.len() - 1;
        let (get, set, arg): (u8, u8, u16);
        if let Some(a) = self.resolve_local(depth, name) {
            get = op::GET_LOCAL;
            set = op::SET_LOCAL;
            arg = a as u16;
        } else if let Some(a) = self.resolve_upvalue(depth, name) {
            get = op::GET_UPVALUE;
            set = op::SET_UPVALUE;
            arg = a as u16;
        } else {
            get = op::GET_GLOBAL;
            set = op::SET_GLOBAL;
            arg = self.global_slot(name);
        }
        let is_global = get == op::GET_GLOBAL;
        let instr = if can_assign && self.matches(TokenType::Equal) {
            self.expression();
            set
        } else {
            get
        };
        if is_global {
            self.emit_op(instr);
            self.emit_u16(arg);
        } else if instr == op::GET_LOCAL && self.fusable_op() == Some(op::GET_LOCAL) {
            self.set_last_op(op::GET_LOCAL2);
            self.emit_byte(arg as u8);
        } else {
            self.emit_op(instr);
            self.emit_byte(arg as u8);
        }
    }

    fn super_(&mut self) {
        match self.classes.last() {
            None => self.error("Can't use 'super' outside of a class."),
            Some(c) if !c.has_superclass => {
                self.error("Can't use 'super' in a class with no superclass.")
            }
            _ => {}
        }
        self.consume(TokenType::Dot, "Expect '.' after 'super'.");
        self.consume(TokenType::Identifier, "Expect superclass method name.");
        let name = self.previous.lexeme;
        let c = self.identifier_constant(name);
        self.named_variable("this", false);
        if self.matches(TokenType::LeftParen) {
            let argc = self.argument_list();
            self.named_variable("super", false);
            self.emit_bytes(op::SUPER_INVOKE, c);
            self.emit_byte(argc);
        } else {
            self.named_variable("super", false);
            self.emit_bytes(op::GET_SUPER, c);
        }
    }

    // ---- statements -----------------------------------------------------

    fn declaration(&mut self) {
        if self.matches(TokenType::Class) {
            self.class_declaration();
        } else if self.matches(TokenType::Fun) {
            self.fun_declaration();
        } else if self.matches(TokenType::Var) {
            self.var_declaration();
        } else {
            self.statement();
        }
        if self.panic_mode {
            self.synchronize();
        }
    }

    fn synchronize(&mut self) {
        self.panic_mode = false;
        use TokenType::*;
        while self.current.ty != Eof {
            if self.previous.ty == Semicolon {
                return;
            }
            match self.current.ty {
                Class | Fun | Var | For | If | While | Print | Return => return,
                _ => {}
            }
            self.advance();
        }
    }

    fn class_declaration(&mut self) {
        self.consume(TokenType::Identifier, "Expect class name.");
        let class_name = self.previous.lexeme;
        let name_const = self.identifier_constant(class_name);
        self.declare_variable();
        let global = if self.cur().scope_depth == 0 { self.global_slot(class_name) } else { 0 };
        self.emit_bytes(op::CLASS, name_const);
        self.define_variable(global);

        self.classes.push(ClassCompiler { has_superclass: false });

        if self.matches(TokenType::Less) {
            self.consume(TokenType::Identifier, "Expect superclass name.");
            let sup = self.previous.lexeme;
            self.named_variable(sup, false);
            if class_name == sup {
                self.error("A class can't inherit from itself.");
            }
            self.begin_scope();
            self.add_local("super");
            self.define_variable(0);
            self.named_variable(class_name, false);
            self.emit_op(op::INHERIT);
            self.classes.last_mut().unwrap().has_superclass = true;
        }

        self.named_variable(class_name, false);
        self.consume(TokenType::LeftBrace, "Expect '{' before class body.");
        while !self.check(TokenType::RightBrace) && !self.check(TokenType::Eof) {
            self.method();
        }
        self.consume(TokenType::RightBrace, "Expect '}' after class body.");
        self.emit_op(op::POP);

        if self.classes.pop().unwrap().has_superclass {
            self.end_scope();
        }
    }

    fn method(&mut self) {
        self.consume(TokenType::Identifier, "Expect method name.");
        let name = self.previous.lexeme;
        let c = self.identifier_constant(name);
        let ty = if name == "init" { FunctionType::Initializer } else { FunctionType::Method };
        self.function(ty);
        self.emit_bytes(op::METHOD, c);
    }

    fn fun_declaration(&mut self) {
        let global = self.parse_variable("Expect function name.");
        self.mark_initialized();
        self.function(FunctionType::Function);
        self.define_variable(global);
    }

    fn function(&mut self, ty: FunctionType) {
        self.init_compiler(ty);
        self.begin_scope();
        self.consume(TokenType::LeftParen, "Expect '(' after function name.");
        if !self.check(TokenType::RightParen) {
            loop {
                self.cur().arity += 1;
                if self.cur().arity > 255 {
                    self.error_at_current("Can't have more than 255 parameters.");
                }
                let g = self.parse_variable("Expect parameter name.");
                self.define_variable(g);
                if !self.matches(TokenType::Comma) {
                    break;
                }
            }
        }
        self.consume(TokenType::RightParen, "Expect ')' after parameters.");
        self.consume(TokenType::LeftBrace, "Expect '{' before function body.");
        self.block();
        self.end_compiler();
    }

    fn var_declaration(&mut self) {
        let global = self.parse_variable("Expect variable name.");
        if self.matches(TokenType::Equal) {
            self.expression();
        } else {
            self.emit_op(op::NIL);
        }
        self.consume(TokenType::Semicolon, "Expect ';' after variable declaration.");
        self.define_variable(global);
    }

    fn statement(&mut self) {
        use TokenType::*;
        if self.matches(Print) {
            self.expression();
            self.consume(Semicolon, "Expect ';' after value.");
            self.emit_op(op::PRINT);
        } else if self.matches(For) {
            self.for_statement();
        } else if self.matches(If) {
            self.if_statement();
        } else if self.matches(Return) {
            self.return_statement();
        } else if self.matches(While) {
            self.while_statement();
        } else if self.matches(LeftBrace) {
            self.begin_scope();
            self.block();
            self.end_scope();
        } else {
            self.expression();
            self.consume(Semicolon, "Expect ';' after expression.");
            self.emit_pop();
        }
    }

    fn block(&mut self) {
        while !self.check(TokenType::RightBrace) && !self.check(TokenType::Eof) {
            self.declaration();
        }
        self.consume(TokenType::RightBrace, "Expect '}' after block.");
    }

    fn for_statement(&mut self) {
        use TokenType::*;
        self.begin_scope();
        self.consume(LeftParen, "Expect '(' after 'for'.");
        if self.matches(Semicolon) {
        } else if self.matches(Var) {
            self.var_declaration();
        } else {
            self.expression();
            self.consume(Semicolon, "Expect ';' after expression.");
            self.emit_pop();
        }
        let mut loop_start = self.mark_target();
        let mut exit_jump = None;
        if !self.matches(Semicolon) {
            self.expression();
            self.consume(Semicolon, "Expect ';' after loop condition.");
            exit_jump = Some(self.emit_cond_jump());
        }
        if !self.matches(RightParen) {
            let body_jump = self.emit_jump(op::JUMP);
            let increment_start = self.mark_target();
            self.expression();
            self.emit_pop();
            self.consume(RightParen, "Expect ')' after for clauses.");
            self.emit_loop(loop_start);
            loop_start = increment_start;
            self.patch_jump(body_jump);
        }
        self.statement();
        self.emit_loop(loop_start);
        if let Some(j) = exit_jump {
            self.patch_jump(j);
        }
        self.end_scope();
    }

    fn if_statement(&mut self) {
        self.consume(TokenType::LeftParen, "Expect '(' after 'if'.");
        self.expression();
        self.consume(TokenType::RightParen, "Expect ')' after condition.");
        let then_jump = self.emit_cond_jump();
        self.statement();
        if self.matches(TokenType::Else) {
            let else_jump = self.emit_jump(op::JUMP);
            self.patch_jump(then_jump);
            self.statement();
            self.patch_jump(else_jump);
        } else {
            self.patch_jump(then_jump);
        }
    }

    fn return_statement(&mut self) {
        if self.cur().ty == FunctionType::Script {
            self.error("Can't return from top-level code.");
        }
        if self.matches(TokenType::Semicolon) {
            self.emit_return();
        } else {
            if self.cur().ty == FunctionType::Initializer {
                self.error("Can't return a value from an initializer.");
            }
            self.expression();
            self.consume(TokenType::Semicolon, "Expect ';' after return value.");
            self.emit_op(op::RETURN);
        }
    }

    fn while_statement(&mut self) {
        let loop_start = self.mark_target();
        self.consume(TokenType::LeftParen, "Expect '(' after 'while'.");
        self.expression();
        self.consume(TokenType::RightParen, "Expect ')' after condition.");
        let exit_jump = self.emit_cond_jump();
        self.statement();
        self.emit_loop(loop_start);
        self.patch_jump(exit_jump);
    }
}
