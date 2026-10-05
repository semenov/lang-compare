//! Compact key/value object: one heap allocation holding header, optional
//! expiry, key bytes and value bytes (or a pointer to a list/hash container).
//!
//! Layout: [tag u8][klen u8][vlen u8] (SMALL) or [tag u8][klen u32][vlen u32],
//! then [expire u64 if EXPF][key][value]

use std::alloc::{Layout, alloc, dealloc, handle_alloc_error};
use std::collections::{HashMap, VecDeque};
use std::hash::BuildHasherDefault;
use std::ptr::{self, NonNull};

pub const STR: u8 = 0;
pub const LIST: u8 = 1;
pub const HASH: u8 = 2;
const KIND_MASK: u8 = 0x03;
const EXPF: u8 = 0x80;
const SMALL: u8 = 0x40;

pub type FastMap<K, V> = HashMap<K, V, BuildHasherDefault<ahash::AHasher>>;

#[derive(Default)]
pub struct ListV {
    pub items: VecDeque<Box<[u8]>>,
    pub cost: u64,
}

#[derive(Default)]
pub struct HashV {
    pub map: FastMap<Box<[u8]>, Box<[u8]>>,
    pub cost: u64,
}

pub struct Obj(NonNull<u8>);
unsafe impl Send for Obj {}
unsafe impl Sync for Obj {}

#[inline]
fn layout(size: usize) -> Layout {
    unsafe { Layout::from_size_align_unchecked(size.max(1), 8) }
}

impl Obj {
    #[inline]
    fn p(&self) -> *mut u8 {
        self.0.as_ptr()
    }
    #[inline]
    fn tag(&self) -> u8 {
        unsafe { *self.p() }
    }
    #[inline]
    fn small(&self) -> bool {
        self.tag() & SMALL != 0
    }
    #[inline]
    fn klen(&self) -> usize {
        unsafe {
            if self.small() {
                *self.p().add(1) as usize
            } else {
                ptr::read_unaligned(self.p().add(1) as *const u32) as usize
            }
        }
    }
    #[inline]
    fn vlen(&self) -> usize {
        unsafe {
            if self.small() {
                *self.p().add(2) as usize
            } else {
                ptr::read_unaligned(self.p().add(5) as *const u32) as usize
            }
        }
    }
    #[inline]
    fn hdr(&self) -> usize {
        if self.small() { 3 } else { 9 }
    }
    #[inline]
    pub fn kind(&self) -> u8 {
        self.tag() & KIND_MASK
    }
    #[inline]
    fn off(&self) -> usize {
        self.hdr() + if self.tag() & EXPF != 0 { 8 } else { 0 }
    }
    #[inline]
    pub fn exp(&self) -> u64 {
        if self.tag() & EXPF != 0 {
            unsafe { ptr::read_unaligned(self.p().add(self.hdr()) as *const u64) }
        } else {
            0
        }
    }
    #[inline]
    pub fn key(&self) -> &[u8] {
        unsafe { std::slice::from_raw_parts(self.p().add(self.off()), self.klen()) }
    }
    /// Raw value bytes (string contents; for containers the pointer bytes).
    #[inline]
    pub fn val(&self) -> &[u8] {
        unsafe { std::slice::from_raw_parts(self.p().add(self.off() + self.klen()), self.vlen()) }
    }
    #[inline]
    fn size(&self) -> usize {
        self.off() + self.klen() + self.vlen()
    }

    fn raw_new(kind: u8, key: &[u8], val: &[u8], exp: u64) -> Obj {
        let small = key.len() < 256 && val.len() < 256;
        let hdr = if small { 3 } else { 9 };
        let off = hdr + if exp != 0 { 8 } else { 0 };
        let size = off + key.len() + val.len();
        unsafe {
            let l = layout(size);
            let p = alloc(l);
            if p.is_null() {
                handle_alloc_error(l);
            }
            *p = kind | if exp != 0 { EXPF } else { 0 } | if small { SMALL } else { 0 };
            if small {
                *p.add(1) = key.len() as u8;
                *p.add(2) = val.len() as u8;
            } else {
                ptr::write_unaligned(p.add(1) as *mut u32, key.len() as u32);
                ptr::write_unaligned(p.add(5) as *mut u32, val.len() as u32);
            }
            if exp != 0 {
                ptr::write_unaligned(p.add(hdr) as *mut u64, exp);
            }
            ptr::copy_nonoverlapping(key.as_ptr(), p.add(off), key.len());
            ptr::copy_nonoverlapping(val.as_ptr(), p.add(off + key.len()), val.len());
            Obj(NonNull::new_unchecked(p))
        }
    }

    pub fn new_str(key: &[u8], val: &[u8], exp: u64) -> Obj {
        Self::raw_new(STR, key, val, exp)
    }
    pub fn new_list(key: &[u8]) -> Obj {
        let b = Box::into_raw(Box::new(ListV::default())) as usize;
        Self::raw_new(LIST, key, &b.to_ne_bytes(), 0)
    }
    pub fn new_hash(key: &[u8]) -> Obj {
        let b = Box::into_raw(Box::new(HashV::default())) as usize;
        Self::raw_new(HASH, key, &b.to_ne_bytes(), 0)
    }

    #[inline]
    fn cptr(&self) -> *mut u8 {
        unsafe { ptr::read_unaligned(self.p().add(self.off() + self.klen()) as *const usize) as *mut u8 }
    }
    #[inline]
    pub fn list(&self) -> &ListV {
        debug_assert_eq!(self.kind(), LIST);
        unsafe { &*(self.cptr() as *const ListV) }
    }
    #[inline]
    pub fn list_mut(&mut self) -> &mut ListV {
        debug_assert_eq!(self.kind(), LIST);
        unsafe { &mut *(self.cptr() as *mut ListV) }
    }
    #[inline]
    pub fn hash(&self) -> &HashV {
        debug_assert_eq!(self.kind(), HASH);
        unsafe { &*(self.cptr() as *const HashV) }
    }
    #[inline]
    pub fn hash_mut(&mut self) -> &mut HashV {
        debug_assert_eq!(self.kind(), HASH);
        unsafe { &mut *(self.cptr() as *mut HashV) }
    }

    /// Accounted memory of this key.
    #[inline]
    pub fn cost(&self) -> u64 {
        64 + self.klen() as u64
            + match self.kind() {
                STR => self.vlen() as u64,
                LIST => self.list().cost,
                _ => self.hash().cost,
            }
    }

    pub fn set_exp(&mut self, exp: u64) {
        let has = self.tag() & EXPF != 0;
        if has && exp != 0 {
            unsafe { ptr::write_unaligned(self.p().add(self.hdr()) as *mut u64, exp) };
            return;
        }
        if !has && exp == 0 {
            return;
        }
        let new = Self::raw_new(self.kind(), self.key(), self.val(), exp);
        let old = std::mem::replace(self, new);
        old.free_raw();
    }

    /// Replace with a string value (any previous type), with the given expiry.
    pub fn set_str(&mut self, val: &[u8], exp: u64) {
        if self.kind() == STR && self.vlen() == val.len() && ((self.tag() & EXPF != 0) == (exp != 0)) {
            unsafe {
                if exp != 0 {
                    ptr::write_unaligned(self.p().add(self.hdr()) as *mut u64, exp);
                }
                let off = self.off() + self.klen();
                ptr::copy_nonoverlapping(val.as_ptr(), self.p().add(off), val.len());
            }
            return;
        }
        let new = Self::raw_new(STR, self.key(), val, exp);
        *self = new;
    }

    /// Free the allocation without dropping a referenced container.
    fn free_raw(self) {
        let size = self.size();
        unsafe { dealloc(self.p(), layout(size)) };
        std::mem::forget(self);
    }
}

impl Drop for Obj {
    fn drop(&mut self) {
        unsafe {
            match self.kind() {
                LIST => drop(Box::from_raw(self.cptr() as *mut ListV)),
                HASH => drop(Box::from_raw(self.cptr() as *mut HashV)),
                _ => {}
            }
            dealloc(self.p(), layout(self.size()));
        }
    }
}
