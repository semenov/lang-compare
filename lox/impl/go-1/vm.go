package main

import (
	"bufio"
	"fmt"
	"os"
	"time"
	"unsafe"
)

const (
	FramesMax = 1024
	StackMax  = FramesMax * 64
	// headroom required at call time: max locals plus temporaries
	frameReserve = 512

	vsize = int(unsafe.Sizeof(Value{}))
)

type CallFrame struct {
	closure *ObjClosure
	ip      unsafe.Pointer // next instruction
	bp      unsafe.Pointer // slot 0 of the frame
}

type VM struct {
	stack        []Value
	stackLimit   uintptr // calls with a base above this overflow
	frames       []CallFrame
	frameCount   int
	openUpvalues *ObjUpvalue

	strings     map[string]*ObjString
	globalIdx   map[string]int
	globals     []Value
	globalNames []string

	initString *ObjString
	out        *bufio.Writer
}

var startTime = time.Now()

func newVM() *VM {
	vm := &VM{
		stack:     make([]Value, StackMax),
		frames:    make([]CallFrame, FramesMax),
		strings:   make(map[string]*ObjString),
		globalIdx: make(map[string]int),
		out:       bufio.NewWriterSize(os.Stdout, 1<<16),
	}
	vm.stackLimit = uintptr(unsafe.Pointer(&vm.stack[StackMax-frameReserve]))
	vm.initString = vm.intern("init")
	vm.defineNative("clock", func(args []Value) Value {
		return numV(time.Since(startTime).Seconds())
	})
	return vm
}

func (vm *VM) intern(s string) *ObjString {
	if o, ok := vm.strings[s]; ok {
		return o
	}
	o := &ObjString{Obj: Obj{kString}, s: s}
	vm.strings[s] = o
	return o
}

func (vm *VM) globalSlot(name string) int {
	if i, ok := vm.globalIdx[name]; ok {
		return i
	}
	i := len(vm.globals)
	vm.globalIdx[name] = i
	vm.globals = append(vm.globals, undefV)
	vm.globalNames = append(vm.globalNames, name)
	return i
}

func (vm *VM) defineNative(name string, fn func([]Value) Value) {
	vm.globals[vm.globalSlot(name)] = objV(&ObjNative{Obj: Obj{kNative}, fn: fn, name: name})
}

func (vm *VM) interpret(src string) int {
	fn := compile(vm, src)
	if fn == nil {
		return 65
	}
	cl := &ObjClosure{Obj: Obj{kClosure}, fn: fn}
	vm.stack[0] = objV(cl)
	vm.frames[0] = CallFrame{closure: cl, ip: fn.codep, bp: unsafe.Pointer(&vm.stack[0])}
	vm.frameCount = 1
	if !vm.run(unsafe.Pointer(&vm.stack[1])) {
		return 70
	}
	return 0
}

func (vm *VM) runtimeError(format string, args ...any) {
	vm.out.Flush()
	fmt.Fprintf(os.Stderr, format+"\n", args...)
	for i := vm.frameCount - 1; i >= 0; i-- {
		f := &vm.frames[i]
		fn := f.closure.fn
		off := int(uintptr(f.ip) - uintptr(fn.codep))
		line := fn.chunk.lines[off-1]
		if fn.name == nil {
			fmt.Fprintf(os.Stderr, "[line %d] in script\n", line)
		} else {
			fmt.Fprintf(os.Stderr, "[line %d] in %s()\n", line, fn.name.s)
		}
	}
	vm.openUpvalues = nil
}

func (vm *VM) captureUpvalue(slot *Value) *ObjUpvalue {
	var prev *ObjUpvalue
	u := vm.openUpvalues
	for u != nil && uintptr(unsafe.Pointer(u.loc)) > uintptr(unsafe.Pointer(slot)) {
		prev = u
		u = u.next
	}
	if u != nil && u.loc == slot {
		return u
	}
	nu := &ObjUpvalue{Obj: Obj{kUpvalue}, loc: slot, next: u}
	if prev == nil {
		vm.openUpvalues = nu
	} else {
		prev.next = nu
	}
	return nu
}

func (vm *VM) closeUpvalues(last unsafe.Pointer) {
	for vm.openUpvalues != nil && uintptr(unsafe.Pointer(vm.openUpvalues.loc)) >= uintptr(last) {
		u := vm.openUpvalues
		u.closed = *u.loc
		u.loc = &u.closed
		vm.openUpvalues = u.next
	}
}

// pushFrame sets up a call to closure cl whose callee slot is at bp.
func (vm *VM) pushFrame(cl *ObjClosure, argc int, bp unsafe.Pointer) bool {
	if argc != cl.fn.arity {
		vm.runtimeError("Expected %d arguments but got %d.", cl.fn.arity, argc)
		return false
	}
	if vm.frameCount == FramesMax || uintptr(bp) > vm.stackLimit {
		vm.runtimeError("Stack overflow.")
		return false
	}
	f := &vm.frames[vm.frameCount]
	vm.frameCount++
	f.closure = cl
	f.ip = cl.fn.codep
	f.bp = bp
	return true
}

// callValue performs a call of callee with argc arguments below sp. It
// returns the new stack top; the caller reloads the current frame.
func (vm *VM) callValue(callee Value, argc int, sp unsafe.Pointer) (unsafe.Pointer, bool) {
	bp := unsafe.Add(sp, -(argc+1)*vsize)
	if callee.p != nil {
		switch callee.kind() {
		case kClosure:
			return sp, vm.pushFrame(callee.asClosure(), argc, bp)
		case kBoundMethod:
			b := callee.asBound()
			*(*Value)(bp) = b.receiver
			return sp, vm.pushFrame(b.method, argc, bp)
		case kClass:
			c := callee.asClass()
			*(*Value)(bp) = objV(newInstance(c))
			if c.init != nil {
				return sp, vm.pushFrame(c.init, argc, bp)
			}
			if argc != 0 {
				vm.runtimeError("Expected 0 arguments but got %d.", argc)
				return sp, false
			}
			return unsafe.Add(bp, vsize), true
		case kNative:
			n := callee.asNative()
			args := unsafe.Slice((*Value)(unsafe.Add(bp, vsize)), argc)
			*(*Value)(bp) = n.fn(args)
			return unsafe.Add(bp, vsize), true
		}
	}
	vm.runtimeError("Can only call functions and classes.")
	return sp, false
}

func slot(bp unsafe.Pointer, i byte) *Value { return (*Value)(unsafe.Add(bp, int(i)*vsize)) }

// top returns the value n slots below the stack top (0 = topmost).
func top(sp unsafe.Pointer, n int) *Value { return (*Value)(unsafe.Add(sp, -(n+1)*vsize)) }

func slot16(p unsafe.Pointer, i int) *Value { return (*Value)(unsafe.Add(p, i*vsize)) }

func u8(ip unsafe.Pointer, off int) byte { return *(*byte)(unsafe.Add(ip, off)) }

func u16(ip unsafe.Pointer, off int) int {
	return int(*(*byte)(unsafe.Add(ip, off)))<<8 | int(*(*byte)(unsafe.Add(ip, off+1)))
}

func konst(fn *ObjFunction, i byte) Value { return *(*Value)(unsafe.Add(fn.constp, int(i)*vsize)) }

func cache(fn *ObjFunction, i int) *PropCache {
	return (*PropCache)(unsafe.Add(fn.cachep, i*int(unsafe.Sizeof(PropCache{}))))
}

func (vm *VM) run(sp unsafe.Pointer) bool {
	frame := &vm.frames[vm.frameCount-1]
	fn := frame.closure.fn
	ip := frame.ip
	bp := frame.bp
	gp := unsafe.Pointer(unsafe.SliceData(vm.globals))

	for {
		op := *(*byte)(ip)
		ip = unsafe.Add(ip, 1)
		switch op {
		case OpConstant:
			*(*Value)(sp) = konst(fn, u8(ip, 0))
			ip = unsafe.Add(ip, 1)
			sp = unsafe.Add(sp, vsize)
		case OpNil:
			*(*Value)(sp) = nilV
			sp = unsafe.Add(sp, vsize)
		case OpTrue:
			*(*Value)(sp) = trueV
			sp = unsafe.Add(sp, vsize)
		case OpFalse:
			*(*Value)(sp) = falseV
			sp = unsafe.Add(sp, vsize)
		case OpPop:
			sp = unsafe.Add(sp, -vsize)
		case OpGetLocal:
			*(*Value)(sp) = *slot(bp, u8(ip, 0))
			ip = unsafe.Add(ip, 1)
			sp = unsafe.Add(sp, vsize)
		case OpSetLocal:
			*slot(bp, u8(ip, 0)) = *top(sp, 0)
			ip = unsafe.Add(ip, 1)
		case OpSetLocalPop:
			sp = unsafe.Add(sp, -vsize)
			*slot(bp, u8(ip, 0)) = *(*Value)(sp)
			ip = unsafe.Add(ip, 1)
		case OpGetGlobal:
			idx := u16(ip, 0)
			ip = unsafe.Add(ip, 2)
			v := *slot16(gp, idx)
			if v.p == undefP {
				frame.ip = ip
				vm.runtimeError("Undefined variable '%s'.", vm.globalNames[idx])
				return false
			}
			*(*Value)(sp) = v
			sp = unsafe.Add(sp, vsize)
		case OpDefineGlobal:
			idx := u16(ip, 0)
			ip = unsafe.Add(ip, 2)
			sp = unsafe.Add(sp, -vsize)
			*slot16(gp, idx) = *(*Value)(sp)
		case OpSetGlobal, OpSetGlobalPop:
			idx := u16(ip, 0)
			ip = unsafe.Add(ip, 2)
			if slot16(gp, idx).p == undefP {
				frame.ip = ip
				vm.runtimeError("Undefined variable '%s'.", vm.globalNames[idx])
				return false
			}
			*slot16(gp, idx) = *top(sp, 0)
			if op == OpSetGlobalPop {
				sp = unsafe.Add(sp, -vsize)
			}
		case OpGetUpvalue:
			*(*Value)(sp) = *frame.closure.upvalues[u8(ip, 0)].loc
			ip = unsafe.Add(ip, 1)
			sp = unsafe.Add(sp, vsize)
		case OpSetUpvalue:
			*frame.closure.upvalues[u8(ip, 0)].loc = *top(sp, 0)
			ip = unsafe.Add(ip, 1)
		case OpGetProperty:
			recv := top(sp, 0)
			if !recv.isObj(kInstance) {
				frame.ip = unsafe.Add(ip, 3)
				vm.runtimeError("Only instances have properties.")
				return false
			}
			inst := recv.asInstance()
			c := cache(fn, u16(ip, 1))
			if inst.shape == c.shape {
				*recv = inst.fields[c.idx]
				ip = unsafe.Add(ip, 3)
				continue
			}
			name := konst(fn, u8(ip, 0)).asString()
			ip = unsafe.Add(ip, 3)
			v, ok := vm.getProperty(inst, name, c)
			if !ok {
				frame.ip = ip
				vm.runtimeError("Undefined property '%s'.", name.s)
				return false
			}
			*recv = v
		case OpGetLocalProperty:
			recv := slot(bp, u8(ip, 0))
			ip = unsafe.Add(ip, 1)
			if !recv.isObj(kInstance) {
				frame.ip = unsafe.Add(ip, 3)
				vm.runtimeError("Only instances have properties.")
				return false
			}
			inst := recv.asInstance()
			c := cache(fn, u16(ip, 1))
			if inst.shape == c.shape {
				*(*Value)(sp) = inst.fields[c.idx]
				sp = unsafe.Add(sp, vsize)
				ip = unsafe.Add(ip, 3)
				continue
			}
			name := konst(fn, u8(ip, 0)).asString()
			ip = unsafe.Add(ip, 3)
			v, ok := vm.getProperty(inst, name, c)
			if !ok {
				frame.ip = ip
				vm.runtimeError("Undefined property '%s'.", name.s)
				return false
			}
			*(*Value)(sp) = v
			sp = unsafe.Add(sp, vsize)
		case OpSetProperty, OpSetPropertyPop:
			recv := top(sp, 1)
			if !recv.isObj(kInstance) {
				frame.ip = unsafe.Add(ip, 3)
				vm.runtimeError("Only instances have fields.")
				return false
			}
			inst := recv.asInstance()
			v := *top(sp, 0)
			c := cache(fn, u16(ip, 1))
			if inst.shape == c.shape && c.newShape == nil {
				inst.fields[c.idx] = v
			} else {
				vm.setProperty(inst, konst(fn, u8(ip, 0)).asString(), v, c)
			}
			ip = unsafe.Add(ip, 3)
			if op == OpSetPropertyPop {
				sp = unsafe.Add(sp, -2*vsize)
			} else {
				*recv = v
				sp = unsafe.Add(sp, -vsize)
			}
		case OpGetSuper:
			name := konst(fn, u8(ip, 0)).asString()
			ip = unsafe.Add(ip, 1)
			sp = unsafe.Add(sp, -vsize)
			super := (*Value)(sp).asClass()
			m := super.methods[name]
			if m == nil {
				frame.ip = ip
				vm.runtimeError("Undefined property '%s'.", name.s)
				return false
			}
			recv := top(sp, 0)
			*recv = objV(&ObjBoundMethod{Obj: Obj{kBoundMethod}, receiver: *recv, method: m})
		case OpEqual:
			sp = unsafe.Add(sp, -vsize)
			a := top(sp, 0)
			*a = boolV(valuesEqual(*a, *(*Value)(sp)))
		case OpNotEqual:
			sp = unsafe.Add(sp, -vsize)
			a := top(sp, 0)
			*a = boolV(!valuesEqual(*a, *(*Value)(sp)))
		case OpGreater, OpGreaterEqual, OpLess, OpLessEqual:
			sp = unsafe.Add(sp, -vsize)
			a, b := top(sp, 0), (*Value)(sp)
			if a.p != nil || b.p != nil {
				frame.ip = ip
				vm.runtimeError("Operands must be numbers.")
				return false
			}
			var r bool
			switch op {
			case OpGreater:
				r = a.n > b.n
			case OpGreaterEqual:
				r = a.n >= b.n
			case OpLess:
				r = a.n < b.n
			default:
				r = a.n <= b.n
			}
			*a = boolV(r)
		case OpAdd:
			sp = unsafe.Add(sp, -vsize)
			a, b := top(sp, 0), (*Value)(sp)
			if a.p == nil && b.p == nil {
				a.n += b.n
			} else if a.isObj(kString) && b.isObj(kString) {
				*a = objV(&ObjString{Obj: Obj{kString}, s: a.asString().s + b.asString().s})
			} else {
				frame.ip = ip
				vm.runtimeError("Operands must be two numbers or two strings.")
				return false
			}
		case OpSubtract:
			sp = unsafe.Add(sp, -vsize)
			a, b := top(sp, 0), (*Value)(sp)
			if a.p != nil || b.p != nil {
				frame.ip = ip
				vm.runtimeError("Operands must be numbers.")
				return false
			}
			a.n -= b.n
		case OpMultiply:
			sp = unsafe.Add(sp, -vsize)
			a, b := top(sp, 0), (*Value)(sp)
			if a.p != nil || b.p != nil {
				frame.ip = ip
				vm.runtimeError("Operands must be numbers.")
				return false
			}
			a.n *= b.n
		case OpDivide:
			sp = unsafe.Add(sp, -vsize)
			a, b := top(sp, 0), (*Value)(sp)
			if a.p != nil || b.p != nil {
				frame.ip = ip
				vm.runtimeError("Operands must be numbers.")
				return false
			}
			a.n /= b.n
		case OpNot:
			a := top(sp, 0)
			*a = boolV(a.isFalsey())
		case OpNegate:
			a := top(sp, 0)
			if a.p != nil {
				frame.ip = ip
				vm.runtimeError("Operand must be a number.")
				return false
			}
			a.n = -a.n
		case OpPrint:
			sp = unsafe.Add(sp, -vsize)
			vm.out.WriteString(formatValue(*(*Value)(sp)))
			vm.out.WriteByte('\n')
		case OpJump:
			ip = unsafe.Add(ip, u16(ip, 0)+2)
		case OpJumpIfFalse:
			if top(sp, 0).isFalsey() {
				ip = unsafe.Add(ip, u16(ip, 0)+2)
			} else {
				ip = unsafe.Add(ip, 2)
			}
		case OpJumpIfFalsePop:
			sp = unsafe.Add(sp, -vsize)
			if (*Value)(sp).isFalsey() {
				ip = unsafe.Add(ip, u16(ip, 0)+2)
			} else {
				ip = unsafe.Add(ip, 2)
			}
		case OpLoop:
			ip = unsafe.Add(ip, 2-u16(ip, 0))
		case OpEqualJump, OpNotEqualJump:
			sp = unsafe.Add(sp, -2*vsize)
			if valuesEqual(*(*Value)(sp), *(*Value)(unsafe.Add(sp, vsize))) == (op == OpEqualJump) {
				ip = unsafe.Add(ip, 2)
			} else {
				ip = unsafe.Add(ip, u16(ip, 0)+2)
			}
		case OpGreaterJump, OpGreaterEqualJump, OpLessJump, OpLessEqualJump:
			sp = unsafe.Add(sp, -2*vsize)
			a, b := (*Value)(sp), (*Value)(unsafe.Add(sp, vsize))
			if a.p != nil || b.p != nil {
				frame.ip = unsafe.Add(ip, -2)
				vm.runtimeError("Operands must be numbers.")
				return false
			}
			var r bool
			switch op {
			case OpGreaterJump:
				r = a.n > b.n
			case OpGreaterEqualJump:
				r = a.n >= b.n
			case OpLessJump:
				r = a.n < b.n
			default:
				r = a.n <= b.n
			}
			if r {
				ip = unsafe.Add(ip, 2)
			} else {
				ip = unsafe.Add(ip, u16(ip, 0)+2)
			}
		case OpAddC:
			a := top(sp, 0)
			if a.p != nil {
				frame.ip = unsafe.Add(ip, 1)
				vm.runtimeError("Operands must be two numbers or two strings.")
				return false
			}
			a.n += konst(fn, u8(ip, 0)).n
			ip = unsafe.Add(ip, 1)
		case OpSubC:
			a := top(sp, 0)
			if a.p != nil {
				frame.ip = unsafe.Add(ip, 1)
				vm.runtimeError("Operands must be numbers.")
				return false
			}
			a.n -= konst(fn, u8(ip, 0)).n
			ip = unsafe.Add(ip, 1)
		case OpGreaterC, OpGreaterEqualC, OpLessC, OpLessEqualC:
			a := top(sp, 0)
			b := konst(fn, u8(ip, 0)).n
			ip = unsafe.Add(ip, 1)
			if a.p != nil {
				frame.ip = ip
				vm.runtimeError("Operands must be numbers.")
				return false
			}
			var r bool
			switch op {
			case OpGreaterC:
				r = a.n > b
			case OpGreaterEqualC:
				r = a.n >= b
			case OpLessC:
				r = a.n < b
			default:
				r = a.n <= b
			}
			*a = boolV(r)
		case OpGreaterCJump, OpGreaterEqualCJump, OpLessCJump, OpLessEqualCJump:
			sp = unsafe.Add(sp, -vsize)
			a := (*Value)(sp)
			b := konst(fn, u8(ip, 0)).n
			if a.p != nil {
				frame.ip = unsafe.Add(ip, 1)
				vm.runtimeError("Operands must be numbers.")
				return false
			}
			var r bool
			switch op {
			case OpGreaterCJump:
				r = a.n > b
			case OpGreaterEqualCJump:
				r = a.n >= b
			case OpLessCJump:
				r = a.n < b
			default:
				r = a.n <= b
			}
			if r {
				ip = unsafe.Add(ip, 3)
			} else {
				ip = unsafe.Add(ip, u16(ip, 1)+3)
			}
		case OpCall:
			argc := int(u8(ip, 0))
			ip = unsafe.Add(ip, 1)
			frame.ip = ip
			callee := *top(sp, argc)
			if callee.isObj(kClosure) {
				ncl := callee.asClosure()
				nb := unsafe.Add(sp, -(argc+1)*vsize)
				if argc != ncl.fn.arity || vm.frameCount == FramesMax || uintptr(nb) > vm.stackLimit {
					vm.pushFrame(ncl, argc, nb)
					return false
				}
				frame = &vm.frames[vm.frameCount]
				vm.frameCount++
				frame.closure = ncl
				frame.bp = nb
				fn = ncl.fn
				ip = fn.codep
				bp = nb
				continue
			}
			var ok bool
			if sp, ok = vm.callValue(callee, argc, sp); !ok {
				return false
			}
			frame = &vm.frames[vm.frameCount-1]
			fn = frame.closure.fn
			ip = frame.ip
			bp = frame.bp
		case OpInvoke:
			argc := int(u8(ip, 1))
			c := cache(fn, u16(ip, 2))
			nameIdx := u8(ip, 0)
			ip = unsafe.Add(ip, 4)
			frame.ip = ip
			nb := unsafe.Add(sp, -(argc+1)*vsize)
			recv := (*Value)(nb)
			if !recv.isObj(kInstance) {
				vm.runtimeError("Only instances have methods.")
				return false
			}
			inst := recv.asInstance()
			var m *ObjClosure
			if inst.shape == c.shape && inst.class == c.class {
				m = c.method
			} else {
				name := konst(fn, nameIdx).asString()
				if i := inst.shape.lookup(name); i >= 0 {
					*recv = inst.fields[i]
					var ok bool
					if sp, ok = vm.callValue(inst.fields[i], argc, sp); !ok {
						return false
					}
					frame = &vm.frames[vm.frameCount-1]
					fn = frame.closure.fn
					ip = frame.ip
					bp = frame.bp
					continue
				}
				m = inst.class.methods[name]
				if m == nil {
					vm.runtimeError("Undefined property '%s'.", name.s)
					return false
				}
				c.shape, c.class, c.method = inst.shape, inst.class, m
			}
			if argc != m.fn.arity || vm.frameCount == FramesMax || uintptr(nb) > vm.stackLimit {
				vm.pushFrame(m, argc, nb)
				return false
			}
			frame = &vm.frames[vm.frameCount]
			vm.frameCount++
			frame.closure = m
			frame.bp = nb
			fn = m.fn
			ip = fn.codep
			bp = nb
		case OpSuperInvoke:
			name := konst(fn, u8(ip, 0)).asString()
			argc := int(u8(ip, 1))
			ip = unsafe.Add(ip, 2)
			frame.ip = ip
			sp = unsafe.Add(sp, -vsize)
			super := (*Value)(sp).asClass()
			m := super.methods[name]
			if m == nil {
				vm.runtimeError("Undefined property '%s'.", name.s)
				return false
			}
			if !vm.pushFrame(m, argc, unsafe.Add(sp, -(argc+1)*vsize)) {
				return false
			}
			frame = &vm.frames[vm.frameCount-1]
			fn = frame.closure.fn
			ip = frame.ip
			bp = frame.bp
		case OpClosure:
			nfn := konst(fn, u8(ip, 0)).asFunction()
			ip = unsafe.Add(ip, 1)
			ncl := &ObjClosure{Obj: Obj{kClosure}, fn: nfn}
			if nfn.upvalueCount > 0 {
				ncl.upvalues = make([]*ObjUpvalue, nfn.upvalueCount)
				for i := range ncl.upvalues {
					isLocal := u8(ip, 0)
					index := u8(ip, 1)
					ip = unsafe.Add(ip, 2)
					if isLocal == 1 {
						ncl.upvalues[i] = vm.captureUpvalue(slot(bp, index))
					} else {
						ncl.upvalues[i] = frame.closure.upvalues[index]
					}
				}
			}
			*(*Value)(sp) = objV(ncl)
			sp = unsafe.Add(sp, vsize)
		case OpCloseUpvalue:
			sp = unsafe.Add(sp, -vsize)
			vm.closeUpvalues(sp)
		case OpReturn:
			result := *top(sp, 0)
			if vm.openUpvalues != nil {
				vm.closeUpvalues(bp)
			}
			vm.frameCount--
			if vm.frameCount == 0 {
				return true
			}
			sp = unsafe.Add(bp, vsize)
			*(*Value)(bp) = result
			frame = &vm.frames[vm.frameCount-1]
			fn = frame.closure.fn
			ip = frame.ip
			bp = frame.bp
		case OpClass:
			name := konst(fn, u8(ip, 0)).asString()
			ip = unsafe.Add(ip, 1)
			*(*Value)(sp) = objV(&ObjClass{Obj: Obj{kClass}, name: name, methods: make(map[*ObjString]*ObjClosure)})
			sp = unsafe.Add(sp, vsize)
		case OpInherit:
			superV := *top(sp, 1)
			if !superV.isObj(kClass) {
				frame.ip = ip
				vm.runtimeError("Superclass must be a class.")
				return false
			}
			super := superV.asClass()
			sub := top(sp, 0).asClass()
			for k, v := range super.methods {
				sub.methods[k] = v
			}
			sub.init = super.init
			sp = unsafe.Add(sp, -vsize)
		case OpMethod:
			name := konst(fn, u8(ip, 0)).asString()
			ip = unsafe.Add(ip, 1)
			c := top(sp, 1).asClass()
			m := top(sp, 0).asClosure()
			c.methods[name] = m
			if name == vm.initString {
				c.init = m
			}
			sp = unsafe.Add(sp, -vsize)
		default:
			panic("unknown opcode")
		}
	}
}

func (vm *VM) getProperty(inst *ObjInstance, name *ObjString, c *PropCache) (Value, bool) {
	if i := inst.shape.lookup(name); i >= 0 {
		c.shape = inst.shape
		c.idx = i
		return inst.fields[i], true
	}
	m := inst.class.methods[name]
	if m == nil {
		return nilV, false
	}
	return objV(&ObjBoundMethod{Obj: Obj{kBoundMethod}, receiver: objV(inst), method: m}), true
}

func (vm *VM) setProperty(inst *ObjInstance, name *ObjString, v Value, c *PropCache) {
	if inst.shape == c.shape {
		inst.fields = append(inst.fields, v)
		inst.shape = c.newShape
		return
	}
	if i := inst.shape.lookup(name); i >= 0 {
		inst.fields[i] = v
		c.shape, c.idx, c.newShape = inst.shape, i, nil
		return
	}
	old := inst.shape
	ns := old.add(name)
	inst.fields = append(inst.fields, v)
	inst.shape = ns
	c.shape, c.idx, c.newShape = old, len(inst.fields)-1, ns
	if len(inst.fields) > inst.class.fieldHint {
		inst.class.fieldHint = len(inst.fields)
	}
}

// newInstance allocates an instance together with inline storage for the
// number of fields instances of its class have been seen to use.
func newInstance(c *ObjClass) *ObjInstance {
	var inst *ObjInstance
	switch n := c.fieldHint; {
	case n == 0:
		inst = &ObjInstance{}
	case n == 1:
		o := &struct {
			ObjInstance
			a [1]Value
		}{}
		o.fields = o.a[:0]
		inst = &o.ObjInstance
	case n == 2:
		o := &struct {
			ObjInstance
			a [2]Value
		}{}
		o.fields = o.a[:0]
		inst = &o.ObjInstance
	case n <= 4:
		o := &struct {
			ObjInstance
			a [4]Value
		}{}
		o.fields = o.a[:0]
		inst = &o.ObjInstance
	case n <= 6:
		o := &struct {
			ObjInstance
			a [6]Value
		}{}
		o.fields = o.a[:0]
		inst = &o.ObjInstance
	case n <= 8:
		o := &struct {
			ObjInstance
			a [8]Value
		}{}
		o.fields = o.a[:0]
		inst = &o.ObjInstance
	default:
		inst = &ObjInstance{fields: make([]Value, 0, n)}
	}
	inst.kind = kInstance
	inst.class = c
	inst.shape = rootShape
	return inst
}
