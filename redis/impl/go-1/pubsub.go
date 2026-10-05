package main

import (
	"strconv"
	"sync"
)

var pubsub struct {
	mu    sync.RWMutex
	chans map[string]map[*client]struct{}
}

func init() { pubsub.chans = make(map[string]map[*client]struct{}) }

func appendPushHdr(b []byte, kind string, ch []byte) []byte {
	b = append(b, "*3\r\n$"...)
	b = strconv.AppendInt(b, int64(len(kind)), 10)
	b = append(b, '\r', '\n')
	b = append(b, kind...)
	b = append(b, "\r\n$"...)
	b = strconv.AppendInt(b, int64(len(ch)), 10)
	b = append(b, '\r', '\n')
	b = append(b, ch...)
	b = append(b, '\r', '\n')
	return b
}

func cmdSubscribe(c *client, a [][]byte) {
	c.startAsync()
	c.pushAsync()
	if c.subs == nil {
		c.subs = make(map[string]struct{})
	}
	var buf []byte
	for _, ch := range a[1:] {
		pubsub.mu.Lock()
		name := string(ch)
		if _, ok := c.subs[name]; !ok {
			c.subs[name] = struct{}{}
			m := pubsub.chans[name]
			if m == nil {
				m = make(map[*client]struct{})
				pubsub.chans[name] = m
			}
			m[c] = struct{}{}
		}
		buf = appendPushHdr(buf[:0], "subscribe", ch)
		buf = append(buf, ':')
		buf = strconv.AppendInt(buf, int64(len(c.subs)), 10)
		buf = append(buf, '\r', '\n')
		c.mu.Lock()
		c.deliverLocked(buf)
		c.mu.Unlock()
		pubsub.mu.Unlock()
	}
}

func unsubOne(c *client, name string) {
	delete(c.subs, name)
	if m := pubsub.chans[name]; m != nil {
		delete(m, c)
		if len(m) == 0 {
			delete(pubsub.chans, name)
		}
	}
}

func cmdUnsubscribe(c *client, a [][]byte) {
	var names []string
	if len(a) > 1 {
		for _, ch := range a[1:] {
			names = append(names, string(ch))
		}
	} else {
		for n := range c.subs {
			names = append(names, n)
		}
		if len(names) == 0 {
			c.addRaw("*3\r\n$11\r\nunsubscribe\r\n$-1\r\n:0\r\n")
			return
		}
	}
	pubsub.mu.Lock()
	for _, n := range names {
		unsubOne(c, n)
		c.out = appendPushHdr(c.out, "unsubscribe", []byte(n))
		c.addInt(int64(len(c.subs)))
	}
	pubsub.mu.Unlock()
}

func pubsubUnsubAll(c *client) {
	pubsub.mu.Lock()
	for n := range c.subs {
		unsubOne(c, n)
	}
	pubsub.mu.Unlock()
}

func cmdPublish(c *client, a [][]byte) {
	pubsub.mu.RLock()
	m := pubsub.chans[string(a[1])]
	n := 0
	if len(m) > 0 {
		msg := appendPushHdr(nil, "message", a[1])
		msg = append(msg, '$')
		msg = strconv.AppendInt(msg, int64(len(a[2])), 10)
		msg = append(msg, '\r', '\n')
		msg = append(msg, a[2]...)
		msg = append(msg, '\r', '\n')
		for s := range m {
			s.mu.Lock()
			s.deliverLocked(msg)
			s.mu.Unlock()
			n++
		}
	}
	pubsub.mu.RUnlock()
	c.addInt(int64(n))
}
