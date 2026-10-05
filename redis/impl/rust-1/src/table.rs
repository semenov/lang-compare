//! Open-addressing (linear probing) hash table of 16-byte slots.
//! Each slot stores the object pointer, 32 bits of the key hash and an LRU stamp,
//! so probing and resizing never dereference objects except for key compares.

use crate::obj::Obj;

pub struct Slot {
    pub obj: Option<Obj>,
    pub h: u32,
    pub lru: u32,
}

pub struct Table {
    pub slots: Box<[Slot]>,
    pub len: usize,
}

fn new_slots(n: usize) -> Box<[Slot]> {
    (0..n).map(|_| Slot { obj: None, h: 0, lru: 0 }).collect::<Vec<_>>().into_boxed_slice()
}

impl Table {
    pub fn new() -> Table {
        Table { slots: Box::new([]), len: 0 }
    }

    #[inline]
    pub fn mask(&self) -> usize {
        self.slots.len().wrapping_sub(1)
    }

    #[inline]
    pub fn obj(&self, i: usize) -> &Obj {
        self.slots[i].obj.as_ref().unwrap()
    }

    #[inline]
    pub fn obj_mut(&mut self, i: usize) -> &mut Obj {
        self.slots[i].obj.as_mut().unwrap()
    }

    #[inline]
    pub fn find(&self, h: u32, key: &[u8]) -> Option<usize> {
        if self.len == 0 {
            return None;
        }
        let mask = self.mask();
        let mut i = h as usize & mask;
        loop {
            let s = unsafe { self.slots.get_unchecked(i) };
            match &s.obj {
                None => return None,
                Some(o) => {
                    if s.h == h && o.key() == key {
                        return Some(i);
                    }
                }
            }
            i = (i + 1) & mask;
        }
    }

    pub fn insert(&mut self, h: u32, obj: Obj, lru: u32) -> usize {
        if (self.len + 1) * 5 > self.slots.len() * 4 {
            let n = (self.slots.len() * 2).max(8);
            self.resize(n);
        }
        let i = self.place(h, obj, lru);
        self.len += 1;
        i
    }

    fn place(&mut self, h: u32, obj: Obj, lru: u32) -> usize {
        let mask = self.mask();
        let mut i = h as usize & mask;
        while self.slots[i].obj.is_some() {
            i = (i + 1) & mask;
        }
        self.slots[i] = Slot { obj: Some(obj), h, lru };
        i
    }

    fn resize(&mut self, n: usize) {
        let old = std::mem::replace(&mut self.slots, new_slots(n));
        for s in old.into_vec() {
            if let Some(o) = s.obj {
                self.place(s.h, o, s.lru);
            }
        }
    }

    /// Remove slot `idx` (must be occupied). Indices are invalidated.
    pub fn remove(&mut self, idx: usize) -> Obj {
        let obj = self.slots[idx].obj.take().unwrap();
        self.len -= 1;
        let mask = self.mask();
        let mut i = idx;
        let mut j = idx;
        loop {
            j = (j + 1) & mask;
            if self.slots[j].obj.is_none() {
                break;
            }
            let k = self.slots[j].h as usize & mask;
            let stays = if i <= j { i < k && k <= j } else { i < k || k <= j };
            if stays {
                continue;
            }
            self.slots.swap(i, j);
            i = j;
        }
        let cap = self.slots.len();
        if cap > 64 && self.len * 8 < cap {
            let n = (self.len * 2).next_power_of_two().max(8);
            self.resize(n);
        }
        obj
    }

    pub fn clear(&mut self) {
        self.slots = Box::new([]);
        self.len = 0;
    }
}
