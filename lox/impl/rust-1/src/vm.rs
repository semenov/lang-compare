use crate::compiler;
use crate::memory::{pool_alloc, pool_free};
use crate::object::*;
use crate::value::{Value, format_number};
use std::collections::HashMap;
use std::io::Write;
use std::ptr;

pub mod op {
    pub const CONSTANT: u8 = 0;
    pub const NIL: u8 = 1;
    pub const TRUE: u8 = 2;
    pub const FALSE: u8 = 3;
    pub const POP: u8 = 4;
    pub const GET_LOCAL: u8 = 5;
    pub const SET_LOCAL: u8 = 6;
    pub const GET_GLOBAL: u8 = 7;
    pub const DEFINE_GLOBAL: u8 = 8;
    pub const SET_GLOBAL: u8 = 9;
    pub const GET_UPVALUE: u8 = 10;
    pub const SET_UPVALUE: u8 = 11;
    pub const GET_PROPERTY: u8 = 12;
    pub const SET_PROPERTY: u8 = 13;
    pub const GET_SUPER: u8 = 14;
    pub const EQUAL: u8 = 15;
    pub const GREATER: u8 = 16;
    pub const LESS: u8 = 17;
    pub const ADD: u8 = 18;
    pub const SUBTRACT: u8 = 19;
    pub const MULTIPLY: u8 = 20;
    pub const DIVIDE: u8 = 21;
    pub const NOT: u8 = 22;
    pub const NEGATE: u8 = 23;
    pub const PRINT: u8 = 24;
    pub const JUMP: u8 = 25;
    pub const JUMP_IF_FALSE: u8 = 26;
    pub const LOOP: u8 = 27;
    pub const CALL: u8 = 28;
    pub const INVOKE: u8 = 29;
    pub const SUPER_INVOKE: u8 = 30;
    pub const CLOSURE: u8 = 31;
    pub const CLOSE_UPVALUE: u8 = 32;
    pub const RETURN: u8 = 33;
    pub const CLASS: u8 = 34;
    pub const INHERIT: u8 = 35;
    pub const METHOD: u8 = 36;
    // Superinstructions produced by the compiler's peephole fusion.
    pub const ADD_C: u8 = 37;
    pub const SUBTRACT_C: u8 = 38;
    pub const MULTIPLY_C: u8 = 39;
    pub const DIVIDE_C: u8 = 40;
    pub const LESS_C: u8 = 41;
    pub const GREATER_C: u8 = 42;
    pub const EQUAL_C: u8 = 43;
    pub const SET_LOCAL_POP: u8 = 44;
    pub const SET_GLOBAL_POP: u8 = 45;
    pub const SET_PROPERTY_POP: u8 = 46;
    pub const GET_LOCAL_PROPERTY: u8 = 47;
    pub const GET_LOCAL2: u8 = 48;
    pub const JUMP_IF_FALSE_POP: u8 = 49;
    pub const JUMP_IF_TRUE_POP: u8 = 50;
    pub const LESS_JIF: u8 = 51;
    pub const GREATER_JIF: u8 = 52;
    pub const EQUAL_JIF: u8 = 53;
    pub const LESS_C_JIF: u8 = 54;
    pub const GREATER_C_JIF: u8 = 55;
    pub const EQUAL_C_JIF: u8 = 56;
}

/// Upper bound on per-class inline field storage; more fields spill to a
/// separate buffer.
const MAX_INLINE_FIELDS: usize = 32;
const FRAMES_MAX: usize = 65536;
const STACK_MAX: usize = FRAMES_MAX * 256;

#[derive(Clone, Copy)]
struct CallFrame {
    closure: *mut ObjClosure,
    ip: *const u8,
    slots: *mut Value,
}

pub enum InterpretResult {
    Ok,
    CompileError,
    RuntimeError,
}

pub struct Vm {
    stack: Vec<Value>,
    stack_top: *mut Value,
    frames: Vec<CallFrame>,
    frame_count: usize,
    globals: Vec<Value>,
    global_names: Vec<*mut ObjString>,
    global_map: HashMap<*mut ObjString, u16>,
    strings: Table,
    init_string: *mut ObjString,
    open_upvalues: *mut ObjUpvalue,
    objects: *mut Obj,
    gray: Vec<*mut Obj>,
    next_gc: usize,
    gc_enabled: bool,
    out: std::io::BufWriter<std::io::Stdout>,
    scratch: String,
    gc_stress: bool,
}

fn clock_native(_args: &[Value]) -> Value {
    use std::sync::OnceLock;
    static START: OnceLock<std::time::Instant> = OnceLock::new();
    let start = START.get_or_init(std::time::Instant::now);
    Value::number(start.elapsed().as_secs_f64())
}

impl Vm {
    pub fn new() -> Box<Vm> {
        // Zeroed allocations: pages are only touched when used.
        let stack: Vec<Value> = zeroed_vec(STACK_MAX);
        let frames: Vec<CallFrame> = zeroed_vec(FRAMES_MAX);
        let mut vm = Box::new(Vm {
            stack,
            stack_top: ptr::null_mut(),
            frames,
            frame_count: 0,
            globals: Vec::new(),
            global_names: Vec::new(),
            global_map: HashMap::new(),
            strings: Table::new(),
            init_string: ptr::null_mut(),
            open_upvalues: ptr::null_mut(),
            objects: ptr::null_mut(),
            gray: Vec::new(),
            next_gc: 1024 * 1024,
            gc_enabled: false,
            out: std::io::BufWriter::with_capacity(1 << 16, std::io::stdout()),
            scratch: String::new(),
            gc_stress: std::env::var_os("LOX_GC_STRESS").is_some(),
        });
        vm.stack_top = vm.stack.as_mut_ptr();
        vm.init_string = vm.intern("init");
        let name = vm.intern("clock");
        let slot = vm.global_slot(name);
        let native = vm.alloc(ObjNative { obj: Vm::header(ObjType::Native), function: clock_native });
        vm.globals[slot as usize] = Value::obj(native);
        clock_native(&[]);
        vm
    }

    pub fn interpret(&mut self, source: &str) -> InterpretResult {
        let Some(function) = compiler::compile(self, source) else {
            return InterpretResult::CompileError;
        };
        self.gc_enabled = true;
        self.push(Value::obj(function));
        let closure = self.new_closure(function);
        self.pop();
        self.push(Value::obj(closure));
        let r = self.call_closure(closure, 0);
        let result = if r { self.run() } else { InterpretResult::RuntimeError };
        let _ = self.out.flush();
        result
    }

    // ---- allocation -----------------------------------------------------

    #[inline(always)]
    fn header(ty: ObjType) -> Obj {
        Obj { ty, marked: false, aux: 0, next: ptr::null_mut() }
    }

    #[inline(always)]
    fn maybe_gc(&mut self) {
        if self.gc_enabled && (unsafe { BYTES_ALLOCATED } > self.next_gc || self.gc_stress) {
            self.collect_garbage();
        }
    }

    fn alloc<T>(&mut self, obj: T) -> *mut T {
        let p = self.alloc_raw(std::mem::size_of::<T>()) as *mut T;
        unsafe {
            p.write(obj);
            (*(p as *mut Obj)).next = self.objects;
        }
        self.objects = p as *mut Obj;
        p
    }

    /// Allocates raw object storage, possibly collecting first. The caller
    /// must initialize the header and link it into `objects`.
    #[inline(always)]
    fn alloc_raw(&mut self, size: usize) -> *mut u8 {
        track_alloc(size);
        self.maybe_gc();
        pool_alloc(size)
    }

    pub fn intern(&mut self, s: &str) -> *mut ObjString {
        let hash = hash_string(s.as_bytes());
        let found = self.strings.find_string(s, hash);
        if !found.is_null() {
            return found;
        }
        self.new_string(s, hash)
    }

    /// Interns the concatenation of two strings.
    fn concat(&mut self, a: *mut ObjString, b: *mut ObjString) -> *mut ObjString {
        let mut buf = std::mem::take(&mut self.scratch);
        buf.clear();
        unsafe {
            buf.push_str((*a).as_str());
            buf.push_str((*b).as_str());
        }
        let r = self.intern(&buf);
        self.scratch = buf;
        r
    }

    fn new_string(&mut self, chars: &str, hash: u32) -> *mut ObjString {
        let p = self.alloc_raw(ObjString::alloc_size(chars.len())) as *mut ObjString;
        unsafe {
            p.write(ObjString {
                obj: Obj { ty: ObjType::String, marked: false, aux: 0, next: self.objects },
                hash,
                len: chars.len() as u32,
            });
            ptr::copy_nonoverlapping(chars.as_ptr(), p.add(1) as *mut u8, chars.len());
        }
        self.objects = p as *mut Obj;
        self.strings.set(p, Value::NIL);
        p
    }

    pub fn new_function(
        &mut self,
        chunk: Chunk,
        arity: usize,
        upvalue_count: usize,
        name: *mut ObjString,
        cache_count: usize,
    ) -> *mut ObjFunction {
        track_alloc(function_heap_size(&chunk, cache_count));
        let caches = vec![InlineCache::EMPTY; cache_count];
        self.alloc(ObjFunction { obj: Vm::header(ObjType::Function), arity, upvalue_count, chunk, name, caches })
    }

    fn new_instance(&mut self, class: *mut ObjClass) -> *mut ObjInstance {
        let cap = unsafe { (*class).field_hint };
        let p = self.alloc_raw(ObjInstance::alloc_size(cap)) as *mut ObjInstance;
        unsafe {
            p.write(ObjInstance {
                obj: Obj { ty: ObjType::Instance, marked: false, aux: cap as u32, next: self.objects },
                class,
                shape: (*class).root_shape,
                fields: ObjInstance::inline_fields(p),
            });
            self.objects = p as *mut Obj;
            p
        }
    }

    fn new_class(&mut self, name: *mut ObjString) -> *mut ObjClass {
        self.alloc(ObjClass {
            obj: Vm::header(ObjType::Class),
            name,
            methods: Table::new(),
            initializer: Value::NIL,
            root_shape: Shape::new_root(),
            field_hint: 0,
        })
    }

    fn new_closure(&mut self, function: *mut ObjFunction) -> *mut ObjClosure {
        let n = unsafe { (*function).upvalue_count };
        let p = self.alloc_raw(ObjClosure::alloc_size(n)) as *mut ObjClosure;
        unsafe {
            p.write(ObjClosure {
                obj: Obj { ty: ObjType::Closure, marked: false, aux: n as u32, next: self.objects },
                function,
            });
            let ups = ObjClosure::upvalues(p);
            for i in 0..n {
                *ups.add(i) = ptr::null_mut();
            }
        }
        self.objects = p as *mut Obj;
        p
    }

    pub fn global_slot(&mut self, name: *mut ObjString) -> u16 {
        if let Some(&s) = self.global_map.get(&name) {
            return s;
        }
        let s = self.globals.len();
        if s > u16::MAX as usize {
            eprintln!("Too many global variables.");
            std::process::exit(65);
        }
        self.globals.push(Value::UNDEF);
        self.global_names.push(name);
        self.global_map.insert(name, s as u16);
        s as u16
    }

    // ---- stack ----------------------------------------------------------

    #[inline(always)]
    fn push(&mut self, v: Value) {
        unsafe {
            *self.stack_top = v;
            self.stack_top = self.stack_top.add(1);
        }
    }

    #[inline(always)]
    fn pop(&mut self) -> Value {
        unsafe {
            self.stack_top = self.stack_top.sub(1);
            *self.stack_top
        }
    }


    // ---- garbage collection --------------------------------------------

    #[inline]
    fn mark_object(&mut self, o: *mut Obj) {
        if o.is_null() {
            return;
        }
        unsafe {
            if (*o).marked {
                return;
            }
            (*o).marked = true;
        }
        self.gray.push(o);
    }

    #[inline]
    fn mark_value(&mut self, v: Value) {
        if v.is_obj() {
            self.mark_object(v.as_obj());
        }
    }

    fn mark_table(&mut self, t: *const Table) {
        unsafe {
            for e in (*t).entries.iter() {
                if !e.key.is_null() {
                    self.mark_object(e.key as *mut Obj);
                    self.mark_value(e.value);
                }
            }
        }
    }

    fn blacken(&mut self, o: *mut Obj) {
        unsafe {
            match (*o).ty {
                ObjType::String | ObjType::Native => {}
                ObjType::Upvalue => {
                    let u = o as *mut ObjUpvalue;
                    self.mark_value((*u).closed);
                }
                ObjType::Function => {
                    let f = o as *mut ObjFunction;
                    self.mark_object((*f).name as *mut Obj);
                    for i in 0..(*f).chunk.constants.len() {
                        let v = (&(*f).chunk.constants)[i];
                        self.mark_value(v);
                    }
                }
                ObjType::Closure => {
                    let c = o as *mut ObjClosure;
                    self.mark_object((*c).function as *mut Obj);
                    let ups = ObjClosure::upvalues(c);
                    for i in 0..(*c).obj.aux as usize {
                        self.mark_object(*ups.add(i) as *mut Obj);
                    }
                }
                ObjType::Class => {
                    let k = o as *mut ObjClass;
                    self.mark_object((*k).name as *mut Obj);
                    self.mark_table(&(*k).methods);
                    self.mark_value((*k).initializer);
                    let mut shapes = vec![(*k).root_shape];
                    while let Some(sh) = shapes.pop() {
                        for &(n, c) in (*sh).transitions.iter() {
                            self.mark_object(n as *mut Obj);
                            shapes.push(c);
                        }
                    }
                }
                ObjType::Instance => {
                    let i = o as *mut ObjInstance;
                    self.mark_object((*i).class as *mut Obj);
                    for j in 0..(*i).len() {
                        let v = (*i).field(j);
                        self.mark_value(v);
                    }
                }
                ObjType::BoundMethod => {
                    let b = o as *mut ObjBoundMethod;
                    self.mark_value((*b).receiver);
                    self.mark_object((*b).method as *mut Obj);
                }
            }
        }
    }

    fn collect_garbage(&mut self) {
        // Roots.
        let mut slot = self.stack.as_mut_ptr();
        while slot < self.stack_top {
            let v = unsafe { *slot };
            self.mark_value(v);
            slot = unsafe { slot.add(1) };
        }
        for i in 0..self.frame_count {
            let c = self.frames[i].closure;
            self.mark_object(c as *mut Obj);
        }
        let mut u = self.open_upvalues;
        while !u.is_null() {
            self.mark_object(u as *mut Obj);
            u = unsafe { (*u).next };
        }
        for i in 0..self.globals.len() {
            let v = self.globals[i];
            self.mark_value(v);
            let n = self.global_names[i];
            self.mark_object(n as *mut Obj);
        }
        self.mark_object(self.init_string as *mut Obj);

        // Trace.
        while let Some(o) = self.gray.pop() {
            self.blacken(o);
        }

        // Weak string table.
        for e in self.strings.entries.iter_mut() {
            if !e.key.is_null() && unsafe { !(*e.key).obj.marked } {
                e.key = ptr::null_mut();
                e.value = Value::TRUE;
            }
        }

        // Sweep.
        let mut prev: *mut Obj = ptr::null_mut();
        let mut obj = self.objects;
        while !obj.is_null() {
            unsafe {
                if (*obj).marked {
                    (*obj).marked = false;
                    prev = obj;
                    obj = (*obj).next;
                } else {
                    let unreached = obj;
                    obj = (*obj).next;
                    if prev.is_null() {
                        self.objects = obj;
                    } else {
                        (*prev).next = obj;
                    }
                    free_object(unreached);
                }
            }
        }

        self.next_gc = unsafe { BYTES_ALLOCATED }.max(1024 * 1024) * 2;
    }

    // ---- runtime helpers -----------------------------------------------

    fn runtime_error(&mut self, msg: &str) {
        let _ = self.out.flush();
        let mut err = String::new();
        err.push_str(msg);
        err.push('\n');
        for i in (0..self.frame_count).rev() {
            let frame = &self.frames[i];
            unsafe {
                let function = (*frame.closure).function;
                let code = (*function).chunk.code.as_ptr();
                let offset = frame.ip.offset_from(code) as usize - 1;
                let line = (&(*function).chunk.lines)[offset];
                err.push_str(&format!("[line {}] in ", line));
                if (*function).name.is_null() {
                    err.push_str("script\n");
                } else {
                    err.push_str(&format!("{}()\n", (*(*function).name).as_str()));
                }
            }
        }
        eprint!("{}", err);
        self.reset_stack();
    }

    fn reset_stack(&mut self) {
        self.stack_top = self.stack.as_mut_ptr();
        self.frame_count = 0;
        self.open_upvalues = ptr::null_mut();
    }

    #[inline(always)]
    fn call_closure(&mut self, closure: *mut ObjClosure, argc: usize) -> bool {
        let arity = unsafe { (*(*closure).function).arity };
        if argc != arity {
            self.runtime_error(&format!("Expected {} arguments but got {}.", arity, argc));
            return false;
        }
        if self.frame_count == FRAMES_MAX {
            self.runtime_error("Stack overflow.");
            return false;
        }
        let frame = &mut self.frames[self.frame_count];
        self.frame_count += 1;
        frame.closure = closure;
        frame.ip = unsafe { (*(*closure).function).chunk.code.as_ptr() };
        frame.slots = unsafe { self.stack_top.sub(argc + 1) };
        true
    }

    fn call_value(&mut self, callee: Value, argc: usize) -> bool {
        if callee.is_obj() {
            let o = callee.as_obj();
            unsafe {
                match (*o).ty {
                    ObjType::BoundMethod => {
                        let b = o as *mut ObjBoundMethod;
                        *self.stack_top.sub(argc + 1) = (*b).receiver;
                        return self.call_closure((*b).method, argc);
                    }
                    ObjType::Class => {
                        let k = o as *mut ObjClass;
                        let inst = self.new_instance(k);
                        *self.stack_top.sub(argc + 1) = Value::obj(inst);
                        let init = (*k).initializer;
                        if !init.is_nil() {
                            return self.call_closure(init.as_obj() as *mut ObjClosure, argc);
                        } else if argc != 0 {
                            self.runtime_error(&format!("Expected 0 arguments but got {}.", argc));
                            return false;
                        }
                        return true;
                    }
                    ObjType::Closure => {
                        return self.call_closure(o as *mut ObjClosure, argc);
                    }
                    ObjType::Native => {
                        let n = o as *mut ObjNative;
                        let args = std::slice::from_raw_parts(self.stack_top.sub(argc), argc);
                        let result = ((*n).function)(args);
                        self.stack_top = self.stack_top.sub(argc + 1);
                        self.push(result);
                        return true;
                    }
                    _ => {}
                }
            }
        }
        self.runtime_error("Can only call functions and classes.");
        false
    }




    fn capture_upvalue(&mut self, local: *mut Value) -> *mut ObjUpvalue {
        let mut prev: *mut ObjUpvalue = ptr::null_mut();
        let mut upvalue = self.open_upvalues;
        unsafe {
            while !upvalue.is_null() && (*upvalue).location > local {
                prev = upvalue;
                upvalue = (*upvalue).next;
            }
            if !upvalue.is_null() && (*upvalue).location == local {
                return upvalue;
            }
        }
        let created = self.alloc(ObjUpvalue {
            obj: Vm::header(ObjType::Upvalue),
            location: local,
            closed: Value::NIL,
            next: upvalue,
        });
        if prev.is_null() {
            self.open_upvalues = created;
        } else {
            unsafe { (*prev).next = created };
        }
        created
    }

    #[inline(always)]
    fn close_upvalues(&mut self, last: *mut Value) {
        unsafe {
            while !self.open_upvalues.is_null() && (*self.open_upvalues).location >= last {
                let u = self.open_upvalues;
                (*u).closed = *(*u).location;
                (*u).location = &mut (*u).closed;
                self.open_upvalues = (*u).next;
            }
        }
    }

    fn print_value(&mut self, v: Value) {
        let s = value_to_string(v);
        let _ = self.out.write_all(s.as_bytes());
        let _ = self.out.write_all(b"\n");
    }

    // ---- interpreter loop ----------------------------------------------
    //
    // Stack convention: `sp` points one past the top. The top value is also
    // cached in the register-resident `tos`; stack memory is always kept up
    // to date (write-through), so reading memory is always valid and only
    // writes to the top slot must also refresh `tos`.

    fn run(&mut self) -> InterpretResult {
        let mut frame: *mut CallFrame;
        let mut ip: *const u8;
        let mut slots: *mut Value;
        let mut constants: *const Value;
        let mut caches: *mut InlineCache;
        let mut upvalues: *mut *mut ObjUpvalue;
        let mut sp: *mut Value = self.stack_top;
        let mut tos: Value;

        macro_rules! load_frame {
            () => {{
                frame = self.frames.as_mut_ptr().add(self.frame_count - 1);
                ip = (*frame).ip;
                slots = (*frame).slots;
                let closure = (*frame).closure;
                upvalues = ObjClosure::upvalues(closure);
                let function = (*closure).function;
                constants = (*function).chunk.constants.as_ptr();
                caches = (*function).caches.as_mut_ptr();
            }};
        }
        // Publish sp/ip before calling anything that may GC or inspect frames.
        macro_rules! sync {
            () => {{
                self.stack_top = sp;
                (*frame).ip = ip;
            }};
        }
        macro_rules! read_byte {
            () => {{
                let b = *ip;
                ip = ip.add(1);
                b
            }};
        }
        macro_rules! read_u16 {
            () => {{
                let v = ((*ip as u16) << 8) | (*ip.add(1) as u16);
                ip = ip.add(2);
                v
            }};
        }
        macro_rules! read_constant {
            () => {
                *constants.add(read_byte!() as usize)
            };
        }
        macro_rules! read_string {
            () => {
                read_constant!().as_string()
            };
        }
        macro_rules! push {
            ($v:expr) => {{
                let v = $v;
                *sp = v;
                sp = sp.add(1);
                tos = v;
            }};
        }
        macro_rules! drop_n {
            ($n:expr) => {{
                sp = sp.sub($n);
                tos = *sp.sub(1);
            }};
        }
        macro_rules! pop {
            () => {{
                let v = tos;
                drop_n!(1);
                v
            }};
        }
        macro_rules! set_top {
            ($v:expr) => {{
                let v = $v;
                *sp.sub(1) = v;
                tos = v;
            }};
        }
        // Replace the top two values with one.
        macro_rules! replace2 {
            ($v:expr) => {{
                let v = $v;
                sp = sp.sub(1);
                *sp.sub(1) = v;
                tos = v;
            }};
        }
        macro_rules! peek {
            ($d:expr) => {
                *sp.sub(1 + $d)
            };
        }
        macro_rules! rt_error {
            ($($arg:tt)*) => {{
                sync!();
                self.runtime_error(&format!($($arg)*));
                return InterpretResult::RuntimeError;
            }};
        }
        macro_rules! binary_num {
            ($op:tt, $wrap:expr) => {{
                let b = tos;
                let a = peek!(1);
                if !a.is_number() || !b.is_number() {
                    rt_error!("Operands must be numbers.");
                }
                replace2!($wrap(a.as_number() $op b.as_number()));
            }};
        }
        macro_rules! binary_const {
            ($op:tt, $wrap:expr) => {{
                let b = read_constant!();
                let a = tos;
                if !a.is_number() {
                    rt_error!("Operands must be numbers.");
                }
                set_top!($wrap(a.as_number() $op b.as_number()));
            }};
        }
        // Pops two operands; jumps unless `a op b`.
        macro_rules! compare_jump {
            ($op:tt) => {{
                let b = tos;
                let a = peek!(1);
                let off = read_u16!() as usize;
                if !a.is_number() || !b.is_number() {
                    rt_error!("Operands must be numbers.");
                }
                drop_n!(2);
                if !(a.as_number() $op b.as_number()) {
                    ip = ip.add(off);
                }
            }};
        }
        macro_rules! compare_const_jump {
            ($op:tt) => {{
                let b = read_constant!();
                let a = tos;
                let off = read_u16!() as usize;
                if !a.is_number() {
                    rt_error!("Operands must be numbers.");
                }
                drop_n!(1);
                if !(a.as_number() $op b.as_number()) {
                    ip = ip.add(off);
                }
            }};
        }
        macro_rules! undefined_property {
            ($name:expr) => {
                rt_error!("Undefined property '{}'.", (*$name).as_str())
            };
        }
        // Replace the receiver on top of the stack with a bound method.
        macro_rules! bind {
            ($method:expr) => {{
                let method = $method;
                sync!();
                let receiver = tos;
                let bound = self.alloc(ObjBoundMethod {
                    obj: Vm::header(ObjType::BoundMethod),
                    receiver,
                    method: method.as_obj() as *mut ObjClosure,
                });
                set_top!(Value::obj(bound));
            }};
        }
        // Property read with inline cache; the receiver must be on top of
        // the stack and is replaced by the result.
        macro_rules! get_property {
            () => {{
                let recv: Value = tos;
                if !recv.is_obj_type(ObjType::Instance) {
                    rt_error!("Only instances have properties.");
                }
                let inst = recv.as_obj() as *mut ObjInstance;
                let name = read_string!();
                let ic = &mut *caches.add(read_u16!() as usize);
                let shape = (*inst).shape;
                if (*shape).id == ic.shape_id {
                    if ic.slot != NO_SLOT {
                        set_top!((*inst).field(ic.slot as usize));
                    } else {
                        bind!(ic.method);
                    }
                } else if let Some(slot) = (*shape).find(name) {
                    *ic = InlineCache {
                        shape_id: (*shape).id,
                        slot: slot as u32,
                        next_shape: ptr::null_mut(),
                        method: Value::NIL,
                    };
                    set_top!((*inst).field(slot));
                } else if let Some(m) = (*(*inst).class).methods.get(name) {
                    *ic = InlineCache {
                        shape_id: (*shape).id,
                        slot: NO_SLOT,
                        next_shape: ptr::null_mut(),
                        method: m,
                    };
                    bind!(m);
                } else {
                    undefined_property!(name);
                }
            }};
        }
        // Stores tos into the instance at sp-2; evaluates to the value.
        macro_rules! set_property {
            () => {{
                let target = peek!(1);
                if !target.is_obj_type(ObjType::Instance) {
                    rt_error!("Only instances have fields.");
                }
                let inst = target.as_obj() as *mut ObjInstance;
                let name = read_string!();
                let ic = &mut *caches.add(read_u16!() as usize);
                let value = tos;
                let shape = (*inst).shape;
                if (*shape).id == ic.shape_id && ic.next_shape.is_null() {
                    (*inst).set_field(ic.slot as usize, value);
                } else if let Some(slot) = (*shape).find(name) {
                    *ic = InlineCache {
                        shape_id: (*shape).id,
                        slot: slot as u32,
                        next_shape: ptr::null_mut(),
                        method: Value::NIL,
                    };
                    (*inst).set_field(slot, value);
                } else {
                    let next = if (*shape).id == ic.shape_id {
                        ic.next_shape
                    } else {
                        let next = (*shape).transition(name);
                        *ic = InlineCache {
                            shape_id: (*shape).id,
                            slot: (*shape).names.len() as u32,
                            next_shape: next,
                            method: Value::NIL,
                        };
                        let k = (*inst).class;
                        let n = (*next).names.len();
                        if n > (*k).field_hint && n <= MAX_INLINE_FIELDS {
                            (*k).field_hint = n;
                        }
                        next
                    };
                    ObjInstance::push_field(inst, (*shape).names.len(), value);
                    (*inst).shape = next;
                }
                value
            }};
        }
        macro_rules! call_closure {
            ($closure:expr, $argc:expr) => {{
                let closure: *mut ObjClosure = $closure;
                let argc: usize = $argc;
                let function = (*closure).function;
                if argc != (*function).arity {
                    rt_error!("Expected {} arguments but got {}.", (*function).arity, argc);
                }
                if self.frame_count == FRAMES_MAX {
                    rt_error!("Stack overflow.");
                }
                (*frame).ip = ip;
                frame = self.frames.as_mut_ptr().add(self.frame_count);
                self.frame_count += 1;
                (*frame).closure = closure;
                ip = (*function).chunk.code.as_ptr();
                slots = sp.sub(argc + 1);
                (*frame).slots = slots;
                upvalues = ObjClosure::upvalues(closure);
                constants = (*function).chunk.constants.as_ptr();
                caches = (*function).caches.as_mut_ptr();
            }};
        }
        // Slow path for calls: delegate to call_value and resync.
        macro_rules! call_value_slow {
            ($callee:expr, $argc:expr) => {{
                sync!();
                if !self.call_value($callee, $argc) {
                    return InterpretResult::RuntimeError;
                }
                sp = self.stack_top;
                tos = *sp.sub(1);
                load_frame!();
            }};
        }

        unsafe {
            load_frame!();
            tos = *sp.sub(1);
            loop {
                let instruction = read_byte!();
                match instruction {
                    op::CONSTANT => {
                        let c = read_constant!();
                        push!(c);
                    }
                    op::NIL => push!(Value::NIL),
                    op::TRUE => push!(Value::TRUE),
                    op::FALSE => push!(Value::FALSE),
                    op::POP => drop_n!(1),
                    op::GET_LOCAL => {
                        let s = read_byte!() as usize;
                        push!(*slots.add(s));
                    }
                    op::SET_LOCAL => {
                        let s = read_byte!() as usize;
                        *slots.add(s) = tos;
                    }
                    op::SET_LOCAL_POP => {
                        let s = read_byte!() as usize;
                        *slots.add(s) = tos;
                        drop_n!(1);
                    }
                    op::GET_LOCAL2 => {
                        let a = read_byte!() as usize;
                        let b = read_byte!() as usize;
                        let vb = *slots.add(b);
                        *sp = *slots.add(a);
                        *sp.add(1) = vb;
                        sp = sp.add(2);
                        tos = vb;
                    }
                    op::GET_GLOBAL => {
                        let s = read_u16!() as usize;
                        let v = *self.globals.get_unchecked(s);
                        if v == Value::UNDEF {
                            let name = (*self.global_names[s]).as_str();
                            rt_error!("Undefined variable '{}'.", name);
                        }
                        push!(v);
                    }
                    op::DEFINE_GLOBAL => {
                        let s = read_u16!() as usize;
                        *self.globals.get_unchecked_mut(s) = tos;
                        drop_n!(1);
                    }
                    op::SET_GLOBAL | op::SET_GLOBAL_POP => {
                        let s = read_u16!() as usize;
                        let g = self.globals.get_unchecked_mut(s);
                        if *g == Value::UNDEF {
                            let name = (*self.global_names[s]).as_str();
                            rt_error!("Undefined variable '{}'.", name);
                        }
                        *g = tos;
                        if instruction == op::SET_GLOBAL_POP {
                            drop_n!(1);
                        }
                    }
                    op::GET_UPVALUE => {
                        let s = read_byte!() as usize;
                        let u = *upvalues.add(s);
                        push!(*(*u).location);
                    }
                    op::SET_UPVALUE => {
                        let s = read_byte!() as usize;
                        let u = *upvalues.add(s);
                        *(*u).location = tos;
                    }
                    op::GET_PROPERTY => get_property!(),
                    op::GET_LOCAL_PROPERTY => {
                        let slot = read_byte!() as usize;
                        push!(*slots.add(slot));
                        get_property!();
                    }
                    op::SET_PROPERTY => {
                        let value = set_property!();
                        replace2!(value);
                    }
                    op::SET_PROPERTY_POP => {
                        set_property!();
                        drop_n!(2);
                    }
                    op::GET_SUPER => {
                        let name = read_string!();
                        let superclass = pop!().as_obj() as *mut ObjClass;
                        match (*superclass).methods.get(name) {
                            Some(m) => bind!(m),
                            None => undefined_property!(name),
                        }
                    }
                    op::EQUAL => {
                        let b = tos;
                        let a = peek!(1);
                        replace2!(Value::bool(a.equals(b)));
                    }
                    op::GREATER => binary_num!(>, Value::bool),
                    op::LESS => binary_num!(<, Value::bool),
                    op::ADD => {
                        let b = tos;
                        let a = peek!(1);
                        if a.is_number() && b.is_number() {
                            replace2!(Value::number(a.as_number() + b.as_number()));
                        } else if a.is_obj_type(ObjType::String) && b.is_obj_type(ObjType::String) {
                            sync!();
                            let r = self.concat(a.as_string(), b.as_string());
                            replace2!(Value::obj(r));
                        } else {
                            rt_error!("Operands must be two numbers or two strings.");
                        }
                    }
                    op::SUBTRACT => binary_num!(-, Value::number),
                    op::MULTIPLY => binary_num!(*, Value::number),
                    op::DIVIDE => binary_num!(/, Value::number),
                    op::ADD_C => {
                        let b = read_constant!();
                        let a = tos;
                        if !a.is_number() {
                            rt_error!("Operands must be two numbers or two strings.");
                        }
                        set_top!(Value::number(a.as_number() + b.as_number()));
                    }
                    op::SUBTRACT_C => binary_const!(-, Value::number),
                    op::MULTIPLY_C => binary_const!(*, Value::number),
                    op::DIVIDE_C => binary_const!(/, Value::number),
                    op::LESS_C => binary_const!(<, Value::bool),
                    op::GREATER_C => binary_const!(>, Value::bool),
                    op::EQUAL_C => {
                        let b = read_constant!();
                        set_top!(Value::bool(tos.equals(b)));
                    }
                    op::NOT => set_top!(Value::bool(tos.is_falsey())),
                    op::NEGATE => {
                        if !tos.is_number() {
                            rt_error!("Operand must be a number.");
                        }
                        set_top!(Value::number(-tos.as_number()));
                    }
                    op::PRINT => {
                        let v = pop!();
                        self.print_value(v);
                    }
                    op::JUMP => {
                        let off = read_u16!() as usize;
                        ip = ip.add(off);
                    }
                    op::JUMP_IF_FALSE => {
                        let off = read_u16!() as usize;
                        if tos.is_falsey() {
                            ip = ip.add(off);
                        }
                    }
                    op::JUMP_IF_FALSE_POP => {
                        let off = read_u16!() as usize;
                        if pop!().is_falsey() {
                            ip = ip.add(off);
                        }
                    }
                    op::JUMP_IF_TRUE_POP => {
                        let off = read_u16!() as usize;
                        if !pop!().is_falsey() {
                            ip = ip.add(off);
                        }
                    }
                    op::LESS_JIF => compare_jump!(<),
                    op::GREATER_JIF => compare_jump!(>),
                    op::LESS_C_JIF => compare_const_jump!(<),
                    op::GREATER_C_JIF => compare_const_jump!(>),
                    op::EQUAL_JIF => {
                        let b = tos;
                        let a = peek!(1);
                        let off = read_u16!() as usize;
                        drop_n!(2);
                        if !a.equals(b) {
                            ip = ip.add(off);
                        }
                    }
                    op::EQUAL_C_JIF => {
                        let b = read_constant!();
                        let a = tos;
                        let off = read_u16!() as usize;
                        drop_n!(1);
                        if !a.equals(b) {
                            ip = ip.add(off);
                        }
                    }
                    op::LOOP => {
                        let off = read_u16!() as usize;
                        ip = ip.sub(off);
                    }
                    op::CALL => {
                        let argc = read_byte!() as usize;
                        let callee = peek!(argc);
                        if callee.is_obj_type(ObjType::Closure) {
                            call_closure!(callee.as_obj() as *mut ObjClosure, argc);
                        } else if callee.is_obj_type(ObjType::Class) {
                            let k = callee.as_obj() as *mut ObjClass;
                            sync!();
                            let inst = self.new_instance(k);
                            *sp.sub(argc + 1) = Value::obj(inst);
                            tos = *sp.sub(1);
                            let init = (*k).initializer;
                            if !init.is_nil() {
                                call_closure!(init.as_obj() as *mut ObjClosure, argc);
                            } else if argc != 0 {
                                rt_error!("Expected 0 arguments but got {}.", argc);
                            }
                        } else {
                            call_value_slow!(callee, argc);
                        }
                    }
                    op::INVOKE => {
                        let name = read_string!();
                        let argc = read_byte!() as usize;
                        let ic = &mut *caches.add(read_u16!() as usize);
                        let receiver = peek!(argc);
                        if !receiver.is_obj_type(ObjType::Instance) {
                            rt_error!("Only instances have methods.");
                        }
                        let inst = receiver.as_obj() as *mut ObjInstance;
                        let shape = (*inst).shape;
                        if (*shape).id == ic.shape_id {
                            if ic.slot == NO_SLOT {
                                call_closure!(ic.method.as_obj() as *mut ObjClosure, argc);
                            } else {
                                let v = (*inst).field(ic.slot as usize);
                                *sp.sub(argc + 1) = v;
                                call_value_slow!(v, argc);
                            }
                        } else if let Some(slot) = (*shape).find(name) {
                            *ic = InlineCache {
                                shape_id: (*shape).id,
                                slot: slot as u32,
                                next_shape: ptr::null_mut(),
                                method: Value::NIL,
                            };
                            let v = (*inst).field(slot);
                            *sp.sub(argc + 1) = v;
                            call_value_slow!(v, argc);
                        } else if let Some(m) = (*(*inst).class).methods.get(name) {
                            *ic = InlineCache {
                                shape_id: (*shape).id,
                                slot: NO_SLOT,
                                next_shape: ptr::null_mut(),
                                method: m,
                            };
                            call_closure!(m.as_obj() as *mut ObjClosure, argc);
                        } else {
                            undefined_property!(name);
                        }
                    }
                    op::SUPER_INVOKE => {
                        let name = read_string!();
                        let argc = read_byte!() as usize;
                        let superclass = pop!().as_obj() as *mut ObjClass;
                        match (*superclass).methods.get(name) {
                            Some(m) => call_closure!(m.as_obj() as *mut ObjClosure, argc),
                            None => undefined_property!(name),
                        }
                    }
                    op::CLOSURE => {
                        let function = read_constant!().as_obj() as *mut ObjFunction;
                        sync!();
                        let closure = self.new_closure(function);
                        push!(Value::obj(closure));
                        self.stack_top = sp;
                        let n = (*function).upvalue_count;
                        for i in 0..n {
                            let is_local = read_byte!();
                            let index = read_byte!() as usize;
                            let uv = if is_local != 0 {
                                self.capture_upvalue(slots.add(index))
                            } else {
                                *upvalues.add(index)
                            };
                            *ObjClosure::upvalues(closure).add(i) = uv;
                        }
                    }
                    op::CLOSE_UPVALUE => {
                        self.close_upvalues(sp.sub(1));
                        drop_n!(1);
                    }
                    op::RETURN => {
                        let result = tos;
                        if !self.open_upvalues.is_null() {
                            self.close_upvalues(slots);
                        }
                        self.frame_count -= 1;
                        if self.frame_count == 0 {
                            self.stack_top = slots;
                            return InterpretResult::Ok;
                        }
                        sp = slots;
                        push!(result);
                        load_frame!();
                    }
                    op::CLASS => {
                        let name = read_string!();
                        sync!();
                        let k = self.new_class(name);
                        push!(Value::obj(k));
                    }
                    op::INHERIT => {
                        let superclass = peek!(1);
                        if !superclass.is_obj_type(ObjType::Class) {
                            rt_error!("Superclass must be a class.");
                        }
                        let sup = superclass.as_obj() as *mut ObjClass;
                        let sub = tos.as_obj() as *mut ObjClass;
                        (*sup).methods.add_all(&mut (*sub).methods);
                        (*sub).initializer = (*sup).initializer;
                        drop_n!(1);
                    }
                    op::METHOD => {
                        let name = read_string!();
                        let method = tos;
                        let k = peek!(1).as_obj() as *mut ObjClass;
                        (*k).methods.set(name, method);
                        if name == self.init_string {
                            (*k).initializer = method;
                        }
                        drop_n!(1);
                    }
                    _ => std::hint::unreachable_unchecked(),
                }
            }
        }
    }
}

fn function_heap_size(chunk: &Chunk, cache_count: usize) -> usize {
    chunk.code.len() * 5 + chunk.constants.len() * 8 + cache_count * std::mem::size_of::<InlineCache>()
}

fn zeroed_vec<T>(n: usize) -> Vec<T> {
    unsafe {
        let p = std::alloc::alloc_zeroed(std::alloc::Layout::array::<T>(n).unwrap());
        Vec::from_raw_parts(p as *mut T, n, n)
    }
}

unsafe fn free_object(o: *mut Obj) {
    unsafe fn free_typed<T>(p: *mut Obj) -> usize {
        unsafe {
            ptr::drop_in_place(p as *mut T);
            let size = std::mem::size_of::<T>();
            pool_free(p as *mut u8, size);
            size
        }
    }
    unsafe {
        let size = match (*o).ty {
            ObjType::String => {
                let size = ObjString::alloc_size((*(o as *mut ObjString)).len as usize);
                pool_free(o as *mut u8, size);
                size
            }
            ObjType::Function => {
                let p = o as *mut ObjFunction;
                let n = function_heap_size(&(*p).chunk, (*p).caches.len());
                n + free_typed::<ObjFunction>(o)
            }
            ObjType::Native => free_typed::<ObjNative>(o),
            ObjType::Closure => {
                let size = ObjClosure::alloc_size((*o).aux as usize);
                pool_free(o as *mut u8, size);
                size
            }
            ObjType::Upvalue => free_typed::<ObjUpvalue>(o),
            ObjType::Class => {
                let p = o as *mut ObjClass;
                let n = (*p).methods.heap_size();
                Shape::free_tree((*p).root_shape);
                n + free_typed::<ObjClass>(o)
            }
            ObjType::Instance => {
                let p = o as *mut ObjInstance;
                ObjInstance::free_external(p);
                let size = ObjInstance::alloc_size((*o).aux as usize);
                pool_free(o as *mut u8, size);
                size
            }
            ObjType::BoundMethod => free_typed::<ObjBoundMethod>(o),
        };
        track_free(size);
    }
}

fn function_name(f: *mut ObjFunction) -> String {
    unsafe {
        if (*f).name.is_null() {
            "<script>".to_string()
        } else {
            format!("<fn {}>", (*(*f).name).as_str())
        }
    }
}

pub fn value_to_string(v: Value) -> String {
    if v.is_number() {
        return format_number(v.as_number());
    }
    if v == Value::NIL {
        return "nil".into();
    }
    if v == Value::TRUE {
        return "true".into();
    }
    if v == Value::FALSE {
        return "false".into();
    }
    let o = v.as_obj();
    unsafe {
        match (*o).ty {
            ObjType::String => (*(o as *mut ObjString)).as_str().to_string(),
            ObjType::Function => function_name(o as *mut ObjFunction),
            ObjType::Native => "<native fn>".into(),
            ObjType::Closure => function_name((*(o as *mut ObjClosure)).function),
            ObjType::Upvalue => "upvalue".into(),
            ObjType::Class => (*(*(o as *mut ObjClass)).name).as_str().to_string(),
            ObjType::Instance => {
                format!("{} instance", (*(*(*(o as *mut ObjInstance)).class).name).as_str())
            }
            ObjType::BoundMethod => function_name((*(*(o as *mut ObjBoundMethod)).method).function),
        }
    }
}
