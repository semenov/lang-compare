use crate::memory::{pool_alloc, pool_free};
use crate::value::Value;
use std::ptr;

pub static mut BYTES_ALLOCATED: usize = 0;

#[inline(always)]
pub fn track_alloc(n: usize) {
    unsafe { BYTES_ALLOCATED += n };
}
#[inline(always)]
pub fn track_free(n: usize) {
    unsafe { BYTES_ALLOCATED = BYTES_ALLOCATED.saturating_sub(n) };
}

#[derive(Clone, Copy, PartialEq, Eq, Debug)]
#[repr(u8)]
pub enum ObjType {
    String,
    Function,
    Native,
    Closure,
    Upvalue,
    Class,
    Instance,
    BoundMethod,
}

#[repr(C)]
pub struct Obj {
    pub ty: ObjType,
    pub marked: bool,
    /// Type-specific spare bits (instances: inline field capacity).
    pub aux: u32,
    pub next: *mut Obj,
}

/// String with its bytes stored inline after the struct.
#[repr(C)]
pub struct ObjString {
    pub obj: Obj,
    pub hash: u32,
    pub len: u32,
}

impl ObjString {
    #[inline(always)]
    pub fn as_str(&self) -> &str {
        unsafe {
            let p = (self as *const ObjString).add(1) as *const u8;
            std::str::from_utf8_unchecked(std::slice::from_raw_parts(p, self.len as usize))
        }
    }

    pub fn alloc_size(len: usize) -> usize {
        std::mem::size_of::<ObjString>() + len
    }
}

pub struct Chunk {
    pub code: Vec<u8>,
    pub lines: Vec<u32>,
    pub constants: Vec<Value>,
}

impl Chunk {
    pub fn new() -> Self {
        Chunk { code: Vec::new(), lines: Vec::new(), constants: Vec::new() }
    }
}

/// Per-site inline cache for property access and method invocation.
#[derive(Clone, Copy)]
pub struct InlineCache {
    /// Shape id the cache is valid for (0 = empty).
    pub shape_id: u32,
    /// Field slot, or NO_SLOT when the cached entry is a method.
    pub slot: u32,
    /// For SET_PROPERTY transitions: the shape after adding the field.
    pub next_shape: *mut Shape,
    pub method: Value,
}

pub const NO_SLOT: u32 = u32::MAX;

impl InlineCache {
    pub const EMPTY: InlineCache =
        InlineCache { shape_id: 0, slot: 0, next_shape: std::ptr::null_mut(), method: Value::NIL };
}

#[repr(C)]
pub struct ObjFunction {
    pub obj: Obj,
    pub arity: usize,
    pub upvalue_count: usize,
    pub chunk: Chunk,
    pub name: *mut ObjString,
    pub caches: Vec<InlineCache>,
}

pub type NativeFn = fn(&[Value]) -> Value;

#[repr(C)]
pub struct ObjNative {
    pub obj: Obj,
    pub function: NativeFn,
}

/// Closure with its upvalue pointers stored inline (count in `obj.aux`).
#[repr(C)]
pub struct ObjClosure {
    pub obj: Obj,
    pub function: *mut ObjFunction,
}

impl ObjClosure {
    #[inline(always)]
    pub unsafe fn upvalues(p: *mut ObjClosure) -> *mut *mut ObjUpvalue {
        unsafe { p.add(1) as *mut *mut ObjUpvalue }
    }

    pub fn alloc_size(count: usize) -> usize {
        std::mem::size_of::<ObjClosure>() + count * std::mem::size_of::<*mut ObjUpvalue>()
    }
}

#[repr(C)]
pub struct ObjUpvalue {
    pub obj: Obj,
    pub location: *mut Value,
    pub closed: Value,
    pub next: *mut ObjUpvalue,
}

#[repr(C)]
pub struct ObjClass {
    pub obj: Obj,
    pub name: *mut ObjString,
    pub methods: Table,
    pub initializer: Value,
    /// Root of this class's shape tree (owned).
    pub root_shape: *mut Shape,
    /// Largest field count seen on an instance; used to presize field storage.
    pub field_hint: usize,
}

/// Instance with fields stored inline after the struct (inline capacity in
/// `obj.aux`). The field count is implied by the shape. Fields that outgrow
/// the inline space move to a separate buffer whose first word holds its
/// capacity.
#[repr(C)]
pub struct ObjInstance {
    pub obj: Obj,
    pub class: *mut ObjClass,
    pub shape: *mut Shape,
    pub fields: *mut Value,
}

impl ObjInstance {
    pub fn alloc_size(inline_cap: usize) -> usize {
        std::mem::size_of::<ObjInstance>() + inline_cap * std::mem::size_of::<Value>()
    }

    #[inline(always)]
    pub unsafe fn inline_fields(p: *mut ObjInstance) -> *mut Value {
        unsafe { p.add(1) as *mut Value }
    }

    #[inline(always)]
    pub unsafe fn len(&self) -> usize {
        unsafe { (*self.shape).names.len() }
    }

    #[inline(always)]
    pub unsafe fn field(&self, slot: usize) -> Value {
        unsafe { *self.fields.add(slot) }
    }

    #[inline(always)]
    pub unsafe fn set_field(&mut self, slot: usize, v: Value) {
        unsafe { *self.fields.add(slot) = v }
    }

    /// Appends a field value at index `n` (the current field count).
    pub unsafe fn push_field(p: *mut ObjInstance, n: usize, v: Value) {
        unsafe {
            let inline = ObjInstance::inline_fields(p);
            let inst = &mut *p;
            let cap = if inst.fields == inline {
                inst.obj.aux as usize
            } else {
                (*inst.fields.sub(1)).0 as usize
            };
            if n == cap {
                let new_cap = (cap * 2).max(4);
                let buf = pool_alloc((new_cap + 1) * 8) as *mut Value;
                *buf = Value(new_cap as u64);
                ptr::copy_nonoverlapping(inst.fields, buf.add(1), n);
                ObjInstance::free_external(p);
                track_alloc((new_cap + 1) * 8);
                inst.fields = buf.add(1);
            }
            *inst.fields.add(n) = v;
        }
    }

    /// Frees the out-of-line field buffer, if any.
    pub unsafe fn free_external(p: *mut ObjInstance) {
        unsafe {
            let inst = &mut *p;
            if inst.fields != ObjInstance::inline_fields(p) {
                let buf = inst.fields.sub(1);
                let cap = (*buf).0 as usize;
                pool_free(buf as *mut u8, (cap + 1) * 8);
                track_free((cap + 1) * 8);
            }
        }
    }
}

static mut NEXT_SHAPE_ID: u32 = 1;

/// Hidden class: maps field names to slots. Shapes form a transition tree
/// rooted at each class, so a shape also identifies the instance's class.
/// Ids are never reused, which keeps inline caches valid after a class dies.
pub struct Shape {
    pub id: u32,
    pub names: Vec<*mut ObjString>,
    pub transitions: Vec<(*mut ObjString, *mut Shape)>,
}

impl Shape {
    pub fn new_root() -> *mut Shape {
        Shape::alloc(Vec::new())
    }

    fn alloc(names: Vec<*mut ObjString>) -> *mut Shape {
        let id = unsafe {
            let id = NEXT_SHAPE_ID;
            NEXT_SHAPE_ID = NEXT_SHAPE_ID.wrapping_add(1).max(1);
            id
        };
        track_alloc(std::mem::size_of::<Shape>() + names.len() * 8);
        Box::into_raw(Box::new(Shape { id, names, transitions: Vec::new() }))
    }

    #[inline(always)]
    pub fn find(&self, name: *mut ObjString) -> Option<usize> {
        self.names.iter().position(|&n| n == name)
    }

    pub fn transition(&mut self, name: *mut ObjString) -> *mut Shape {
        for &(n, s) in self.transitions.iter() {
            if n == name {
                return s;
            }
        }
        let mut names = Vec::with_capacity(self.names.len() + 1);
        names.extend_from_slice(&self.names);
        names.push(name);
        let s = Shape::alloc(names);
        self.transitions.push((name, s));
        s
    }

    /// Frees this shape and all its descendants.
    pub unsafe fn free_tree(root: *mut Shape) {
        let mut stack = vec![root];
        while let Some(s) = stack.pop() {
            let b = unsafe { Box::from_raw(s) };
            track_free(std::mem::size_of::<Shape>() + b.names.len() * 8);
            for &(_, c) in b.transitions.iter() {
                stack.push(c);
            }
        }
    }
}

#[repr(C)]
pub struct ObjBoundMethod {
    pub obj: Obj,
    pub receiver: Value,
    pub method: *mut ObjClosure,
}

pub fn hash_string(s: &[u8]) -> u32 {
    let mut hash: u32 = 2166136261;
    for &b in s {
        hash ^= b as u32;
        hash = hash.wrapping_mul(16777619);
    }
    hash
}

// ---------------------------------------------------------------------------
// Hash table keyed by interned strings.

#[derive(Clone, Copy)]
pub struct Entry {
    pub key: *mut ObjString,
    pub value: Value,
}

pub struct Table {
    pub count: usize,
    pub entries: Vec<Entry>,
}

const ENTRY_SIZE: usize = std::mem::size_of::<Entry>();

impl Table {
    pub const fn new() -> Self {
        Table { count: 0, entries: Vec::new() }
    }

    pub fn heap_size(&self) -> usize {
        self.entries.len() * ENTRY_SIZE
    }

    #[inline(always)]
    fn find_entry(entries: &[Entry], key: *mut ObjString) -> usize {
        let mask = entries.len() - 1;
        let mut index = unsafe { (*key).hash } as usize & mask;
        let mut tombstone: Option<usize> = None;
        loop {
            let e = unsafe { entries.get_unchecked(index) };
            if e.key.is_null() {
                if e.value.is_nil() {
                    return tombstone.unwrap_or(index);
                } else if tombstone.is_none() {
                    tombstone = Some(index);
                }
            } else if e.key == key {
                return index;
            }
            index = (index + 1) & mask;
        }
    }

    #[inline(always)]
    pub fn get(&self, key: *mut ObjString) -> Option<Value> {
        if self.count == 0 {
            return None;
        }
        let mask = self.entries.len() - 1;
        let mut index = unsafe { (*key).hash } as usize & mask;
        loop {
            let e = unsafe { self.entries.get_unchecked(index) };
            if e.key == key {
                return Some(e.value);
            }
            if e.key.is_null() && e.value.is_nil() {
                return None;
            }
            index = (index + 1) & mask;
        }
    }

    fn adjust_capacity(&mut self, capacity: usize) {
        let mut entries = vec![Entry { key: ptr::null_mut(), value: Value::NIL }; capacity];
        self.count = 0;
        for e in self.entries.iter() {
            if e.key.is_null() {
                continue;
            }
            let i = Table::find_entry(&entries, e.key);
            entries[i] = *e;
            self.count += 1;
        }
        track_free(self.heap_size());
        self.entries = entries;
        track_alloc(self.heap_size());
    }

    /// Returns true if the key was newly added.
    pub fn set(&mut self, key: *mut ObjString, value: Value) -> bool {
        if (self.count + 1) * 4 > self.entries.len() * 3 {
            let cap = if self.entries.len() < 8 { 8 } else { self.entries.len() * 2 };
            self.adjust_capacity(cap);
        }
        let i = Table::find_entry(&self.entries, key);
        let e = &mut self.entries[i];
        let is_new = e.key.is_null();
        if is_new && e.value.is_nil() {
            self.count += 1;
        }
        e.key = key;
        e.value = value;
        is_new
    }



    pub fn add_all(&self, to: &mut Table) {
        for e in self.entries.iter() {
            if !e.key.is_null() {
                to.set(e.key, e.value);
            }
        }
    }

    pub fn find_string(&self, chars: &str, hash: u32) -> *mut ObjString {
        if self.count == 0 {
            return ptr::null_mut();
        }
        let mask = self.entries.len() - 1;
        let mut index = hash as usize & mask;
        loop {
            let e = &self.entries[index];
            if e.key.is_null() {
                if e.value.is_nil() {
                    return ptr::null_mut();
                }
            } else {
                let k = unsafe { &*e.key };
                if k.hash == hash && k.as_str() == chars {
                    return e.key;
                }
            }
            index = (index + 1) & mask;
        }
    }
}
