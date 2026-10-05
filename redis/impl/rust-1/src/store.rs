//! Sharded keyspace with expiry heaps, memory accounting and approximate LRU eviction.

use crate::obj::Obj;
use crate::table::Table;
use parking_lot::{Mutex, MutexGuard};
use std::cell::Cell;
use std::cmp::Reverse;
use std::collections::BinaryHeap;
use std::sync::LazyLock;
use std::sync::atomic::{AtomicI64, AtomicU64, AtomicUsize, Ordering::Relaxed};
use std::time::Instant;

pub const SHARD_BITS: u32 = 10;
pub const NSHARDS: usize = 1 << SHARD_BITS;

pub static MAXMEM: AtomicU64 = AtomicU64::new(0);
/// Global accounted memory; maintained only when MAXMEM > 0.
pub static USED: AtomicI64 = AtomicI64::new(0);

static HS: LazyLock<ahash::RandomState> = LazyLock::new(|| {
    ahash::RandomState::with_seeds(0x243f6a8885a308d3, 0x13198a2e03707344, 0xa4093822299f31d0, 0x082efa98ec4e6c89)
});
static START: LazyLock<Instant> = LazyLock::new(Instant::now);

/// Monotonic milliseconds (always >= 1, so 0 can mean "no expiry").
#[inline]
pub fn now_ms() -> u64 {
    START.elapsed().as_millis() as u64 + 1
}

#[inline]
pub fn maxmem() -> u64 {
    MAXMEM.load(Relaxed)
}

#[inline]
pub fn hk(key: &[u8]) -> (u32, usize) {
    let h = HS.hash_one(key);
    (h as u32, (h >> (64 - SHARD_BITS)) as usize)
}

thread_local! {
    static RNG: Cell<u64> = Cell::new({
        let x = &RNG as *const _ as u64;
        x ^ 0x9e3779b97f4a7c15 ^ (now_ms() << 17)
    });
}

fn rand() -> u64 {
    RNG.with(|r| {
        let mut x = r.get();
        x ^= x << 13;
        x ^= x >> 7;
        x ^= x << 17;
        r.set(x);
        x
    })
}

pub struct Shard {
    pub t: Table,
    pub used: u64,
    heap: BinaryHeap<Reverse<(u64, u32)>>,
    nexp: usize,
}

impl Shard {
    fn new() -> Shard {
        Shard { t: Table::new(), used: 0, heap: BinaryHeap::new(), nexp: 0 }
    }

    #[inline]
    fn acct(&mut self, d: i64) {
        self.used = (self.used as i64 + d) as u64;
        if maxmem() > 0 {
            USED.fetch_add(d, Relaxed);
        }
    }

    /// Find a live key; expired keys are deleted. Touches the LRU stamp.
    #[inline]
    pub fn lookup(&mut self, h: u32, key: &[u8], now: u64) -> Option<usize> {
        let i = self.t.find(h, key)?;
        let e = self.t.obj(i).exp();
        if e != 0 && e <= now {
            self.del(i);
            return None;
        }
        self.t.slots[i].lru = now as u32;
        Some(i)
    }

    pub fn del(&mut self, i: usize) {
        let o = self.t.remove(i);
        if o.exp() != 0 {
            self.nexp -= 1;
        }
        self.acct(-(o.cost() as i64));
        drop(o);
    }

    pub fn insert(&mut self, h: u32, o: Obj, now: u64) -> usize {
        let c = o.cost();
        let e = o.exp();
        self.acct(c as i64);
        let i = self.t.insert(h, o, now as u32);
        if e != 0 {
            self.nexp += 1;
            self.push_exp(e, h);
        }
        i
    }

    /// Mutate the object at `i`, keeping accounting and expiry bookkeeping right.
    /// Indices stay valid (no table changes).
    pub fn modify<R>(&mut self, i: usize, f: impl FnOnce(&mut Obj) -> R) -> R {
        let h = self.t.slots[i].h;
        let o = self.t.obj_mut(i);
        let c0 = o.cost();
        let e0 = o.exp();
        let r = f(o);
        let c1 = o.cost();
        let e1 = o.exp();
        if c1 != c0 {
            self.acct(c1 as i64 - c0 as i64);
        }
        if e0 != e1 {
            if e0 == 0 {
                self.nexp += 1;
            } else if e1 == 0 {
                self.nexp -= 1;
            }
            if e1 != 0 {
                self.push_exp(e1, h);
            }
        }
        r
    }

    fn push_exp(&mut self, e: u64, h: u32) {
        self.heap.push(Reverse((e, h)));
        if self.heap.len() > 2 * self.nexp + 1024 {
            let mut v = Vec::with_capacity(self.nexp);
            for s in self.t.slots.iter() {
                if let Some(o) = &s.obj {
                    let e = o.exp();
                    if e != 0 {
                        v.push(Reverse((e, s.h)));
                    }
                }
            }
            self.heap = BinaryHeap::from(v);
        }
    }

    /// Reclaim up to `budget` due expiry entries. Returns number processed.
    pub fn expire_some(&mut self, now: u64, budget: usize) -> usize {
        let mut n = 0;
        while n < budget {
            match self.heap.peek() {
                Some(&Reverse((e, h))) if e <= now => {
                    self.heap.pop();
                    self.purge_hash(h, now);
                    n += 1;
                }
                _ => break,
            }
        }
        if self.heap.is_empty() && self.heap.capacity() > 1024 {
            self.heap = BinaryHeap::new();
        }
        n
    }

    fn purge_hash(&mut self, h: u32, now: u64) {
        'outer: loop {
            if self.t.len == 0 {
                return;
            }
            let mask = self.t.mask();
            let mut i = h as usize & mask;
            loop {
                let s = &self.t.slots[i];
                match &s.obj {
                    None => return,
                    Some(o) => {
                        if s.h == h {
                            let e = o.exp();
                            if e != 0 && e <= now {
                                self.del(i);
                                continue 'outer;
                            }
                        }
                    }
                }
                i = (i + 1) & mask;
            }
        }
    }

    pub fn clear(&mut self) {
        let u = self.used;
        self.acct(-(u as i64));
        self.t.clear();
        self.heap = BinaryHeap::new();
        self.nexp = 0;
    }

    /// Evict the least recently used of a small sample of keys, skipping `protect`.
    fn evict_one(&mut self, protect: &[&[u8]], now: u64) -> bool {
        if self.t.len == 0 {
            return false;
        }
        let mask = self.t.mask();
        let start = rand() as usize & mask;
        let now32 = now as u32;
        let mut best: Option<usize> = None;
        let mut best_age = 0u32;
        let mut seen = 0;
        for step in 0..self.t.slots.len() {
            let i = (start + step) & mask;
            let s = &self.t.slots[i];
            if let Some(o) = &s.obj {
                seen += 1;
                if !protect.iter().any(|k| *k == o.key()) {
                    let e = o.exp();
                    let age = if e != 0 && e <= now { u32::MAX } else { now32.wrapping_sub(s.lru) };
                    if best.is_none() || age > best_age {
                        best = Some(i);
                        best_age = age;
                    }
                }
                if seen >= 16 {
                    break;
                }
            }
        }
        match best {
            Some(i) => {
                self.del(i);
                true
            }
            None => false,
        }
    }
}

#[repr(align(128))]
pub struct Padded<T>(pub T);

pub struct Store {
    pub shards: Box<[Padded<Mutex<Shard>>]>,
    cursor: AtomicUsize,
}

pub struct Multi<'a> {
    ids: Vec<usize>,
    gs: Vec<MutexGuard<'a, Shard>>,
}

impl<'a> Multi<'a> {
    #[inline]
    pub fn get(&mut self, si: usize) -> &mut Shard {
        let p = if self.ids.len() == 1 { 0 } else { self.ids.binary_search(&si).unwrap() };
        &mut self.gs[p]
    }
}

impl Store {
    pub fn new() -> Store {
        Store {
            shards: (0..NSHARDS).map(|_| Padded(Mutex::new(Shard::new()))).collect(),
            cursor: AtomicUsize::new(0),
        }
    }

    #[inline]
    pub fn lock(&self, si: usize) -> MutexGuard<'_, Shard> {
        self.shards[si].0.lock()
    }

    /// Lock all shards holding the given keys, in index order.
    pub fn lock_many<'k>(&self, keys: impl Iterator<Item = &'k [u8]>) -> Multi<'_> {
        let mut ids: Vec<usize> = keys.map(|k| hk(k).1).collect();
        ids.sort_unstable();
        ids.dedup();
        let gs = ids.iter().map(|&i| self.lock(i)).collect();
        Multi { ids, gs }
    }

    pub fn lock_all(&self) -> Vec<MutexGuard<'_, Shard>> {
        self.shards.iter().map(|s| s.0.lock()).collect()
    }

    pub fn used_memory(&self) -> u64 {
        self.shards.iter().map(|s| s.0.lock().used).sum()
    }

    pub fn dbsize(&self) -> usize {
        self.shards.iter().map(|s| s.0.lock().t.len).sum()
    }

    /// Evict keys until used memory is within the limit.
    pub fn evict(&self, protect: &[&[u8]]) {
        let max = maxmem();
        if max == 0 {
            return;
        }
        let now = now_ms();
        let mut fails = 0;
        while USED.load(Relaxed) > max as i64 {
            let si = self.cursor.fetch_add(1, Relaxed) % NSHARDS;
            let ok = self.lock(si).evict_one(protect, now);
            if ok {
                fails = 0;
            } else {
                fails += 1;
                if fails > NSHARDS {
                    break;
                }
            }
        }
    }

    /// Background reclamation of expired keys.
    pub fn expire_cycle(&self) {
        let now = now_ms();
        for s in self.shards.iter() {
            loop {
                let n = s.0.lock().expire_some(now, 256);
                if n < 256 {
                    break;
                }
            }
        }
    }
}
