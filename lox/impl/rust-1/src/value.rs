use crate::object::{Obj, ObjString, ObjType};

/// NaN-boxed value: numbers are stored as raw f64 bits; everything else lives
/// inside the quiet-NaN space.
#[derive(Clone, Copy, PartialEq, Eq)]
#[repr(transparent)]
pub struct Value(pub u64);

const SIGN_BIT: u64 = 0x8000_0000_0000_0000;
const QNAN: u64 = 0x7ffc_0000_0000_0000;
const TAG_NIL: u64 = 1;
const TAG_FALSE: u64 = 2;
const TAG_TRUE: u64 = 3;
const TAG_UNDEF: u64 = 4;

impl Value {
    pub const NIL: Value = Value(QNAN | TAG_NIL);
    pub const FALSE: Value = Value(QNAN | TAG_FALSE);
    pub const TRUE: Value = Value(QNAN | TAG_TRUE);
    /// Marks an undefined global slot; never visible to programs.
    pub const UNDEF: Value = Value(QNAN | TAG_UNDEF);

    #[inline(always)]
    pub fn number(n: f64) -> Value {
        Value(n.to_bits())
    }
    #[inline(always)]
    pub fn bool(b: bool) -> Value {
        if b { Value::TRUE } else { Value::FALSE }
    }
    #[inline(always)]
    pub fn obj<T>(p: *mut T) -> Value {
        Value(SIGN_BIT | QNAN | (p as usize as u64))
    }
    #[inline(always)]
    pub fn is_number(self) -> bool {
        (self.0 & QNAN) != QNAN
    }
    #[inline(always)]
    pub fn as_number(self) -> f64 {
        f64::from_bits(self.0)
    }
    #[inline(always)]
    pub fn is_obj(self) -> bool {
        (self.0 & (QNAN | SIGN_BIT)) == (QNAN | SIGN_BIT)
    }
    #[inline(always)]
    pub fn as_obj(self) -> *mut Obj {
        (self.0 & !(SIGN_BIT | QNAN)) as usize as *mut Obj
    }
    #[inline(always)]
    pub fn is_nil(self) -> bool {
        self == Value::NIL
    }
    #[inline(always)]
    pub fn is_falsey(self) -> bool {
        self == Value::NIL || self == Value::FALSE
    }
    #[inline(always)]
    pub fn is_obj_type(self, t: ObjType) -> bool {
        self.is_obj() && unsafe { (*self.as_obj()).ty == t }
    }
    #[inline(always)]
    pub fn as_string(self) -> *mut ObjString {
        self.as_obj() as *mut ObjString
    }

    #[inline(always)]
    pub fn equals(self, other: Value) -> bool {
        if self.is_number() && other.is_number() {
            self.as_number() == other.as_number()
        } else {
            self.0 == other.0
        }
    }
}

pub fn format_number(n: f64) -> String {
    if n.is_nan() {
        return if n.is_sign_negative() { "-nan".into() } else { "nan".into() };
    }
    if n.is_infinite() {
        return if n > 0.0 { "inf".into() } else { "-inf".into() };
    }
    if n == 0.0 {
        return if n.is_sign_negative() { "-0".into() } else { "0".into() };
    }
    if n.fract() == 0.0 && n.abs() < 1e6 {
        return format!("{}", n as i64);
    }
    // Emulate printf("%g") with precision 6.
    let e = format!("{:.5e}", n);
    let epos = e.find('e').unwrap();
    let exp: i32 = e[epos + 1..].parse().unwrap();
    if exp < -4 || exp >= 6 {
        let mut mant = e[..epos].to_string();
        if mant.contains('.') {
            while mant.ends_with('0') {
                mant.pop();
            }
            if mant.ends_with('.') {
                mant.pop();
            }
        }
        let sign = if exp < 0 { '-' } else { '+' };
        format!("{}e{}{:02}", mant, sign, exp.abs())
    } else {
        let prec = (5 - exp) as usize;
        let mut s = format!("{:.*}", prec, n);
        if s.contains('.') {
            while s.ends_with('0') {
                s.pop();
            }
            if s.ends_with('.') {
                s.pop();
            }
        }
        s
    }
}
