mod cmd;
mod glob;
mod obj;
mod pubsub;
mod resp;
mod store;
mod table;

use bytes::{Buf, BytesMut};
use cmd::{CLIENTS, ConnState, exec};
use std::sync::atomic::Ordering::Relaxed;
use std::time::Duration;
use store::{MAXMEM, Store, now_ms};
use tokio::io::{AsyncReadExt, AsyncWriteExt};
use tokio::net::{TcpSocket, TcpStream};

#[global_allocator]
static GLOBAL: mimalloc::MiMalloc = mimalloc::MiMalloc;

const RBUF_MIN: usize = 4096;
const FLUSH_AT: usize = 256 * 1024;

fn env_num(name: &str) -> Option<u64> {
    std::env::var(name).ok().and_then(|v| v.trim().parse().ok())
}

fn raise_nofile() {
    unsafe {
        let mut r: libc::rlimit = std::mem::zeroed();
        if libc::getrlimit(libc::RLIMIT_NOFILE, &mut r) == 0 {
            for want in [r.rlim_max, 1 << 20, 65536, 10240] {
                let target = want.min(r.rlim_max);
                if target <= r.rlim_cur {
                    break;
                }
                let n = libc::rlimit { rlim_cur: target, rlim_max: r.rlim_max };
                if libc::setrlimit(libc::RLIMIT_NOFILE, &n) == 0 {
                    break;
                }
            }
        }
    }
}

fn main() {
    let port = env_num("PORT").unwrap_or(6380) as u16;
    let threads = env_num("THREADS")
        .filter(|&t| t > 0)
        .map(|t| t as usize)
        .unwrap_or_else(|| std::thread::available_parallelism().map(|n| n.get()).unwrap_or(1));
    MAXMEM.store(env_num("MAXMEMORY").unwrap_or(0), Relaxed);
    raise_nofile();
    now_ms();

    let st: &'static Store = Box::leak(Box::new(Store::new()));

    std::thread::Builder::new()
        .name("expire".into())
        .spawn(move || {
            loop {
                std::thread::sleep(Duration::from_millis(50));
                st.expire_cycle();
            }
        })
        .unwrap();

    let rt = tokio::runtime::Builder::new_multi_thread()
        .worker_threads(threads)
        .enable_io()
        .enable_time()
        .build()
        .unwrap();
    rt.block_on(async move {
        let sock = TcpSocket::new_v4().unwrap();
        sock.set_reuseaddr(true).unwrap();
        sock.bind(([0, 0, 0, 0], port).into()).expect("bind");
        let l = sock.listen(16384).unwrap();
        loop {
            match l.accept().await {
                Ok((s, _)) => {
                    tokio::spawn(conn(s, st));
                }
                Err(e) => {
                    eprintln!("accept: {e}");
                    tokio::time::sleep(Duration::from_millis(5)).await;
                }
            }
        }
    });
}

fn drain_pending(cs: &ConnState, w: &mut Vec<u8>) {
    if let Some(h) = &cs.handle {
        let mut b = h.buf.lock();
        if !b.is_empty() {
            if w.is_empty() {
                std::mem::swap(&mut *b, w);
            } else {
                w.extend_from_slice(&b);
                b.clear();
            }
        }
    }
}

async fn conn(mut sock: TcpStream, st: &'static Store) {
    CLIENTS.fetch_add(1, Relaxed);
    let _ = sock.set_nodelay(true);
    let mut cs = ConnState::default();
    let _ = serve(&mut sock, st, &mut cs).await;
    cs.cleanup();
    CLIENTS.fetch_sub(1, Relaxed);
}

async fn serve(sock: &mut TcpStream, st: &'static Store, cs: &mut ConnState) -> std::io::Result<()> {
    let mut rbuf = BytesMut::with_capacity(RBUF_MIN);
    let mut wbuf: Vec<u8> = Vec::new();
    loop {
        if rbuf.capacity() - rbuf.len() < 1024 {
            let c = rbuf.capacity().max(RBUF_MIN);
            rbuf.reserve(c);
        }
        let n = if let Some(h) = cs.handle.clone().filter(|_| !cs.subs.is_empty()) {
            tokio::select! {
                r = sock.read_buf(&mut rbuf) => r?,
                _ = h.notify.notified() => {
                    if h.dead.load(Relaxed) {
                        return Ok(());
                    }
                    drain_pending(cs, &mut wbuf);
                    if !wbuf.is_empty() {
                        sock.write_all(&wbuf).await?;
                        wbuf.clear();
                        shrink(&mut wbuf);
                    }
                    continue;
                }
            }
        } else {
            sock.read_buf(&mut rbuf).await?
        };
        if n == 0 {
            return Ok(());
        }

        drain_pending(cs, &mut wbuf);
        let now = now_ms();
        let mut pos = 0;
        let mut close = false;
        {
            let mut av: Vec<&[u8]> = Vec::with_capacity(8);
            let buf: &[u8] = &rbuf;
            loop {
                match resp::parse(&buf[pos..], &mut av) {
                    Ok(Some(used)) => {
                        pos += used;
                        if av.is_empty() {
                            continue;
                        }
                        if exec(st, &av, &mut wbuf, cs, now) {
                            close = true;
                            break;
                        }
                        if wbuf.len() >= FLUSH_AT {
                            sock.write_all(&wbuf).await?;
                            wbuf.clear();
                        }
                    }
                    Ok(None) => break,
                    Err(e) => {
                        resp::err(&mut wbuf, &format!("ERR Protocol error: {e}"));
                        close = true;
                        break;
                    }
                }
            }
        }
        rbuf.advance(pos);
        if rbuf.is_empty() && rbuf.capacity() > 64 * 1024 {
            rbuf = BytesMut::with_capacity(RBUF_MIN);
        }
        drain_pending(cs, &mut wbuf);
        if !wbuf.is_empty() {
            sock.write_all(&wbuf).await?;
            wbuf.clear();
            shrink(&mut wbuf);
        }
        if close {
            let _ = sock.shutdown().await;
            return Ok(());
        }
    }
}

fn shrink(w: &mut Vec<u8>) {
    if w.capacity() > 256 * 1024 {
        *w = Vec::new();
    }
}
