package main

import (
	"math"
	"strconv"
	"unsafe"
)

// Object kinds. Every heap object starts with an Obj header so that a Value's
// pointer can be inspected uniformly. nil/bool/undefined are represented by
// pointers to static sentinel headers; numbers have a nil pointer.
const (
	kNil uint8 = iota
	kTrue
	kFalse
	kUndef
	kString
	kFunction
	kNative
	kClosure
	kUpvalue
	kClass
	kInstance
	kBoundMethod
)

type Obj struct {
	kind uint8
}

// Value is a number when p == nil, otherwise p points to an Obj header.
type Value struct {
	n float64
	p unsafe.Pointer
}

var (
	tagNil   = Obj{kNil}
	tagTrue  = Obj{kTrue}
	tagFalse = Obj{kFalse}
	tagUndef = Obj{kUndef}

	nilP   = unsafe.Pointer(&tagNil)
	trueP  = unsafe.Pointer(&tagTrue)
	falseP = unsafe.Pointer(&tagFalse)
	undefP = unsafe.Pointer(&tagUndef)

	nilV   = Value{p: nilP}
	trueV  = Value{p: trueP}
	falseV = Value{p: falseP}
	undefV = Value{p: undefP}
)

func numV(f float64) Value { return Value{n: f} }

func boolV(b bool) Value {
	if b {
		return trueV
	}
	return falseV
}

func objV[T any](o *T) Value { return Value{p: unsafe.Pointer(o)} }

func (v Value) isNum() bool    { return v.p == nil }
func (v Value) kind() uint8    { return (*Obj)(v.p).kind }
func (v Value) isFalsey() bool { return v.p == nilP || v.p == falseP }

func (v Value) isObj(k uint8) bool { return v.p != nil && (*Obj)(v.p).kind == k }

func (v Value) asString() *ObjString     { return (*ObjString)(v.p) }
func (v Value) asFunction() *ObjFunction { return (*ObjFunction)(v.p) }
func (v Value) asClosure() *ObjClosure   { return (*ObjClosure)(v.p) }
func (v Value) asClass() *ObjClass       { return (*ObjClass)(v.p) }
func (v Value) asInstance() *ObjInstance { return (*ObjInstance)(v.p) }
func (v Value) asBound() *ObjBoundMethod { return (*ObjBoundMethod)(v.p) }
func (v Value) asNative() *ObjNative     { return (*ObjNative)(v.p) }

func valuesEqual(a, b Value) bool {
	if a.p == nil {
		return b.p == nil && a.n == b.n
	}
	if a.p == b.p {
		return true
	}
	if b.p != nil && a.kind() == kString && b.kind() == kString {
		return a.asString().s == b.asString().s
	}
	return false
}

type ObjString struct {
	Obj
	s string
}

type Chunk struct {
	code   []byte
	lines  []int32
	consts []Value
}

type ObjFunction struct {
	Obj
	arity        int
	upvalueCount int
	chunk        Chunk
	caches       []PropCache
	name         *ObjString
	// raw pointers into code/consts/caches, set once compilation finishes
	codep  unsafe.Pointer
	constp unsafe.Pointer
	cachep unsafe.Pointer
}

type ObjNative struct {
	Obj
	fn   func(args []Value) Value
	name string
}

type ObjUpvalue struct {
	Obj
	loc    *Value
	closed Value
	next   *ObjUpvalue
}

type PropCache struct {
	shape    *Shape
	newShape *Shape
	idx      int
	class    *ObjClass
	method   *ObjClosure
}

type ObjClosure struct {
	Obj
	fn       *ObjFunction
	upvalues []*ObjUpvalue
}

type ObjClass struct {
	Obj
	name      *ObjString
	methods   map[*ObjString]*ObjClosure
	init      *ObjClosure
	fieldHint int
}

type ObjInstance struct {
	Obj
	class  *ObjClass
	shape  *Shape
	fields []Value
}

type ObjBoundMethod struct {
	Obj
	receiver Value
	method   *ObjClosure
}

// Shape (hidden class) maps field names to slots in an instance's field array.
type Shape struct {
	keys  []*ObjString
	index map[*ObjString]int
	trans map[*ObjString]*Shape
}

var rootShape = &Shape{}

func (s *Shape) lookup(name *ObjString) int {
	if s.index != nil {
		if i, ok := s.index[name]; ok {
			return i
		}
		return -1
	}
	for i, k := range s.keys {
		if k == name {
			return i
		}
	}
	return -1
}

func (s *Shape) add(name *ObjString) *Shape {
	if t, ok := s.trans[name]; ok {
		return t
	}
	n := len(s.keys)
	keys := make([]*ObjString, n+1)
	copy(keys, s.keys)
	keys[n] = name
	ns := &Shape{keys: keys}
	if n+1 > 12 {
		ns.index = make(map[*ObjString]int, n+1)
		for i, k := range keys {
			ns.index[k] = i
		}
	}
	if s.trans == nil {
		s.trans = make(map[*ObjString]*Shape)
	}
	s.trans[name] = ns
	return ns
}

func formatNumber(n float64) string {
	if math.IsNaN(n) {
		return "nan"
	}
	if math.IsInf(n, 1) {
		return "inf"
	}
	if math.IsInf(n, -1) {
		return "-inf"
	}
	if n == math.Trunc(n) && math.Abs(n) < 1e6 {
		if n == 0 && math.Signbit(n) {
			return "-0"
		}
		return strconv.FormatInt(int64(n), 10)
	}
	return strconv.FormatFloat(n, 'g', 6, 64)
}

func formatValue(v Value) string {
	if v.p == nil {
		return formatNumber(v.n)
	}
	switch v.kind() {
	case kNil:
		return "nil"
	case kTrue:
		return "true"
	case kFalse:
		return "false"
	case kString:
		return v.asString().s
	case kFunction:
		return fnName(v.asFunction())
	case kClosure:
		return fnName(v.asClosure().fn)
	case kNative:
		return "<native fn>"
	case kClass:
		return v.asClass().name.s
	case kInstance:
		return v.asInstance().class.name.s + " instance"
	case kBoundMethod:
		return fnName(v.asBound().method.fn)
	}
	return "?"
}

func fnName(f *ObjFunction) string {
	if f.name == nil {
		return "<script>"
	}
	return "<fn " + f.name.s + ">"
}
