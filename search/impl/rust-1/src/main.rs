#[global_allocator]
static GLOBAL: mimalloc::MiMalloc = mimalloc::MiMalloc;

mod index;
mod indexer;
mod text;

use bytes::Bytes;
use http_body_util::Full;
use hyper::body::Incoming;
use hyper::server::conn::http1;
use hyper::service::service_fn;
use hyper::{Request, Response, StatusCode};
use hyper_util::rt::TokioIo;
use index::Index;
use std::convert::Infallible;
use std::fmt::Write as _;
use std::path::Path;

fn main() {
    let args: Vec<String> = std::env::args().collect();
    match args.get(1).map(|s| s.as_str()) {
        Some("index") if args.len() == 4 => {
            if let Err(e) = indexer::build(Path::new(&args[2]), Path::new(&args[3])) {
                eprintln!("index failed: {e}");
                std::process::exit(1);
            }
        }
        Some("serve") if args.len() == 3 => serve(Path::new(&args[2])),
        Some("bench") if args.len() >= 4 => {
            let idx = Index::open(Path::new(&args[2])).unwrap();
            for q in &args[3..] {
                let t = std::time::Instant::now();
                let mut total = 0;
                for _ in 0..20 {
                    total = idx.search(q, 10).0;
                }
                eprintln!("{q:40} total {total:8} {:.3} ms", t.elapsed().as_secs_f64() * 1000.0 / 20.0);
            }
        }
        _ => {
            eprintln!("usage: search index <corpus.jsonl> <index-dir> | search serve <index-dir>");
            std::process::exit(2);
        }
    }
}

fn serve(dir: &Path) {
    let idx: &'static Index = Box::leak(Box::new(Index::open(dir).unwrap_or_else(|e| {
        eprintln!("cannot open index: {e}");
        std::process::exit(1);
    })));
    let port = std::env::var("PORT").ok().and_then(|p| p.parse::<u16>().ok()).unwrap_or(8080);
    let rt = tokio::runtime::Builder::new_multi_thread().enable_io().build().unwrap();
    rt.block_on(async move {
        let listener = tokio::net::TcpListener::bind(("0.0.0.0", port)).await.unwrap_or_else(|e| {
            eprintln!("bind failed: {e}");
            std::process::exit(1);
        });
        loop {
            let (stream, _) = match listener.accept().await {
                Ok(s) => s,
                Err(_) => continue,
            };
            let _ = stream.set_nodelay(true);
            tokio::spawn(async move {
                let svc = service_fn(move |req: Request<Incoming>| async move {
                    Ok::<_, Infallible>(handle(idx, &req))
                });
                let _ = http1::Builder::new().keep_alive(true).serve_connection(TokioIo::new(stream), svc).await;
            });
        }
    });
}

fn json(status: StatusCode, body: String) -> Response<Full<Bytes>> {
    Response::builder()
        .status(status)
        .header("Content-Type", "application/json")
        .body(Full::new(Bytes::from(body)))
        .unwrap()
}

fn error(status: StatusCode, msg: &str) -> Response<Full<Bytes>> {
    let mut s = String::from("{\"error\":");
    esc(&mut s, msg);
    s.push('}');
    json(status, s)
}

fn esc(out: &mut String, s: &str) {
    out.push('"');
    let mut last = 0;
    for (i, b) in s.bytes().enumerate() {
        let r: &str = match b {
            b'"' => "\\\"",
            b'\\' => "\\\\",
            b'\n' => "\\n",
            b'\r' => "\\r",
            b'\t' => "\\t",
            0..=0x1f => "",
            _ => continue,
        };
        out.push_str(&s[last..i]);
        if r.is_empty() {
            let _ = write!(out, "\\u{:04x}", b);
        } else {
            out.push_str(r);
        }
        last = i + 1;
    }
    out.push_str(&s[last..]);
    out.push('"');
}

fn hex(b: u8) -> Option<u8> {
    match b {
        b'0'..=b'9' => Some(b - b'0'),
        b'a'..=b'f' => Some(b - b'a' + 10),
        b'A'..=b'F' => Some(b - b'A' + 10),
        _ => None,
    }
}

fn url_decode(s: &str) -> String {
    let b = s.as_bytes();
    let mut out = Vec::with_capacity(b.len());
    let mut i = 0;
    while i < b.len() {
        match b[i] {
            b'+' => out.push(b' '),
            b'%' if i + 2 < b.len() && hex(b[i + 1]).is_some() && hex(b[i + 2]).is_some() => {
                out.push(hex(b[i + 1]).unwrap() * 16 + hex(b[i + 2]).unwrap());
                i += 2;
            }
            c => out.push(c),
        }
        i += 1;
    }
    String::from_utf8_lossy(&out).into_owned()
}

fn handle(idx: &Index, req: &Request<Incoming>) -> Response<Full<Bytes>> {
    let path = req.uri().path();
    if path == "/search" {
        let mut q = None;
        let mut k = None;
        for kv in req.uri().query().unwrap_or("").split('&') {
            let (key, v) = kv.split_once('=').unwrap_or((kv, ""));
            match url_decode(key).as_str() {
                "q" if q.is_none() => q = Some(url_decode(v)),
                "k" if k.is_none() => k = Some(url_decode(v)),
                _ => {}
            }
        }
        let Some(q) = q else { return error(StatusCode::BAD_REQUEST, "missing q") };
        let k = match k {
            None => 10,
            Some(k) => match k.parse::<usize>() {
                Ok(k) if (1..=1000).contains(&k) => k,
                _ => return error(StatusCode::BAD_REQUEST, "invalid k"),
            },
        };
        let (total, hits) = idx.search(&q, k);
        let mut s = String::with_capacity(64 + hits.len() * 64);
        let _ = write!(s, "{{\"total\":{total},\"hits\":[");
        for (i, h) in hits.iter().enumerate() {
            if i > 0 {
                s.push(',');
            }
            let _ = write!(s, "{{\"id\":{},\"title\":", h.id);
            esc(&mut s, idx.title(h.ord));
            let _ = write!(s, ",\"score\":{}}}", h.score);
        }
        s.push_str("]}");
        json(StatusCode::OK, s)
    } else if let Some(id) = path.strip_prefix("/doc/") {
        let doc = id.parse::<u32>().ok().and_then(|id| idx.doc(id).map(|d| (id, d)));
        match doc {
            Some((id, (title, text))) => {
                let mut s = String::with_capacity(text.len() + title.len() + 64);
                let _ = write!(s, "{{\"id\":{id},\"title\":");
                esc(&mut s, title);
                s.push_str(",\"text\":");
                esc(&mut s, &text);
                s.push('}');
                json(StatusCode::OK, s)
            }
            None => error(StatusCode::NOT_FOUND, "not found"),
        }
    } else if path == "/health" {
        json(StatusCode::OK, "{\"status\":\"ok\"}".into())
    } else {
        error(StatusCode::NOT_FOUND, "not found")
    }
}
