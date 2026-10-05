package main

import (
	"math"
	"runtime"
	"runtime/debug"
	"strconv"
	"strings"
)

const (
	errWrongType = "WRONGTYPE Operation against a key holding the wrong kind of value"
	errNotInt    = "ERR value is not an integer or out of range"
	errOverflow  = "ERR increment or decrement would overflow"
	errSyntax    = "ERR syntax error"
	errOOM       = "OOM command not allowed when used memory > 'maxmemory'."
	errHashInt   = "ERR hash value is not an integer"
	errPositive  = "ERR value is out of range, must be positive"
)

type command struct {
	name  string // lowercase
	arity int
	fn    func(c *client, a [][]byte)
	subOK bool
}

var commands = map[string]*command{}

func reg(name string, arity int, fn func(c *client, a [][]byte)) *command {
	cmd := &command{name: name, arity: arity, fn: fn}
	commands[strings.ToUpper(name)] = cmd
	return cmd
}

func init() {
	reg("ping", -1, cmdPing).subOK = true
	reg("quit", -1, cmdQuit).subOK = true
	reg("subscribe", -2, cmdSubscribe).subOK = true
	reg("unsubscribe", -1, cmdUnsubscribe).subOK = true
	reg("publish", 3, cmdPublish)
	reg("echo", 2, func(c *client, a [][]byte) { c.addBulk(a[1]) })
	reg("dbsize", 1, cmdDbsize)
	reg("flushall", -1, cmdFlushall)
	reg("flushdb", -1, cmdFlushall)
	reg("info", -1, cmdInfo)
	reg("config", -2, func(c *client, a [][]byte) { c.addArr(0) })
	reg("command", -1, func(c *client, a [][]byte) { c.addArr(0) })
	reg("select", 2, func(c *client, a [][]byte) {
		if string(a[1]) == "0" {
			c.addOK()
		} else {
			c.addErr("ERR DB index is out of range")
		}
	})

	reg("get", 2, cmdGet)
	reg("set", -3, cmdSet)
	reg("mget", -2, cmdMget)
	reg("mset", -3, cmdMset)
	reg("incr", 2, func(c *client, a [][]byte) { incrBy(c, a[1], 1) })
	reg("decr", 2, func(c *client, a [][]byte) { incrBy(c, a[1], -1) })
	reg("incrby", 3, func(c *client, a [][]byte) {
		n, ok := parseInt(a[2])
		if !ok {
			c.addErr(errNotInt)
			return
		}
		incrBy(c, a[1], n)
	})
	reg("decrby", 3, func(c *client, a [][]byte) {
		n, ok := parseInt(a[2])
		if !ok {
			c.addErr(errNotInt)
			return
		}
		if n == math.MinInt64 {
			c.addErr(errOverflow)
			return
		}
		incrBy(c, a[1], -n)
	})
	reg("append", 3, cmdAppend)
	reg("strlen", 2, cmdStrlen)

	reg("del", -2, cmdDel)
	reg("unlink", -2, cmdDel)
	reg("exists", -2, cmdExists)
	reg("type", 2, cmdType)
	reg("expire", 3, func(c *client, a [][]byte) { cmdExpire(c, a, 1000, "expire") })
	reg("pexpire", 3, func(c *client, a [][]byte) { cmdExpire(c, a, 1, "pexpire") })
	reg("ttl", 2, func(c *client, a [][]byte) { cmdTTL(c, a, true) })
	reg("pttl", 2, func(c *client, a [][]byte) { cmdTTL(c, a, false) })
	reg("persist", 2, cmdPersist)
	reg("keys", 2, cmdKeys)

	reg("lpush", -3, func(c *client, a [][]byte) { cmdPush(c, a, true) })
	reg("rpush", -3, func(c *client, a [][]byte) { cmdPush(c, a, false) })
	reg("lpop", -2, func(c *client, a [][]byte) { cmdPop(c, a, true) })
	reg("rpop", -2, func(c *client, a [][]byte) { cmdPop(c, a, false) })
	reg("llen", 2, cmdLlen)
	reg("lrange", 4, cmdLrange)
	reg("lindex", 3, cmdLindex)

	reg("hset", -4, cmdHset)
	reg("hmset", -4, func(c *client, a [][]byte) {
		l := len(c.out)
		cmdHset(c, a)
		if len(c.out) > l && c.out[l] == ':' {
			c.out = c.out[:l]
			c.addOK()
		}
	})
	reg("hget", 3, cmdHget)
	reg("hdel", -3, cmdHdel)
	reg("hgetall", 2, cmdHgetall)
	reg("hlen", 2, cmdHlen)
	reg("hexists", 3, cmdHexists)
	reg("hincrby", 4, cmdHincrby)
}

func (c *client) exec(args [][]byte) {
	name := args[0]
	var ub [24]byte
	var cmd *command
	if len(name) <= len(ub) {
		for i, b := range name {
			if 'a' <= b && b <= 'z' {
				b -= 32
			}
			ub[i] = b
		}
		cmd = commands[string(ub[:len(name)])]
	}
	if cmd == nil {
		var sb strings.Builder
		sb.WriteString("ERR unknown command '")
		sb.WriteString(sanitize(name))
		sb.WriteString("', with args beginning with: ")
		for _, a := range args[1:] {
			sb.WriteString("'")
			sb.WriteString(sanitize(a))
			sb.WriteString("' ")
		}
		c.addErr(sb.String())
	} else if (cmd.arity > 0 && len(args) != cmd.arity) || (cmd.arity < 0 && len(args) < -cmd.arity) {
		c.addErr("ERR wrong number of arguments for '" + cmd.name + "' command")
	} else if len(c.subs) > 0 && !cmd.subOK {
		c.addErr("ERR Can't execute '" + cmd.name + "': only (P|S)SUBSCRIBE / (P|S)UNSUBSCRIBE / PING / QUIT / RESET are allowed in this context")
	} else {
		cmd.fn(c, args)
	}
	if c.async && len(c.out) > 0 {
		c.pushAsync()
	}
}

func sanitize(b []byte) string {
	if len(b) > 128 {
		b = b[:128]
	}
	return strings.Map(func(r rune) rune {
		if r == '\r' || r == '\n' {
			return ' '
		}
		return r
	}, string(b))
}

// parseInt parses a strict decimal int64 like Redis' string2ll.
func parseInt(b []byte) (int64, bool) {
	if len(b) == 0 || len(b) > 20 {
		return 0, false
	}
	if len(b) == 1 && b[0] == '0' {
		return 0, true
	}
	i, neg := 0, false
	if b[0] == '-' {
		neg, i = true, 1
		if len(b) == 1 {
			return 0, false
		}
	}
	if b[i] < '1' || b[i] > '9' {
		return 0, false
	}
	var v uint64
	for ; i < len(b); i++ {
		d := uint64(b[i] - '0')
		if d > 9 || v > (math.MaxUint64-d)/10 {
			return 0, false
		}
		v = v*10 + d
	}
	if neg {
		if v > 1<<63 {
			return 0, false
		}
		return int64(-v), true
	}
	if v > math.MaxInt64 {
		return 0, false
	}
	return int64(v), true
}

func eqFold(b []byte, s string) bool {
	if len(b) != len(s) {
		return false
	}
	for i := range b {
		ch := b[i]
		if 'a' <= ch && ch <= 'z' {
			ch -= 32
		}
		if ch != s[i] {
			return false
		}
	}
	return true
}

func (c *client) afterWrite(keys ...[]byte) {
	if maxMemory > 0 && globalUsed.Load() > maxMemory {
		evict(keys)
	}
}

// lockKeys locks the shards of all keys in ascending order; hashes go to c.hs.
func (c *client) lockKeys(keys [][]byte, step int) {
	c.hs = c.hs[:0]
	c.locks = c.locks[:0]
	for i := 0; i < len(keys); i += step {
		h := hashKey(keys[i])
		c.hs = append(c.hs, h)
		si := int(h & (nShards - 1))
		pos := len(c.locks)
		dup := false
		for j, l := range c.locks {
			if l == si {
				dup = true
				break
			}
			if l > si {
				pos = j
				break
			}
		}
		if !dup {
			c.locks = append(c.locks, 0)
			copy(c.locks[pos+1:], c.locks[pos:])
			c.locks[pos] = si
		}
	}
	for _, si := range c.locks {
		shards[si].mu.Lock()
	}
}

func (c *client) unlockKeys() {
	for i := len(c.locks) - 1; i >= 0; i-- {
		shards[c.locks[i]].mu.Unlock()
	}
}

// ---------------------------------------------------------------- server

func cmdPing(c *client, a [][]byte) {
	if len(a) > 2 {
		c.addErr("ERR wrong number of arguments for 'ping' command")
		return
	}
	if len(c.subs) > 0 {
		c.addRaw("*2\r\n$4\r\npong\r\n")
		if len(a) == 2 {
			c.addBulk(a[1])
		} else {
			c.addRaw("$0\r\n\r\n")
		}
		return
	}
	if len(a) == 2 {
		c.addBulk(a[1])
	} else {
		c.addRaw("+PONG\r\n")
	}
}

func cmdQuit(c *client, a [][]byte) {
	c.addOK()
	c.quit = true
}

func cmdDbsize(c *client, a [][]byte) {
	n := 0
	for i := range shards {
		s := &shards[i]
		s.mu.Lock()
		n += s.count
		s.mu.Unlock()
	}
	c.addInt(int64(n))
}

func cmdFlushall(c *client, a [][]byte) {
	for i := range shards {
		shards[i].mu.Lock()
	}
	for i := range shards {
		shards[i].flush()
	}
	arenaReset()
	for i := range shards {
		shards[i].mu.Unlock()
	}
	go debug.FreeOSMemory()
	c.addOK()
}

func cmdInfo(c *client, a [][]byte) {
	var used int64
	var keys, expires int
	for i := range shards {
		s := &shards[i]
		s.mu.Lock()
		used += s.used
		keys += s.count
		expires += s.nexp
		s.mu.Unlock()
	}
	var b []byte
	b = append(b, "# Server\r\nredis_version:8.0.0\r\nredis_mode:standalone\r\n"...)
	b = append(b, "\r\n# Clients\r\nconnected_clients:"...)
	b = strconv.AppendInt(b, connectedClients.Load(), 10)
	b = append(b, "\r\n\r\n# Memory\r\nused_memory:"...)
	b = strconv.AppendInt(b, used, 10)
	var ms runtime.MemStats
	runtime.ReadMemStats(&ms)
	b = append(b, "\r\ngo_heap_inuse:"...)
	b = strconv.AppendUint(b, ms.HeapInuse, 10)
	b = append(b, "\r\ngo_heap_idle:"...)
	b = strconv.AppendUint(b, ms.HeapIdle, 10)
	b = append(b, "\r\ngo_heap_released:"...)
	b = strconv.AppendUint(b, ms.HeapReleased, 10)
	b = append(b, "\r\ngo_sys:"...)
	b = strconv.AppendUint(b, ms.Sys, 10)
	b = append(b, "\r\nmaxmemory:"...)
	b = strconv.AppendInt(b, maxMemory, 10)
	b = append(b, "\r\nmaxmemory_policy:allkeys-lru\r\n\r\n# Keyspace\r\n"...)
	if keys > 0 {
		b = append(b, "db0:keys="...)
		b = strconv.AppendInt(b, int64(keys), 10)
		b = append(b, ",expires="...)
		b = strconv.AppendInt(b, int64(expires), 10)
		b = append(b, ",avg_ttl=0\r\n"...)
	}
	c.addBulk(b)
}

// ---------------------------------------------------------------- strings

func cmdGet(c *client, a [][]byte) {
	h := hashKey(a[1])
	s := shardOf(h)
	s.mu.Lock()
	_, it, _ := s.get(a[1], h, c.now)
	if it == 0 {
		c.addNull()
	} else if it.typ() != typStr {
		c.addErr(errWrongType)
	} else {
		c.addBulk(it.val())
	}
	s.mu.Unlock()
}

func cmdSet(c *client, a [][]byte) {
	var nx, xx, get, keep, hasExp bool
	var exp int64
	for i := 3; i < len(a); i++ {
		o := a[i]
		switch {
		case eqFold(o, "NX") && !xx:
			nx = true
		case eqFold(o, "XX") && !nx:
			xx = true
		case eqFold(o, "GET"):
			get = true
		case eqFold(o, "KEEPTTL") && !hasExp:
			keep = true
		case (eqFold(o, "EX") || eqFold(o, "PX")) && !keep && !hasExp && i+1 < len(a):
			v, ok := parseInt(a[i+1])
			if !ok {
				c.addErr(errNotInt)
				return
			}
			if v <= 0 {
				c.addErr("ERR invalid expire time in 'set' command")
				return
			}
			if o[0] == 'E' || o[0] == 'e' {
				if v > math.MaxInt64/1000 {
					c.addErr("ERR invalid expire time in 'set' command")
					return
				}
				v *= 1000
			}
			if v > math.MaxInt64-c.now {
				c.addErr("ERR invalid expire time in 'set' command")
				return
			}
			exp = c.now + v
			hasExp = true
			i++
		default:
			c.addErr(errSyntax)
			return
		}
	}
	key, val := a[1], a[2]
	if maxMemory > 0 && int64(64+len(key)+len(val)) > maxMemory {
		c.addErr(errOOM)
		return
	}
	h := hashKey(key)
	s := shardOf(h)
	s.mu.Lock()
	s.grow()
	idx, it, ins := s.get(key, h, c.now)
	if get && it != 0 && it.typ() != typStr {
		s.mu.Unlock()
		c.addErr(errWrongType)
		return
	}
	if (nx && it != 0) || (xx && it == 0) {
		if get && it != 0 {
			c.addBulk(it.val())
		} else {
			c.addNull()
		}
		s.mu.Unlock()
		return
	}
	if get {
		if it != 0 {
			c.addBulk(it.val())
		} else {
			c.addNull()
		}
	}
	if keep && it != 0 {
		exp = it.exp()
	}
	s.setStr(idx, it, ins, h, key, val, exp, c.now)
	s.mu.Unlock()
	if !get {
		c.addOK()
	}
	c.afterWrite(key)
}

func cmdMget(c *client, a [][]byte) {
	keys := a[1:]
	c.lockKeys(keys, 1)
	c.addArr(len(keys))
	for i, k := range keys {
		h := c.hs[i]
		_, it, _ := shardOf(h).get(k, h, c.now)
		if it == 0 || it.typ() != typStr {
			c.addNull()
		} else {
			c.addBulk(it.val())
		}
	}
	c.unlockKeys()
}

func cmdMset(c *client, a [][]byte) {
	if (len(a)-1)%2 != 0 {
		c.addErr("ERR wrong number of arguments for 'mset' command")
		return
	}
	if maxMemory > 0 {
		var total int64
		for i := 1; i < len(a); i += 2 {
			total += int64(64 + len(a[i]) + len(a[i+1]))
		}
		if total > maxMemory {
			c.addErr(errOOM)
			return
		}
	}
	c.lockKeys(a[1:], 2)
	for i := 1; i < len(a); i += 2 {
		h := c.hs[(i-1)/2]
		s := shardOf(h)
		s.grow()
		idx, it, ins := s.get(a[i], h, c.now)
		s.setStr(idx, it, ins, h, a[i], a[i+1], 0, c.now)
	}
	c.unlockKeys()
	c.addOK()
	if maxMemory > 0 {
		c.keys = c.keys[:0]
		for i := 1; i < len(a); i += 2 {
			c.keys = append(c.keys, a[i])
		}
		c.afterWrite(c.keys...)
	}
}

func incrBy(c *client, key []byte, delta int64) {
	h := hashKey(key)
	s := shardOf(h)
	s.mu.Lock()
	s.grow()
	idx, it, ins := s.get(key, h, c.now)
	var cur, exp int64
	if it != 0 {
		if it.typ() != typStr {
			s.mu.Unlock()
			c.addErr(errWrongType)
			return
		}
		v, ok := parseInt(it.val())
		if !ok {
			s.mu.Unlock()
			c.addErr(errNotInt)
			return
		}
		cur, exp = v, it.exp()
	}
	if (delta > 0 && cur > math.MaxInt64-delta) || (delta < 0 && cur < math.MinInt64-delta) {
		s.mu.Unlock()
		c.addErr(errOverflow)
		return
	}
	cur += delta
	c.num = strconv.AppendInt(c.num[:0], cur, 10)
	s.setStr(idx, it, ins, h, key, c.num, exp, c.now)
	s.mu.Unlock()
	c.addInt(cur)
	c.afterWrite(key)
}

func cmdAppend(c *client, a [][]byte) {
	key, v := a[1], a[2]
	h := hashKey(key)
	s := shardOf(h)
	s.mu.Lock()
	s.grow()
	idx, it, ins := s.get(key, h, c.now)
	if it == 0 {
		if maxMemory > 0 && int64(64+len(key)+len(v)) > maxMemory {
			s.mu.Unlock()
			c.addErr(errOOM)
			return
		}
		s.setStr(idx, it, ins, h, key, v, 0, c.now)
		s.mu.Unlock()
		c.addInt(int64(len(v)))
		c.afterWrite(key)
		return
	}
	if it.typ() != typStr {
		s.mu.Unlock()
		c.addErr(errWrongType)
		return
	}
	ol := it.vlen()
	nl := ol + len(v)
	if maxMemory > 0 && int64(64+len(key)+nl) > maxMemory {
		s.mu.Unlock()
		c.addErr(errOOM)
		return
	}
	if nl <= it.capacity() && it.cls() != clsBig {
		copy(it.valN(nl)[ol:], v)
		it.setVlen(nl)
		s.addUsed(int64(len(v)))
	} else {
		// grow with headroom so repeated appends are amortized
		capN := nl + nl/4
		if hdrSize+len(key)+capN > maxSmall {
			capN = nl
		}
		nw := s.newItem(key, capN, typStr, c.now)
		nw.setVlen(nl)
		copy(nw.val(), it.val())
		copy(nw.val()[ol:], v)
		nw.setExpRaw(it.exp())
		s.replace(idx, it, nw)
	}
	s.mu.Unlock()
	c.addInt(int64(nl))
	c.afterWrite(key)
}

func cmdStrlen(c *client, a [][]byte) {
	h := hashKey(a[1])
	s := shardOf(h)
	s.mu.Lock()
	_, it, _ := s.get(a[1], h, c.now)
	if it == 0 {
		c.addInt(0)
	} else if it.typ() != typStr {
		c.addErr(errWrongType)
	} else {
		c.addInt(int64(it.vlen()))
	}
	s.mu.Unlock()
}

// ---------------------------------------------------------------- keys

func cmdDel(c *client, a [][]byte) {
	keys := a[1:]
	c.lockKeys(keys, 1)
	n := 0
	for i, k := range keys {
		h := c.hs[i]
		s := shardOf(h)
		idx, it, _ := s.get(k, h, c.now)
		if it != 0 {
			s.del(idx, it)
			n++
		}
	}
	c.unlockKeys()
	c.addInt(int64(n))
}

func cmdExists(c *client, a [][]byte) {
	keys := a[1:]
	c.lockKeys(keys, 1)
	n := 0
	for i, k := range keys {
		h := c.hs[i]
		if _, it, _ := shardOf(h).get(k, h, c.now); it != 0 {
			n++
		}
	}
	c.unlockKeys()
	c.addInt(int64(n))
}

func cmdType(c *client, a [][]byte) {
	h := hashKey(a[1])
	s := shardOf(h)
	s.mu.Lock()
	_, it, _ := s.get(a[1], h, c.now)
	t := "none"
	if it != 0 {
		t = [...]string{"string", "list", "hash"}[it.typ()]
	}
	s.mu.Unlock()
	c.addSimple(t)
}

func cmdExpire(c *client, a [][]byte, unit int64, name string) {
	v, ok := parseInt(a[2])
	if !ok {
		c.addErr(errNotInt)
		return
	}
	if v > math.MaxInt64/unit || v < math.MinInt64/unit {
		c.addErr("ERR invalid expire time in '" + name + "' command")
		return
	}
	v *= unit
	if (v > 0 && v > math.MaxInt64-c.now) || (v < 0 && c.now+v > c.now) {
		c.addErr("ERR invalid expire time in '" + name + "' command")
		return
	}
	when := c.now + v
	h := hashKey(a[1])
	s := shardOf(h)
	s.mu.Lock()
	idx, it, _ := s.get(a[1], h, c.now)
	if it == 0 {
		s.mu.Unlock()
		c.addInt(0)
		return
	}
	if when <= c.now {
		s.del(idx, it)
	} else {
		s.setExp(it, when)
	}
	s.mu.Unlock()
	c.addInt(1)
}

func cmdTTL(c *client, a [][]byte, secs bool) {
	h := hashKey(a[1])
	s := shardOf(h)
	s.mu.Lock()
	_, it, _ := s.get(a[1], h, c.now)
	var r int64
	switch {
	case it == 0:
		r = -2
	case it.exp() == 0:
		r = -1
	default:
		r = it.exp() - c.now
		if r < 0 {
			r = 0
		}
		if secs {
			r = (r + 500) / 1000
		}
	}
	s.mu.Unlock()
	c.addInt(r)
}

func cmdPersist(c *client, a [][]byte) {
	h := hashKey(a[1])
	s := shardOf(h)
	s.mu.Lock()
	_, it, _ := s.get(a[1], h, c.now)
	r := int64(0)
	if it != 0 && it.exp() != 0 {
		s.setExp(it, 0)
		r = 1
	}
	s.mu.Unlock()
	c.addInt(r)
}

func cmdKeys(c *client, a [][]byte) {
	pat := a[1]
	var out []byte
	n := 0
	for i := range shards {
		s := &shards[i]
		s.mu.Lock()
		for _, v := range s.slots {
			if v > tomb {
				it := item(v & ptrMask)
				if e := it.exp(); e != 0 && e <= c.now {
					continue
				}
				k := it.key()
				if globMatch(pat, k) {
					out = append(out, '$')
					out = strconv.AppendInt(out, int64(len(k)), 10)
					out = append(out, '\r', '\n')
					out = append(out, k...)
					out = append(out, '\r', '\n')
					n++
				}
			}
		}
		s.mu.Unlock()
	}
	c.addArr(n)
	c.out = append(c.out, out...)
}

// globMatch is a port of Redis' stringmatchlen.
func globMatch(p, s []byte) bool {
	pi, si := 0, 0
	for pi < len(p) && si < len(s) {
		switch p[pi] {
		case '*':
			for pi+1 < len(p) && p[pi+1] == '*' {
				pi++
			}
			if pi+1 == len(p) {
				return true
			}
			for si < len(s) {
				if globMatch(p[pi+1:], s[si:]) {
					return true
				}
				si++
			}
			return false
		case '?':
		case '[':
			pi++
			not := pi < len(p) && p[pi] == '^'
			if not {
				pi++
			}
			match := false
			for {
				if pi+1 < len(p) && p[pi] == '\\' {
					pi++
					if p[pi] == s[si] {
						match = true
					}
				} else if pi >= len(p) {
					pi--
					break
				} else if p[pi] == ']' {
					break
				} else if pi+2 < len(p) && p[pi+1] == '-' {
					lo, hi := p[pi], p[pi+2]
					if lo > hi {
						lo, hi = hi, lo
					}
					if s[si] >= lo && s[si] <= hi {
						match = true
					}
					pi += 2
				} else if p[pi] == s[si] {
					match = true
				}
				pi++
			}
			if not {
				match = !match
			}
			if !match {
				return false
			}
		case '\\':
			if pi+1 < len(p) {
				pi++
			}
			if p[pi] != s[si] {
				return false
			}
		default:
			if p[pi] != s[si] {
				return false
			}
		}
		pi++
		si++
		if si == len(s) {
			for pi < len(p) && p[pi] == '*' {
				pi++
			}
			break
		}
	}
	return pi == len(p) && si == len(s)
}

// ---------------------------------------------------------------- lists

func (l *listObj) resize(n int) {
	nb := make([]string, n)
	for i := 0; i < l.n; i++ {
		nb[i] = l.buf[(l.head+i)&(len(l.buf)-1)]
	}
	l.buf, l.head = nb, 0
}

func (l *listObj) at(i int) string { return l.buf[(l.head+i)&(len(l.buf)-1)] }

func (l *listObj) push(v string, front bool) {
	if l.n == len(l.buf) {
		l.resize(max(4, len(l.buf)*2))
	}
	mask := len(l.buf) - 1
	if front {
		l.head = (l.head - 1) & mask
		l.buf[l.head] = v
	} else {
		l.buf[(l.head+l.n)&mask] = v
	}
	l.n++
}

func (l *listObj) pop(front bool) string {
	mask := len(l.buf) - 1
	var i int
	if front {
		i = l.head
		l.head = (l.head + 1) & mask
	} else {
		i = (l.head + l.n - 1) & mask
	}
	v := l.buf[i]
	l.buf[i] = ""
	l.n--
	if len(l.buf) > 16 && l.n < len(l.buf)/4 {
		l.resize(len(l.buf) / 2)
	}
	return v
}

func cmdPush(c *client, a [][]byte, front bool) {
	key := a[1]
	h := hashKey(key)
	s := shardOf(h)
	s.mu.Lock()
	s.grow()
	_, it, ins := s.get(key, h, c.now)
	if it != 0 && it.typ() != typList {
		s.mu.Unlock()
		c.addErr(errWrongType)
		return
	}
	var delta int64
	for _, v := range a[2:] {
		delta += int64(16 + len(v))
	}
	if maxMemory > 0 {
		cur := int64(64 + len(key))
		if it != 0 {
			cur = s.cost(it)
		}
		if cur+delta > maxMemory {
			s.mu.Unlock()
			c.addErr(errOOM)
			return
		}
	}
	var l *listObj
	if it == 0 {
		l = &listObj{}
		s.newContainer(ins, h, key, typList, l, c.now)
	} else {
		l = s.objs[it.objIdx()].(*listObj)
	}
	for _, v := range a[2:] {
		l.push(string(v), front)
	}
	l.cost += delta
	s.addUsed(delta)
	n := l.n
	s.mu.Unlock()
	c.addInt(int64(n))
	c.afterWrite(key)
}

func cmdPop(c *client, a [][]byte, front bool) {
	if len(a) > 3 {
		c.addErr("ERR wrong number of arguments for '" + [...]string{"rpop", "lpop"}[b2i(front)] + "' command")
		return
	}
	count := int64(-1)
	if len(a) == 3 {
		v, ok := parseInt(a[2])
		if !ok || v < 0 {
			c.addErr(errPositive)
			return
		}
		count = v
	}
	h := hashKey(a[1])
	s := shardOf(h)
	s.mu.Lock()
	idx, it, _ := s.get(a[1], h, c.now)
	if it == 0 {
		s.mu.Unlock()
		if count >= 0 {
			c.addNullArr()
		} else {
			c.addNull()
		}
		return
	}
	if it.typ() != typList {
		s.mu.Unlock()
		c.addErr(errWrongType)
		return
	}
	l := s.objs[it.objIdx()].(*listObj)
	var delta int64
	if count < 0 {
		v := l.pop(front)
		delta += int64(16 + len(v))
		c.addBulkStr(v)
	} else {
		n := int(min(count, int64(l.n)))
		c.addArr(n)
		for i := 0; i < n; i++ {
			v := l.pop(front)
			delta += int64(16 + len(v))
			c.addBulkStr(v)
		}
	}
	l.cost -= delta
	s.addUsed(-delta)
	if l.n == 0 {
		s.del(idx, it)
	}
	s.mu.Unlock()
}

func b2i(b bool) int {
	if b {
		return 1
	}
	return 0
}

// lookupTyped locks the key's shard and returns the item if it has type typ.
// On a type mismatch it writes WRONGTYPE and returns ok=false. The caller must
// unlock s when ok is true.
func lookupTyped(c *client, key []byte, typ uint8) (s *shard, idx int, it item, ok bool) {
	h := hashKey(key)
	s = shardOf(h)
	s.mu.Lock()
	idx, it, _ = s.get(key, h, c.now)
	if it != 0 && it.typ() != typ {
		s.mu.Unlock()
		c.addErr(errWrongType)
		return nil, 0, 0, false
	}
	return s, idx, it, true
}

func cmdLlen(c *client, a [][]byte) {
	s, _, it, ok := lookupTyped(c, a[1], typList)
	if !ok {
		return
	}
	n := 0
	if it != 0 {
		n = s.objs[it.objIdx()].(*listObj).n
	}
	s.mu.Unlock()
	c.addInt(int64(n))
}

func cmdLrange(c *client, a [][]byte) {
	start, ok1 := parseInt(a[2])
	stop, ok2 := parseInt(a[3])
	if !ok1 || !ok2 {
		c.addErr(errNotInt)
		return
	}
	s, _, it, ok := lookupTyped(c, a[1], typList)
	if !ok {
		return
	}
	if it == 0 {
		s.mu.Unlock()
		c.addArr(0)
		return
	}
	l := s.objs[it.objIdx()].(*listObj)
	n := int64(l.n)
	if start < 0 {
		start += n
	}
	if stop < 0 {
		stop += n
	}
	if start < 0 {
		start = 0
	}
	if start > stop || start >= n {
		s.mu.Unlock()
		c.addArr(0)
		return
	}
	if stop >= n {
		stop = n - 1
	}
	c.addArr(int(stop - start + 1))
	for i := start; i <= stop; i++ {
		c.addBulkStr(l.at(int(i)))
	}
	s.mu.Unlock()
}

func cmdLindex(c *client, a [][]byte) {
	idx, ok := parseInt(a[2])
	if !ok {
		c.addErr(errNotInt)
		return
	}
	s, _, it, ok := lookupTyped(c, a[1], typList)
	if !ok {
		return
	}
	if it == 0 {
		s.mu.Unlock()
		c.addNull()
		return
	}
	l := s.objs[it.objIdx()].(*listObj)
	if idx < 0 {
		idx += int64(l.n)
	}
	if idx < 0 || idx >= int64(l.n) {
		c.addNull()
	} else {
		c.addBulkStr(l.at(int(idx)))
	}
	s.mu.Unlock()
}

// ---------------------------------------------------------------- hashes

func cmdHset(c *client, a [][]byte) {
	if len(a)%2 != 0 {
		c.addErr("ERR wrong number of arguments for '" + strings.ToLower(string(a[0])) + "' command")
		return
	}
	key := a[1]
	h := hashKey(key)
	s := shardOf(h)
	s.mu.Lock()
	s.grow()
	_, it, ins := s.get(key, h, c.now)
	if it != 0 && it.typ() != typHash {
		s.mu.Unlock()
		c.addErr(errWrongType)
		return
	}
	if maxMemory > 0 {
		cur := int64(64 + len(key))
		if it != 0 {
			cur = s.cost(it)
		}
		for i := 2; i < len(a); i += 2 {
			cur += int64(32 + len(a[i]) + len(a[i+1]))
		}
		if cur > maxMemory {
			s.mu.Unlock()
			c.addErr(errOOM)
			return
		}
	}
	var ho *hashObj
	if it == 0 {
		ho = &hashObj{}
		s.newContainer(ins, h, key, typHash, ho, c.now)
	} else {
		ho = s.objs[it.objIdx()].(*hashObj)
	}
	var delta int64
	added := 0
	for i := 2; i < len(a); i += 2 {
		f, v := a[i], a[i+1]
		if old, ok := ho.set(f, string(v)); ok {
			delta += int64(len(v) - len(old))
		} else {
			delta += int64(32 + len(f) + len(v))
			added++
		}
	}
	ho.cost += delta
	s.addUsed(delta)
	s.mu.Unlock()
	c.addInt(int64(added))
	c.afterWrite(key)
}

func cmdHget(c *client, a [][]byte) {
	s, _, it, ok := lookupTyped(c, a[1], typHash)
	if !ok {
		return
	}
	if it == 0 {
		c.addNull()
	} else if v, ok := s.objs[it.objIdx()].(*hashObj).get(a[2]); ok {
		c.addBulkStr(v)
	} else {
		c.addNull()
	}
	s.mu.Unlock()
}

func cmdHdel(c *client, a [][]byte) {
	s, idx, it, ok := lookupTyped(c, a[1], typHash)
	if !ok {
		return
	}
	n := 0
	if it != 0 {
		ho := s.objs[it.objIdx()].(*hashObj)
		var delta int64
		for _, f := range a[2:] {
			if old, ok := ho.del(f); ok {
				delta += int64(32 + len(f) + len(old))
				n++
			}
		}
		ho.cost -= delta
		s.addUsed(-delta)
		if ho.size() == 0 {
			s.del(idx, it)
		}
	}
	s.mu.Unlock()
	c.addInt(int64(n))
}

func cmdHgetall(c *client, a [][]byte) {
	s, _, it, ok := lookupTyped(c, a[1], typHash)
	if !ok {
		return
	}
	if it == 0 {
		c.addArr(0)
	} else {
		ho := s.objs[it.objIdx()].(*hashObj)
		c.addArr(ho.size() * 2)
		if ho.m != nil {
			for f, v := range ho.m {
				c.addBulkStr(f)
				c.addBulkStr(v)
			}
		} else {
			for _, x := range ho.kv {
				c.addBulkStr(x)
			}
		}
	}
	s.mu.Unlock()
}

func cmdHlen(c *client, a [][]byte) {
	s, _, it, ok := lookupTyped(c, a[1], typHash)
	if !ok {
		return
	}
	n := 0
	if it != 0 {
		n = s.objs[it.objIdx()].(*hashObj).size()
	}
	s.mu.Unlock()
	c.addInt(int64(n))
}

func cmdHexists(c *client, a [][]byte) {
	s, _, it, ok := lookupTyped(c, a[1], typHash)
	if !ok {
		return
	}
	n := 0
	if it != 0 {
		if _, ok := s.objs[it.objIdx()].(*hashObj).get(a[2]); ok {
			n = 1
		}
	}
	s.mu.Unlock()
	c.addInt(int64(n))
}

func cmdHincrby(c *client, a [][]byte) {
	delta, ok := parseInt(a[3])
	if !ok {
		c.addErr(errNotInt)
		return
	}
	key, f := a[1], a[2]
	h := hashKey(key)
	s := shardOf(h)
	s.mu.Lock()
	s.grow()
	_, it, ins := s.get(key, h, c.now)
	if it != 0 && it.typ() != typHash {
		s.mu.Unlock()
		c.addErr(errWrongType)
		return
	}
	var ho *hashObj
	var cur int64
	var old string
	exists := false
	if it != 0 {
		ho = s.objs[it.objIdx()].(*hashObj)
		if old, exists = ho.get(f); exists {
			v, ok := parseInt([]byte(old))
			if !ok {
				s.mu.Unlock()
				c.addErr(errHashInt)
				return
			}
			cur = v
		}
	}
	if (delta > 0 && cur > math.MaxInt64-delta) || (delta < 0 && cur < math.MinInt64-delta) {
		s.mu.Unlock()
		c.addErr(errOverflow)
		return
	}
	cur += delta
	nv := strconv.FormatInt(cur, 10)
	if maxMemory > 0 {
		est := int64(64 + len(key) + 32 + len(f) + len(nv))
		if it != 0 {
			est += s.cost(it)
		}
		if est > maxMemory {
			s.mu.Unlock()
			c.addErr(errOOM)
			return
		}
	}
	if ho == nil {
		ho = &hashObj{}
		s.newContainer(ins, h, key, typHash, ho, c.now)
	}
	var d int64
	if exists {
		d = int64(len(nv) - len(old))
	} else {
		d = int64(32 + len(f) + len(nv))
	}
	ho.set(f, nv)
	ho.cost += d
	s.addUsed(d)
	s.mu.Unlock()
	c.addInt(cur)
	c.afterWrite(key)
}
