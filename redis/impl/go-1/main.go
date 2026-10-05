package main

import (
	"fmt"
	"net"
	"os"
	"runtime"
	"strconv"
	"sync"
	"sync/atomic"
	"time"
)

var connectedClients atomic.Int64

func main() {
	port := os.Getenv("PORT")
	if port == "" {
		port = "6380"
	}
	if t, err := strconv.Atoi(os.Getenv("THREADS")); err == nil && t > 0 {
		runtime.GOMAXPROCS(t)
	}
	if m, err := strconv.ParseInt(os.Getenv("MAXMEMORY"), 10, 64); err == nil && m > 0 {
		maxMemory = m
	}
	if len(classSize) > len(shards[0].free) {
		panic("too many size classes")
	}
	ln, err := net.Listen("tcp", "0.0.0.0:"+port)
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
	go expiryLoop()
	for {
		conn, err := ln.Accept()
		if err != nil {
			time.Sleep(10 * time.Millisecond)
			continue
		}
		go serve(conn)
	}
}

type client struct {
	conn net.Conn
	rbuf []byte
	r, w int
	out  []byte
	args [][]byte
	now  int64
	quit bool

	num   []byte
	hs    []uint64
	locks []int
	keys  [][]byte

	// pub/sub: once subscribed, all output goes through pending + writer goroutine
	subs    map[string]struct{}
	async   bool
	mu      sync.Mutex
	pending []byte
	sig     chan struct{}
	dead    bool
}

const (
	rbufInit = 4096
	outMax   = 64 << 10
)

func serve(conn net.Conn) {
	connectedClients.Add(1)
	c := &client{conn: conn, rbuf: make([]byte, rbufInit), out: make([]byte, 0, 1024)}
	for {
		n, err := conn.Read(c.rbuf[c.w:])
		if n > 0 {
			c.w += n
			c.now = time.Now().UnixMilli()
			if !c.process() {
				break
			}
		}
		if err != nil {
			break
		}
	}
	c.cleanup()
	connectedClients.Add(-1)
}

// process executes all complete commands in the input buffer.
func (c *client) process() bool {
	for c.r < c.w {
		args, n, st := parseCommand(c.rbuf[c.r:c.w], c.args[:0])
		if st == parseIncomplete {
			break
		}
		if st == parseError {
			c.addErr("ERR Protocol error: invalid request")
			c.flush()
			return false
		}
		c.r += n
		c.args = args
		if len(args) > 0 {
			c.exec(args)
			if c.quit {
				c.flush()
				return false
			}
			if len(c.out) > outMax && !c.flush() {
				return false
			}
		}
	}
	for i := range c.args {
		c.args[i] = nil
	}
	if !c.flush() {
		return false
	}
	if c.r == c.w {
		c.r, c.w = 0, 0
		if len(c.rbuf) > 256<<10 {
			c.rbuf = make([]byte, rbufInit)
		}
	} else if c.r > 0 {
		copy(c.rbuf, c.rbuf[c.r:c.w])
		c.w -= c.r
		c.r = 0
	}
	if c.w == len(c.rbuf) {
		nb := make([]byte, len(c.rbuf)*2)
		copy(nb, c.rbuf[:c.w])
		c.rbuf = nb
	}
	return true
}

func (c *client) flush() bool {
	if len(c.out) == 0 {
		return true
	}
	if c.async {
		c.pushAsync()
		return true
	}
	_, err := c.conn.Write(c.out)
	if cap(c.out) > 1<<20 {
		c.out = make([]byte, 0, 4096)
	} else {
		c.out = c.out[:0]
	}
	return err == nil
}

// ---------------------------------------------------------------- async output (pub/sub)

const maxPending = 32 << 20

func (c *client) startAsync() {
	if c.async {
		return
	}
	c.flush()
	c.async = true
	c.sig = make(chan struct{}, 1)
	go c.writer()
}

// deliver appends data to the pending output. Caller must hold c.mu.
func (c *client) deliverLocked(b []byte) {
	if c.dead {
		return
	}
	c.pending = append(c.pending, b...)
	if len(c.pending) > maxPending {
		c.dead = true
		c.pending = nil
		close(c.sig)
		c.conn.Close()
		return
	}
	select {
	case c.sig <- struct{}{}:
	default:
	}
}

func (c *client) pushAsync() {
	c.mu.Lock()
	c.deliverLocked(c.out)
	c.mu.Unlock()
	c.out = c.out[:0]
}

func (c *client) writer() {
	var spare []byte
	for range c.sig {
		c.mu.Lock()
		buf := c.pending
		c.pending = spare[:0]
		c.mu.Unlock()
		if len(buf) > 0 {
			if _, err := c.conn.Write(buf); err != nil {
				c.conn.Close()
			}
		}
		if cap(buf) > 1<<20 {
			buf = nil
		}
		spare = buf
	}
	c.conn.Close()
}

func (c *client) cleanup() {
	if len(c.subs) > 0 {
		pubsubUnsubAll(c)
	}
	if c.async {
		c.conn.SetWriteDeadline(time.Now().Add(5 * time.Second))
		c.mu.Lock()
		if !c.dead {
			c.dead = true
			close(c.sig)
		}
		c.mu.Unlock()
		return // writer closes the connection after draining
	}
	c.conn.Close()
}

// ---------------------------------------------------------------- RESP parsing

const (
	parseOK = iota
	parseIncomplete
	parseError
)

// parseLine parses an integer terminated by \r\n starting at b[i].
func parseLine(b []byte, i int) (n int64, next int, st int) {
	neg := false
	if i < len(b) && b[i] == '-' {
		neg = true
		i++
	}
	start := i
	for ; i < len(b); i++ {
		ch := b[i]
		if ch == '\r' {
			if i+1 >= len(b) {
				return 0, 0, parseIncomplete
			}
			if b[i+1] != '\n' || i == start {
				return 0, 0, parseError
			}
			if neg {
				n = -n
			}
			return n, i + 2, parseOK
		}
		if ch < '0' || ch > '9' || i-start > 18 {
			return 0, 0, parseError
		}
		n = n*10 + int64(ch-'0')
	}
	if i-start > 19 {
		return 0, 0, parseError
	}
	return 0, 0, parseIncomplete
}

func parseCommand(b []byte, args [][]byte) ([][]byte, int, int) {
	if b[0] != '*' {
		return args, 0, parseError
	}
	n, i, st := parseLine(b, 1)
	if st != parseOK {
		return args, 0, st
	}
	if n > 1024*1024 {
		return args, 0, parseError
	}
	for k := int64(0); k < n; k++ {
		if i >= len(b) {
			return args, 0, parseIncomplete
		}
		if b[i] != '$' {
			return args, 0, parseError
		}
		l, j, st := parseLine(b, i+1)
		if st != parseOK {
			return args, 0, st
		}
		if l < 0 || l > 512<<20 {
			return args, 0, parseError
		}
		end := j + int(l)
		if end+2 > len(b) {
			return args, 0, parseIncomplete
		}
		if b[end] != '\r' || b[end+1] != '\n' {
			return args, 0, parseError
		}
		args = append(args, b[j:end:end])
		i = end + 2
	}
	return args, i, parseOK
}

// ---------------------------------------------------------------- replies

func (c *client) addRaw(s string) { c.out = append(c.out, s...) }

func (c *client) addSimple(s string) {
	c.out = append(c.out, '+')
	c.out = append(c.out, s...)
	c.out = append(c.out, '\r', '\n')
}

func (c *client) addErr(s string) {
	c.out = append(c.out, '-')
	c.out = append(c.out, s...)
	c.out = append(c.out, '\r', '\n')
}

func (c *client) addInt(n int64) {
	c.out = append(c.out, ':')
	c.out = strconv.AppendInt(c.out, n, 10)
	c.out = append(c.out, '\r', '\n')
}

func (c *client) addLen(prefix byte, n int) {
	c.out = append(c.out, prefix)
	c.out = strconv.AppendInt(c.out, int64(n), 10)
	c.out = append(c.out, '\r', '\n')
}

func (c *client) addBulk(b []byte) {
	c.addLen('$', len(b))
	c.out = append(c.out, b...)
	c.out = append(c.out, '\r', '\n')
}

func (c *client) addBulkStr(b string) {
	c.addLen('$', len(b))
	c.out = append(c.out, b...)
	c.out = append(c.out, '\r', '\n')
}

func (c *client) addNull()     { c.out = append(c.out, "$-1\r\n"...) }
func (c *client) addNullArr()  { c.out = append(c.out, "*-1\r\n"...) }
func (c *client) addArr(n int) { c.addLen('*', n) }
func (c *client) addOK()       { c.out = append(c.out, "+OK\r\n"...) }
