//! Size-class pool allocator for heap objects. Freed blocks go onto
//! per-class free lists and are reused directly, which keeps allocation
//! cheap and the heap compact. Large blocks fall back to the system allocator.

use std::alloc::{Layout, alloc, dealloc, handle_alloc_error};
use std::ptr;

const GRANULE_SHIFT: usize = 3;
const GRANULE: usize = 1 << GRANULE_SHIFT;
const NCLASSES: usize = 65; // block sizes up to 512 bytes
const CHUNK_SIZE: usize = 256 * 1024;

struct Pool {
    free: [*mut u8; NCLASSES],
    bump: *mut u8,
    end: *mut u8,
}

static mut POOL: Pool = Pool { free: [ptr::null_mut(); NCLASSES], bump: ptr::null_mut(), end: ptr::null_mut() };

#[inline(always)]
fn class_of(size: usize) -> usize {
    (size + GRANULE - 1) >> GRANULE_SHIFT
}

#[inline(always)]
fn large_layout(size: usize) -> Layout {
    Layout::from_size_align(size, GRANULE).unwrap()
}

#[inline(always)]
pub fn pool_alloc(size: usize) -> *mut u8 {
    let c = class_of(size).max(1);
    unsafe {
        if c >= NCLASSES {
            let l = large_layout(size);
            let p = alloc(l);
            if p.is_null() {
                handle_alloc_error(l);
            }
            return p;
        }
        let pool = &raw mut POOL;
        let head = (*pool).free[c];
        if !head.is_null() {
            (*pool).free[c] = *(head as *mut *mut u8);
            return head;
        }
        let sz = c << GRANULE_SHIFT;
        if ((*pool).end as usize) - ((*pool).bump as usize) < sz {
            refill(pool);
        }
        let p = (*pool).bump;
        (*pool).bump = p.add(sz);
        p
    }
}

#[cold]
unsafe fn refill(pool: *mut Pool) {
    let l = Layout::from_size_align(CHUNK_SIZE, GRANULE).unwrap();
    unsafe {
        let p = alloc(l);
        if p.is_null() {
            handle_alloc_error(l);
        }
        (*pool).bump = p;
        (*pool).end = p.add(CHUNK_SIZE);
    }
}

#[inline(always)]
pub unsafe fn pool_free(p: *mut u8, size: usize) {
    let c = class_of(size).max(1);
    unsafe {
        if c >= NCLASSES {
            dealloc(p, large_layout(size));
            return;
        }
        let pool = &raw mut POOL;
        *(p as *mut *mut u8) = (*pool).free[c];
        (*pool).free[c] = p;
    }
}
