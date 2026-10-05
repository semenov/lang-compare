//! Command execution.

use crate::glob::glob_match;
use crate::obj::{HASH, LIST, Obj, STR};
use crate::pubsub::{self, SubHandle};
use crate::resp::*;
use crate::store::{Store, hk, maxmem};
use std::sync::Arc;
use std::sync::atomic::{AtomicUsize, Ordering::Relaxed};

pub static CLIENTS: AtomicUsize = AtomicUsize::new(0);

const WRONGTYPE: &str = "WRONGTYPE Operation against a key holding the wrong kind of value";
const NOTINT: &str = "ERR value is not an integer or out of range";
const OVERFLOW: &str = "ERR increment or decrement would overflow";
const SYNTAX: &str = "ERR syntax error";
const OOM: &str = "OOM command not allowed when used memory > 'maxmemory'.";

#[derive(Default)]
pub struct ConnState {
    pub subs: Vec<Vec<u8>>,
    pub handle: Option<Arc<SubHandle>>,
}

impl ConnState {
    pub fn cleanup(&mut self) {
        if let Some(h) = &self.handle {
            for ch in self.subs.drain(..) {
                pubsub::unsubscribe(&ch, h.id);
            }
        }
    }
}

/// Strict int64 parse (Redis string2ll semantics).
pub fn parse_i64(s: &[u8]) -> Option<i64> {
    if s.is_empty() || s.len() > 20 {
        return None;
    }
    if s == b"0" {
        return Some(0);
    }
    let (neg, d) = if s[0] == b'-' { (true, &s[1..]) } else { (false, s) };
    if d.is_empty() || !(b'1'..=b'9').contains(&d[0]) {
        return None;
    }
    let mut v: u64 = 0;
    for &c in d {
        if !c.is_ascii_digit() {
            return None;
        }
        v = v.checked_mul(10)?.checked_add((c - b'0') as u64)?;
    }
    if neg {
        if v > 1u64 << 63 {
            None
        } else {
            Some((v as i64).wrapping_neg())
        }
    } else if v > i64::MAX as u64 {
        None
    } else {
        Some(v as i64)
    }
}

#[inline]
fn oom_exceeds(cost: u64) -> bool {
    let m = maxmem();
    m > 0 && cost > m
}

fn wrong_args(o: &mut Vec<u8>, name: &str) {
    err(o, &format!("ERR wrong number of arguments for '{}' command", name));
}

/// Returns true to close the connection after flushing.
pub fn exec(st: &Store, a: &[&[u8]], o: &mut Vec<u8>, cs: &mut ConnState, now: u64) -> bool {
    let mut nb = [0u8; 20];
    let raw = a[0];
    if raw.len() > nb.len() {
        unknown(a, o);
        return false;
    }
    for (d, s) in nb.iter_mut().zip(raw) {
        *d = s.to_ascii_uppercase();
    }
    let name = &nb[..raw.len()];
    let n = a.len();

    macro_rules! arity {
        ($cond:expr, $lname:expr) => {
            if !($cond) {
                wrong_args(o, $lname);
                return false;
            }
        };
    }

    if !cs.subs.is_empty() {
        match name {
            b"SUBSCRIBE" | b"UNSUBSCRIBE" | b"PSUBSCRIBE" | b"PUNSUBSCRIBE" | b"PING" | b"QUIT" | b"RESET" => {}
            _ => {
                let lower = String::from_utf8_lossy(raw).to_lowercase();
                err(
                    o,
                    &format!(
                        "ERR Can't execute '{}': only (P|S)SUBSCRIBE / (P|S)UNSUBSCRIBE / PING / QUIT / RESET are allowed in this context",
                        lower
                    ),
                );
                return false;
            }
        }
    }

    match name {
        b"GET" => {
            arity!(n == 2, "get");
            get(st, a[1], o, now);
        }
        b"SET" => {
            arity!(n >= 3, "set");
            if set(st, a, o, now) {
                st.evict(&a[1..2]);
            }
        }
        b"MGET" => {
            arity!(n >= 2, "mget");
            mget(st, &a[1..], o, now);
        }
        b"MSET" => {
            arity!(n >= 3 && n % 2 == 1, "mset");
            if mset(st, &a[1..], o, now) && maxmem() > 0 {
                let keys: Vec<&[u8]> = a[1..].iter().step_by(2).copied().collect();
                st.evict(&keys);
            }
        }
        b"DEL" | b"UNLINK" => {
            arity!(n >= 2, if name == b"DEL" { "del" } else { "unlink" });
            let mut m = st.lock_many(a[1..].iter().copied());
            let mut cnt = 0;
            for k in &a[1..] {
                let (h, si) = hk(k);
                let s = m.get(si);
                if let Some(i) = s.lookup(h, k, now) {
                    s.del(i);
                    cnt += 1;
                }
            }
            int(o, cnt);
        }
        b"EXISTS" => {
            arity!(n >= 2, "exists");
            let mut m = st.lock_many(a[1..].iter().copied());
            let mut cnt = 0;
            for k in &a[1..] {
                let (h, si) = hk(k);
                if m.get(si).lookup(h, k, now).is_some() {
                    cnt += 1;
                }
            }
            int(o, cnt);
        }
        b"INCR" => {
            arity!(n == 2, "incr");
            incr(st, a[1], 1, o, now);
        }
        b"DECR" => {
            arity!(n == 2, "decr");
            incr(st, a[1], -1, o, now);
        }
        b"INCRBY" | b"DECRBY" => {
            let dec = name == b"DECRBY";
            arity!(n == 3, if dec { "decrby" } else { "incrby" });
            match parse_i64(a[2]) {
                None => err(o, NOTINT),
                Some(v) => {
                    if dec {
                        match v.checked_neg() {
                            Some(v) => incr(st, a[1], v, o, now),
                            None => err(o, "ERR decrement would overflow"),
                        }
                    } else {
                        incr(st, a[1], v, o, now)
                    }
                }
            }
        }
        b"APPEND" => {
            arity!(n == 3, "append");
            if append(st, a[1], a[2], o, now) {
                st.evict(&a[1..2]);
            }
        }
        b"STRLEN" => {
            arity!(n == 2, "strlen");
            let (h, si) = hk(a[1]);
            let mut s = st.lock(si);
            match s.lookup(h, a[1], now) {
                None => int(o, 0),
                Some(i) => {
                    let ob = s.t.obj(i);
                    if ob.kind() != STR { err(o, WRONGTYPE) } else { int(o, ob.val().len() as i64) }
                }
            }
        }
        b"TYPE" => {
            arity!(n == 2, "type");
            let (h, si) = hk(a[1]);
            let mut s = st.lock(si);
            match s.lookup(h, a[1], now) {
                None => simple(o, "none"),
                Some(i) => simple(
                    o,
                    match s.t.obj(i).kind() {
                        STR => "string",
                        LIST => "list",
                        _ => "hash",
                    },
                ),
            }
        }
        b"EXPIRE" | b"PEXPIRE" => {
            let ms = name == b"PEXPIRE";
            let lname = if ms { "pexpire" } else { "expire" };
            arity!(n >= 3, lname);
            if n > 3 {
                err(o, SYNTAX);
                return false;
            }
            expire(st, a[1], a[2], ms, lname, o, now);
        }
        b"TTL" | b"PTTL" => {
            let ms = name == b"PTTL";
            arity!(n == 2, if ms { "pttl" } else { "ttl" });
            let (h, si) = hk(a[1]);
            let mut s = st.lock(si);
            match s.lookup(h, a[1], now) {
                None => int(o, -2),
                Some(i) => {
                    let e = s.t.obj(i).exp();
                    if e == 0 {
                        int(o, -1)
                    } else {
                        let rem = e.saturating_sub(now) as i64;
                        int(o, if ms { rem } else { (rem + 500) / 1000 })
                    }
                }
            }
        }
        b"PERSIST" => {
            arity!(n == 2, "persist");
            let (h, si) = hk(a[1]);
            let mut s = st.lock(si);
            match s.lookup(h, a[1], now) {
                Some(i) if s.t.obj(i).exp() != 0 => {
                    s.modify(i, |ob| ob.set_exp(0));
                    int(o, 1)
                }
                _ => int(o, 0),
            }
        }
        b"KEYS" => {
            arity!(n == 2, "keys");
            keys(st, a[1], o, now);
        }
        b"DBSIZE" => {
            arity!(n == 1, "dbsize");
            int(o, st.dbsize() as i64);
        }
        b"FLUSHALL" | b"FLUSHDB" => {
            let mut gs = st.lock_all();
            for g in gs.iter_mut() {
                g.clear();
            }
            drop(gs);
            ok(o);
        }
        b"LPUSH" | b"RPUSH" => {
            let left = name == b"LPUSH";
            arity!(n >= 3, if left { "lpush" } else { "rpush" });
            if push(st, a, left, o, now) {
                st.evict(&a[1..2]);
            }
        }
        b"LPOP" | b"RPOP" => {
            let left = name == b"LPOP";
            arity!(n == 2 || n == 3, if left { "lpop" } else { "rpop" });
            pop(st, a, left, o, now);
        }
        b"LLEN" => {
            arity!(n == 2, "llen");
            let (h, si) = hk(a[1]);
            let mut s = st.lock(si);
            match s.lookup(h, a[1], now) {
                None => int(o, 0),
                Some(i) => {
                    let ob = s.t.obj(i);
                    if ob.kind() != LIST { err(o, WRONGTYPE) } else { int(o, ob.list().items.len() as i64) }
                }
            }
        }
        b"LRANGE" => {
            arity!(n == 4, "lrange");
            lrange(st, a, o, now);
        }
        b"LINDEX" => {
            arity!(n == 3, "lindex");
            let Some(idx) = parse_i64(a[2]) else {
                err(o, NOTINT);
                return false;
            };
            let (h, si) = hk(a[1]);
            let mut s = st.lock(si);
            match s.lookup(h, a[1], now) {
                None => null(o),
                Some(i) => {
                    let ob = s.t.obj(i);
                    if ob.kind() != LIST {
                        err(o, WRONGTYPE)
                    } else {
                        let l = &ob.list().items;
                        let len = l.len() as i64;
                        let j = if idx < 0 { idx + len } else { idx };
                        if j < 0 || j >= len { null(o) } else { bulk(o, &l[j as usize]) }
                    }
                }
            }
        }
        b"HSET" | b"HMSET" => {
            let hm = name == b"HMSET";
            arity!(n >= 4 && n % 2 == 0, if hm { "hmset" } else { "hset" });
            if hset(st, a, hm, o, now) {
                st.evict(&a[1..2]);
            }
        }
        b"HGET" => {
            arity!(n == 3, "hget");
            let (h, si) = hk(a[1]);
            let mut s = st.lock(si);
            match s.lookup(h, a[1], now) {
                None => null(o),
                Some(i) => {
                    let ob = s.t.obj(i);
                    if ob.kind() != HASH {
                        err(o, WRONGTYPE)
                    } else {
                        match ob.hash().map.get(a[2]) {
                            Some(v) => bulk(o, v),
                            None => null(o),
                        }
                    }
                }
            }
        }
        b"HMGET" => {
            arity!(n >= 3, "hmget");
            let (h, si) = hk(a[1]);
            let mut s = st.lock(si);
            match s.lookup(h, a[1], now) {
                None => {
                    arr(o, n - 2);
                    for _ in 2..n {
                        null(o);
                    }
                }
                Some(i) => {
                    let ob = s.t.obj(i);
                    if ob.kind() != HASH {
                        err(o, WRONGTYPE)
                    } else {
                        let m = &ob.hash().map;
                        arr(o, n - 2);
                        for f in &a[2..] {
                            match m.get(*f) {
                                Some(v) => bulk(o, v),
                                None => null(o),
                            }
                        }
                    }
                }
            }
        }
        b"HDEL" => {
            arity!(n >= 3, "hdel");
            let (h, si) = hk(a[1]);
            let mut s = st.lock(si);
            match s.lookup(h, a[1], now) {
                None => int(o, 0),
                Some(i) => {
                    if s.t.obj(i).kind() != HASH {
                        err(o, WRONGTYPE);
                    } else {
                        let (cnt, empty) = s.modify(i, |ob| {
                            let hv = ob.hash_mut();
                            let mut cnt = 0;
                            for f in &a[2..] {
                                if let Some((k, v)) = hv.map.remove_entry(*f) {
                                    hv.cost -= 32 + (k.len() + v.len()) as u64;
                                    cnt += 1;
                                }
                            }
                            (cnt, hv.map.is_empty())
                        });
                        if empty {
                            s.del(i);
                        }
                        int(o, cnt);
                    }
                }
            }
        }
        b"HGETALL" => {
            arity!(n == 2, "hgetall");
            let (h, si) = hk(a[1]);
            let mut s = st.lock(si);
            match s.lookup(h, a[1], now) {
                None => arr(o, 0),
                Some(i) => {
                    let ob = s.t.obj(i);
                    if ob.kind() != HASH {
                        err(o, WRONGTYPE)
                    } else {
                        let m = &ob.hash().map;
                        arr(o, m.len() * 2);
                        for (k, v) in m {
                            bulk(o, k);
                            bulk(o, v);
                        }
                    }
                }
            }
        }
        b"HLEN" | b"HEXISTS" => {
            let ex = name == b"HEXISTS";
            arity!(n == if ex { 3 } else { 2 }, if ex { "hexists" } else { "hlen" });
            let (h, si) = hk(a[1]);
            let mut s = st.lock(si);
            match s.lookup(h, a[1], now) {
                None => int(o, 0),
                Some(i) => {
                    let ob = s.t.obj(i);
                    if ob.kind() != HASH {
                        err(o, WRONGTYPE)
                    } else if ex {
                        int(o, ob.hash().map.contains_key(a[2]) as i64)
                    } else {
                        int(o, ob.hash().map.len() as i64)
                    }
                }
            }
        }
        b"HINCRBY" => {
            arity!(n == 4, "hincrby");
            if hincrby(st, a, o, now) {
                st.evict(&a[1..2]);
            }
        }
        b"PING" => {
            arity!(n <= 2, "ping");
            if !cs.subs.is_empty() {
                o.extend_from_slice(b"*2\r\n$4\r\npong\r\n");
                bulk(o, if n == 2 { a[1] } else { b"" });
            } else if n == 2 {
                bulk(o, a[1]);
            } else {
                o.extend_from_slice(b"+PONG\r\n");
            }
        }
        b"ECHO" => {
            arity!(n == 2, "echo");
            bulk(o, a[1]);
        }
        b"QUIT" => {
            ok(o);
            return true;
        }
        b"INFO" => {
            let s = format!(
                "# Server\r\nredis_version:8.0.0\r\nredis_mode:standalone\r\n\r\n# Clients\r\nconnected_clients:{}\r\n\r\n# Memory\r\nused_memory:{}\r\nmaxmemory:{}\r\nmaxmemory_policy:{}\r\n\r\n# Keyspace\r\ndb0:keys={},expires=0,avg_ttl=0\r\n",
                CLIENTS.load(Relaxed),
                st.used_memory(),
                maxmem(),
                if maxmem() > 0 { "allkeys-lru" } else { "noeviction" },
                st.dbsize()
            );
            bulk(o, s.as_bytes());
        }
        b"CONFIG" => {
            arity!(n >= 2, "config");
            if a[1].eq_ignore_ascii_case(b"SET") || a[1].eq_ignore_ascii_case(b"RESETSTAT") {
                ok(o);
            } else {
                arr(o, 0);
            }
        }
        b"COMMAND" => arr(o, 0),
        b"SELECT" => {
            arity!(n == 2, "select");
            if a[1] == b"0" { ok(o) } else { err(o, "ERR DB index is out of range") }
        }
        b"CLIENT" => ok(o),
        b"SUBSCRIBE" => {
            arity!(n >= 2, "subscribe");
            let h = cs.handle.get_or_insert_with(pubsub::new_handle).clone();
            for ch in &a[1..] {
                if !cs.subs.iter().any(|c| c == ch) {
                    cs.subs.push(ch.to_vec());
                    pubsub::subscribe(ch, &h);
                }
                o.extend_from_slice(b"*3\r\n$9\r\nsubscribe\r\n");
                bulk(o, ch);
                int(o, cs.subs.len() as i64);
            }
        }
        b"UNSUBSCRIBE" => {
            let id = cs.handle.as_ref().map(|h| h.id).unwrap_or(0);
            if n == 1 {
                if cs.subs.is_empty() {
                    o.extend_from_slice(b"*3\r\n$11\r\nunsubscribe\r\n$-1\r\n:0\r\n");
                }
                while let Some(ch) = cs.subs.pop() {
                    pubsub::unsubscribe(&ch, id);
                    o.extend_from_slice(b"*3\r\n$11\r\nunsubscribe\r\n");
                    bulk(o, &ch);
                    int(o, cs.subs.len() as i64);
                }
            } else {
                for ch in &a[1..] {
                    if let Some(p) = cs.subs.iter().position(|c| c == ch) {
                        cs.subs.remove(p);
                        pubsub::unsubscribe(ch, id);
                    }
                    o.extend_from_slice(b"*3\r\n$11\r\nunsubscribe\r\n");
                    bulk(o, ch);
                    int(o, cs.subs.len() as i64);
                }
            }
        }
        b"PUBLISH" => {
            arity!(n == 3, "publish");
            int(o, pubsub::publish(a[1], a[2]));
        }
        _ => unknown(a, o),
    }
    false
}

fn unknown(a: &[&[u8]], o: &mut Vec<u8>) {
    let mut s = format!("ERR unknown command '{}', with args beginning with: ", String::from_utf8_lossy(&a[0][..a[0].len().min(128)]));
    for x in &a[1..] {
        s.push_str(&format!("'{}' ", String::from_utf8_lossy(&x[..x.len().min(128)])));
    }
    let s: String = s.chars().map(|c| if c == '\r' || c == '\n' { ' ' } else { c }).collect();
    err(o, &s);
}

fn get(st: &Store, k: &[u8], o: &mut Vec<u8>, now: u64) {
    let (h, si) = hk(k);
    let mut s = st.lock(si);
    match s.lookup(h, k, now) {
        None => null(o),
        Some(i) => {
            let ob = s.t.obj(i);
            if ob.kind() != STR { err(o, WRONGTYPE) } else { bulk(o, ob.val()) }
        }
    }
}

/// Returns true if a write happened.
fn set(st: &Store, a: &[&[u8]], o: &mut Vec<u8>, now: u64) -> bool {
    let (k, v) = (a[1], a[2]);
    let (mut nx, mut xx, mut getf, mut keepttl) = (false, false, false, false);
    let mut exp: u64 = 0;
    let mut has_exp = false;
    let mut i = 3;
    while i < a.len() {
        let opt = a[i];
        if opt.eq_ignore_ascii_case(b"NX") && !xx {
            nx = true;
        } else if opt.eq_ignore_ascii_case(b"XX") && !nx {
            xx = true;
        } else if opt.eq_ignore_ascii_case(b"GET") {
            getf = true;
        } else if opt.eq_ignore_ascii_case(b"KEEPTTL") && !has_exp {
            keepttl = true;
        } else if (opt.eq_ignore_ascii_case(b"EX")
            || opt.eq_ignore_ascii_case(b"PX")
            || opt.eq_ignore_ascii_case(b"EXAT")
            || opt.eq_ignore_ascii_case(b"PXAT"))
            && !keepttl
            && !has_exp
            && i + 1 < a.len()
        {
            let Some(t) = parse_i64(a[i + 1]) else {
                err(o, NOTINT);
                return false;
            };
            if t <= 0 {
                err(o, "ERR invalid expire time in 'set' command");
                return false;
            }
            let up = opt.to_ascii_uppercase();
            let ms = match up.as_slice() {
                b"EX" | b"EXAT" => t.checked_mul(1000),
                _ => Some(t),
            };
            let Some(ms) = ms else {
                err(o, "ERR invalid expire time in 'set' command");
                return false;
            };
            exp = if up.ends_with(b"AT") {
                // absolute unix time -> monotonic clock
                let unix_now = std::time::SystemTime::now()
                    .duration_since(std::time::UNIX_EPOCH)
                    .map(|d| d.as_millis() as i64)
                    .unwrap_or(0);
                let rel = ms - unix_now;
                if rel <= 0 { 1 } else { now + rel as u64 }
            } else {
                match now.checked_add(ms as u64) {
                    Some(e) if e < (1u64 << 62) => e,
                    _ => {
                        err(o, "ERR invalid expire time in 'set' command");
                        return false;
                    }
                }
            };
            has_exp = true;
            i += 1;
        } else {
            err(o, SYNTAX);
            return false;
        }
        i += 1;
    }

    let (h, si) = hk(k);
    let mut s = st.lock(si);
    let idx = s.lookup(h, k, now);
    if getf {
        if let Some(i) = idx {
            if s.t.obj(i).kind() != STR {
                err(o, WRONGTYPE);
                return false;
            }
        }
    }
    if (nx && idx.is_some()) || (xx && idx.is_none()) {
        match (getf, idx) {
            (true, Some(i)) => bulk(o, s.t.obj(i).val()),
            _ => null(o),
        }
        return false;
    }
    if oom_exceeds(64 + (k.len() + v.len()) as u64) {
        err(o, OOM);
        return false;
    }
    match idx {
        Some(i) => {
            if getf {
                bulk(o, s.t.obj(i).val());
            } else {
                ok(o);
            }
            s.modify(i, |ob| {
                let e = if keepttl { ob.exp() } else { exp };
                ob.set_str(v, e)
            });
        }
        None => {
            if getf {
                null(o);
            } else {
                ok(o);
            }
            s.insert(h, Obj::new_str(k, v, exp), now);
        }
    }
    true
}

fn mget(st: &Store, keys: &[&[u8]], o: &mut Vec<u8>, now: u64) {
    let mut m = st.lock_many(keys.iter().copied());
    arr(o, keys.len());
    for k in keys {
        let (h, si) = hk(k);
        let s = m.get(si);
        match s.lookup(h, k, now) {
            Some(i) if s.t.obj(i).kind() == STR => bulk(o, s.t.obj(i).val()),
            _ => null(o),
        }
    }
}

fn mset(st: &Store, kv: &[&[u8]], o: &mut Vec<u8>, now: u64) -> bool {
    for p in kv.chunks(2) {
        if oom_exceeds(64 + (p[0].len() + p[1].len()) as u64) {
            err(o, OOM);
            return false;
        }
    }
    let mut m = st.lock_many(kv.iter().step_by(2).copied());
    for p in kv.chunks(2) {
        let (k, v) = (p[0], p[1]);
        let (h, si) = hk(k);
        let s = m.get(si);
        match s.lookup(h, k, now) {
            Some(i) => s.modify(i, |ob| ob.set_str(v, 0)),
            None => {
                s.insert(h, Obj::new_str(k, v, 0), now);
            }
        }
    }
    ok(o);
    true
}

fn incr(st: &Store, k: &[u8], d: i64, o: &mut Vec<u8>, now: u64) {
    let (h, si) = hk(k);
    let mut s = st.lock(si);
    match s.lookup(h, k, now) {
        None => {
            let mut b = itoa::Buffer::new();
            s.insert(h, Obj::new_str(k, b.format(d).as_bytes(), 0), now);
            int(o, d);
        }
        Some(i) => {
            let ob = s.t.obj(i);
            if ob.kind() != STR {
                err(o, WRONGTYPE);
                return;
            }
            let Some(cur) = parse_i64(ob.val()) else {
                err(o, NOTINT);
                return;
            };
            let Some(nv) = cur.checked_add(d) else {
                err(o, OVERFLOW);
                return;
            };
            let mut b = itoa::Buffer::new();
            let txt = b.format(nv).as_bytes();
            s.modify(i, |ob| {
                let e = ob.exp();
                ob.set_str(txt, e)
            });
            int(o, nv);
        }
    }
}

fn append(st: &Store, k: &[u8], v: &[u8], o: &mut Vec<u8>, now: u64) -> bool {
    let (h, si) = hk(k);
    let mut s = st.lock(si);
    match s.lookup(h, k, now) {
        None => {
            if oom_exceeds(64 + (k.len() + v.len()) as u64) {
                err(o, OOM);
                return false;
            }
            s.insert(h, Obj::new_str(k, v, 0), now);
            int(o, v.len() as i64);
        }
        Some(i) => {
            let ob = s.t.obj(i);
            if ob.kind() != STR {
                err(o, WRONGTYPE);
                return false;
            }
            if oom_exceeds(ob.cost() + v.len() as u64) {
                err(o, OOM);
                return false;
            }
            let len = s.modify(i, |ob| {
                let mut nv = Vec::with_capacity(ob.val().len() + v.len());
                nv.extend_from_slice(ob.val());
                nv.extend_from_slice(v);
                let e = ob.exp();
                ob.set_str(&nv, e);
                nv.len()
            });
            int(o, len as i64);
        }
    }
    true
}

fn expire(st: &Store, k: &[u8], t: &[u8], ms: bool, lname: &str, o: &mut Vec<u8>, now: u64) {
    let Some(t) = parse_i64(t) else {
        err(o, NOTINT);
        return;
    };
    let dur = if ms { Some(t) } else { t.checked_mul(1000) };
    let when = dur.and_then(|d| (now as i64).checked_add(d));
    let Some(when) = when.filter(|w| *w < (1i64 << 62)) else {
        err(o, &format!("ERR invalid expire time in '{}' command", lname));
        return;
    };
    let (h, si) = hk(k);
    let mut s = st.lock(si);
    match s.lookup(h, k, now) {
        None => int(o, 0),
        Some(i) => {
            if when <= now as i64 {
                s.del(i);
            } else {
                s.modify(i, |ob| ob.set_exp(when as u64));
            }
            int(o, 1);
        }
    }
}

fn keys(st: &Store, pat: &[u8], o: &mut Vec<u8>, now: u64) {
    let mut tmp = Vec::new();
    let mut cnt = 0;
    for sh in st.shards.iter() {
        let s = sh.0.lock();
        for slot in s.t.slots.iter() {
            if let Some(ob) = &slot.obj {
                let e = ob.exp();
                if e != 0 && e <= now {
                    continue;
                }
                if glob_match(pat, ob.key()) {
                    bulk(&mut tmp, ob.key());
                    cnt += 1;
                }
            }
        }
    }
    arr(o, cnt);
    o.extend_from_slice(&tmp);
}

fn push(st: &Store, a: &[&[u8]], left: bool, o: &mut Vec<u8>, now: u64) -> bool {
    let k = a[1];
    let (h, si) = hk(k);
    let mut s = st.lock(si);
    let idx = s.lookup(h, k, now);
    let add: u64 = a[2..].iter().map(|v| 16 + v.len() as u64).sum();
    let cur = match idx {
        Some(i) => {
            let ob = s.t.obj(i);
            if ob.kind() != LIST {
                err(o, WRONGTYPE);
                return false;
            }
            ob.cost()
        }
        None => 64 + k.len() as u64,
    };
    if oom_exceeds(cur + add) {
        err(o, OOM);
        return false;
    }
    let i = match idx {
        Some(i) => i,
        None => s.insert(h, Obj::new_list(k), now),
    };
    let len = s.modify(i, |ob| {
        let l = ob.list_mut();
        for v in &a[2..] {
            let b: Box<[u8]> = (*v).into();
            if left { l.items.push_front(b) } else { l.items.push_back(b) }
        }
        l.cost += add;
        l.items.len()
    });
    int(o, len as i64);
    true
}

fn pop(st: &Store, a: &[&[u8]], left: bool, o: &mut Vec<u8>, now: u64) {
    let cnt = if a.len() == 3 {
        match parse_i64(a[2]) {
            Some(c) if c >= 0 => Some(c as usize),
            _ => {
                err(o, "ERR value is out of range, must be positive");
                return;
            }
        }
    } else {
        None
    };
    let k = a[1];
    let (h, si) = hk(k);
    let mut s = st.lock(si);
    let Some(i) = s.lookup(h, k, now) else {
        if cnt.is_some() { null_arr(o) } else { null(o) }
        return;
    };
    if s.t.obj(i).kind() != LIST {
        err(o, WRONGTYPE);
        return;
    }
    let want = cnt.unwrap_or(1);
    let empty = s.modify(i, |ob| {
        let l = ob.list_mut();
        let n = want.min(l.items.len());
        if cnt.is_some() {
            arr(o, n);
        }
        for _ in 0..n {
            let e = if left { l.items.pop_front() } else { l.items.pop_back() }.unwrap();
            l.cost -= 16 + e.len() as u64;
            bulk(o, &e);
        }
        l.items.is_empty()
    });
    if empty {
        s.del(i);
    }
}

fn lrange(st: &Store, a: &[&[u8]], o: &mut Vec<u8>, now: u64) {
    let (Some(mut start), Some(mut stop)) = (parse_i64(a[2]), parse_i64(a[3])) else {
        err(o, NOTINT);
        return;
    };
    let k = a[1];
    let (h, si) = hk(k);
    let mut s = st.lock(si);
    let Some(i) = s.lookup(h, k, now) else {
        arr(o, 0);
        return;
    };
    let ob = s.t.obj(i);
    if ob.kind() != LIST {
        err(o, WRONGTYPE);
        return;
    }
    let l = &ob.list().items;
    let len = l.len() as i64;
    if start < 0 {
        start += len;
    }
    if stop < 0 {
        stop += len;
    }
    if start < 0 {
        start = 0;
    }
    if start > stop || start >= len {
        arr(o, 0);
        return;
    }
    if stop >= len {
        stop = len - 1;
    }
    arr(o, (stop - start + 1) as usize);
    for e in l.range(start as usize..=stop as usize) {
        bulk(o, e);
    }
}

fn hset(st: &Store, a: &[&[u8]], hm: bool, o: &mut Vec<u8>, now: u64) -> bool {
    let k = a[1];
    let (h, si) = hk(k);
    let mut s = st.lock(si);
    let idx = s.lookup(h, k, now);
    let add: u64 = a[2..].chunks(2).map(|p| 32 + (p[0].len() + p[1].len()) as u64).sum();
    let cur = match idx {
        Some(i) => {
            let ob = s.t.obj(i);
            if ob.kind() != HASH {
                err(o, WRONGTYPE);
                return false;
            }
            ob.cost()
        }
        None => 64 + k.len() as u64,
    };
    if oom_exceeds(cur + add) {
        err(o, OOM);
        return false;
    }
    let i = match idx {
        Some(i) => i,
        None => s.insert(h, Obj::new_hash(k), now),
    };
    let added = s.modify(i, |ob| {
        let hv = ob.hash_mut();
        let mut added = 0;
        for p in a[2..].chunks(2) {
            let (f, v) = (p[0], p[1]);
            match hv.map.get_mut(f) {
                Some(old) => {
                    hv.cost = hv.cost - old.len() as u64 + v.len() as u64;
                    *old = v.into();
                }
                None => {
                    hv.cost += 32 + (f.len() + v.len()) as u64;
                    hv.map.insert(f.into(), v.into());
                    added += 1;
                }
            }
        }
        added
    });
    if hm { ok(o) } else { int(o, added) }
    true
}

fn hincrby(st: &Store, a: &[&[u8]], o: &mut Vec<u8>, now: u64) -> bool {
    let Some(d) = parse_i64(a[3]) else {
        err(o, NOTINT);
        return false;
    };
    let (k, f) = (a[1], a[2]);
    let (h, si) = hk(k);
    let mut s = st.lock(si);
    let idx = s.lookup(h, k, now);
    let cur = match idx {
        Some(i) => {
            let ob = s.t.obj(i);
            if ob.kind() != HASH {
                err(o, WRONGTYPE);
                return false;
            }
            match ob.hash().map.get(f) {
                None => 0,
                Some(v) => match parse_i64(v) {
                    Some(x) => x,
                    None => {
                        err(o, "ERR hash value is not an integer");
                        return false;
                    }
                },
            }
        }
        None => 0,
    };
    let Some(nv) = cur.checked_add(d) else {
        err(o, OVERFLOW);
        return false;
    };
    let mut b = itoa::Buffer::new();
    let txt = b.format(nv).as_bytes();
    let base = match idx {
        Some(i) => s.t.obj(i).cost(),
        None => 64 + k.len() as u64,
    };
    if oom_exceeds(base + 32 + (f.len() + txt.len()) as u64) {
        err(o, OOM);
        return false;
    }
    let i = match idx {
        Some(i) => i,
        None => s.insert(h, Obj::new_hash(k), now),
    };
    s.modify(i, |ob| {
        let hv = ob.hash_mut();
        match hv.map.get_mut(f) {
            Some(old) => {
                hv.cost = hv.cost - old.len() as u64 + txt.len() as u64;
                *old = txt.into();
            }
            None => {
                hv.cost += 32 + (f.len() + txt.len()) as u64;
                hv.map.insert(f.into(), txt.into());
            }
        }
    });
    int(o, nv);
    true
}

