//! RESP2 request parsing and reply encoding.

pub type ProtoErr = &'static str;

const MAX_BULK: i64 = 512 * 1024 * 1024;
const MAX_MULTIBULK: i64 = 1024 * 1024;
const MAX_INLINE: usize = 64 * 1024;

/// Parse an integer terminated by CRLF starting at `pos`.
/// Ok(None) = incomplete. Returns (value, position after CRLF).
#[inline]
fn int_line(buf: &[u8], mut pos: usize, err: ProtoErr) -> Result<Option<(i64, usize)>, ProtoErr> {
    let start = pos;
    let mut neg = false;
    let mut v: i64 = 0;
    if pos < buf.len() && buf[pos] == b'-' {
        neg = true;
        pos += 1;
    }
    loop {
        if pos >= buf.len() {
            return if pos - start > 20 { Err(err) } else { Ok(None) };
        }
        let c = buf[pos];
        if c == b'\r' {
            if pos + 1 >= buf.len() {
                return Ok(None);
            }
            if buf[pos + 1] != b'\n' || pos == start || (neg && pos == start + 1) {
                return Err(err);
            }
            return Ok(Some((if neg { -v } else { v }, pos + 2)));
        }
        if !c.is_ascii_digit() || pos - start > 17 {
            return Err(err);
        }
        v = v * 10 + (c - b'0') as i64;
        pos += 1;
    }
}

/// Parse one command from `buf` into `argv`. Ok(None) = need more data.
/// Ok(Some(n)) = consumed n bytes (argv may be empty for empty requests).
pub fn parse<'a>(buf: &'a [u8], argv: &mut Vec<&'a [u8]>) -> Result<Option<usize>, ProtoErr> {
    argv.clear();
    if buf.is_empty() {
        return Ok(None);
    }
    if buf[0] != b'*' {
        return parse_inline(buf, argv);
    }
    let (n, mut pos) = match int_line(buf, 1, "invalid multibulk length")? {
        None => return Ok(None),
        Some(x) => x,
    };
    if n > MAX_MULTIBULK {
        return Err("invalid multibulk length");
    }
    if n <= 0 {
        return Ok(Some(pos));
    }
    for _ in 0..n {
        if pos >= buf.len() {
            return Ok(None);
        }
        if buf[pos] != b'$' {
            return Err("expected '$'");
        }
        let (bl, p2) = match int_line(buf, pos + 1, "invalid bulk length")? {
            None => return Ok(None),
            Some(x) => x,
        };
        if !(0..=MAX_BULK).contains(&bl) {
            return Err("invalid bulk length");
        }
        let end = p2 + bl as usize;
        if end + 2 > buf.len() {
            return Ok(None);
        }
        if buf[end] != b'\r' || buf[end + 1] != b'\n' {
            return Err("invalid bulk format");
        }
        argv.push(&buf[p2..end]);
        pos = end + 2;
    }
    Ok(Some(pos))
}

fn parse_inline<'a>(buf: &'a [u8], argv: &mut Vec<&'a [u8]>) -> Result<Option<usize>, ProtoErr> {
    let nl = match buf.iter().position(|&c| c == b'\n') {
        None => {
            return if buf.len() > MAX_INLINE { Err("too big inline request") } else { Ok(None) };
        }
        Some(p) => p,
    };
    let mut line = &buf[..nl];
    if line.last() == Some(&b'\r') {
        line = &line[..line.len() - 1];
    }
    for part in line.split(|&c| c == b' ' || c == b'\t') {
        if !part.is_empty() {
            argv.push(part);
        }
    }
    Ok(Some(nl + 1))
}

// ---------------------------------------------------------------- replies

#[inline]
pub fn simple(o: &mut Vec<u8>, s: &str) {
    o.push(b'+');
    o.extend_from_slice(s.as_bytes());
    o.extend_from_slice(b"\r\n");
}
#[inline]
pub fn ok(o: &mut Vec<u8>) {
    o.extend_from_slice(b"+OK\r\n");
}
#[inline]
pub fn err(o: &mut Vec<u8>, s: &str) {
    o.push(b'-');
    o.extend_from_slice(s.as_bytes());
    o.extend_from_slice(b"\r\n");
}
#[inline]
pub fn int(o: &mut Vec<u8>, v: i64) {
    o.push(b':');
    o.extend_from_slice(itoa::Buffer::new().format(v).as_bytes());
    o.extend_from_slice(b"\r\n");
}
#[inline]
pub fn bulk(o: &mut Vec<u8>, b: &[u8]) {
    o.push(b'$');
    o.extend_from_slice(itoa::Buffer::new().format(b.len()).as_bytes());
    o.extend_from_slice(b"\r\n");
    o.extend_from_slice(b);
    o.extend_from_slice(b"\r\n");
}
#[inline]
pub fn null(o: &mut Vec<u8>) {
    o.extend_from_slice(b"$-1\r\n");
}
#[inline]
pub fn null_arr(o: &mut Vec<u8>) {
    o.extend_from_slice(b"*-1\r\n");
}
#[inline]
pub fn arr(o: &mut Vec<u8>, n: usize) {
    o.push(b'*');
    o.extend_from_slice(itoa::Buffer::new().format(n).as_bytes());
    o.extend_from_slice(b"\r\n");
}
