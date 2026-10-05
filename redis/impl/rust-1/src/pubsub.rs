//! Pub/Sub registry. Publishers append encoded messages to each subscriber's
//! pending buffer and wake its connection task; they never block on sockets.

use parking_lot::{Mutex, RwLock};
use std::collections::HashMap;
use std::sync::atomic::{AtomicBool, AtomicU64, Ordering::Relaxed};
use std::sync::{Arc, LazyLock};
use tokio::sync::Notify;

const MAX_PENDING: usize = 32 * 1024 * 1024;

pub struct SubHandle {
    pub id: u64,
    pub buf: Mutex<Vec<u8>>,
    pub notify: Notify,
    pub dead: AtomicBool,
}

static NEXT_ID: AtomicU64 = AtomicU64::new(1);
static REG: LazyLock<RwLock<HashMap<Vec<u8>, Vec<Arc<SubHandle>>>>> = LazyLock::new(Default::default);

pub fn new_handle() -> Arc<SubHandle> {
    Arc::new(SubHandle {
        id: NEXT_ID.fetch_add(1, Relaxed),
        buf: Mutex::new(Vec::new()),
        notify: Notify::new(),
        dead: AtomicBool::new(false),
    })
}

pub fn subscribe(ch: &[u8], h: &Arc<SubHandle>) {
    REG.write().entry(ch.to_vec()).or_default().push(h.clone());
}

pub fn unsubscribe(ch: &[u8], id: u64) {
    let mut reg = REG.write();
    if let Some(v) = reg.get_mut(ch) {
        v.retain(|s| s.id != id);
        if v.is_empty() {
            reg.remove(ch);
        }
    }
}

pub fn publish(ch: &[u8], msg: &[u8]) -> i64 {
    let reg = REG.read();
    let Some(subs) = reg.get(ch) else { return 0 };
    let mut m = Vec::with_capacity(32 + ch.len() + msg.len());
    m.extend_from_slice(b"*3\r\n$7\r\nmessage\r\n");
    crate::resp::bulk(&mut m, ch);
    crate::resp::bulk(&mut m, msg);
    let mut n = 0;
    for s in subs {
        if s.dead.load(Relaxed) {
            continue;
        }
        {
            let mut b = s.buf.lock();
            if b.len() + m.len() > MAX_PENDING {
                s.dead.store(true, Relaxed);
                b.clear();
                b.shrink_to_fit();
            } else {
                b.extend_from_slice(&m);
            }
        }
        s.notify.notify_one();
        n += 1;
    }
    n
}
