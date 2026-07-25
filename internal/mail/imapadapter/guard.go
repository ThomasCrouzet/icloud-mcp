package imapadapter

import (
	"errors"
	"net"
	"strconv"
	"strings"
)

const (
	maxSessionBytes = 4 * 1024 * 1024
	maxProtocolLine = 1024 * 1024
	// FETCH adds protocol lists around BODYSTRUCTURE's maximum MIME depth.
	maxProtocolDepth = 24
	maxProtocolLists = 512
	maxQuotedWire    = 2*4*1024 + 2
)

var errInboundLimit = errors.New("IMAP inbound limit exceeded")

// guardedConn rejects oversized sessions and deeply nested protocol lists
// before go-imap materializes recursive BODYSTRUCTURE values. Literal payloads
// are excluded from protocol nesting checks.
type guardedConn struct {
	net.Conn
	total            int
	line             []byte
	literalRemaining int64
	depth            int
	lists            int
	quoted           bool
	quotedLength     int
	escaped          bool
}

func newGuardedConn(conn net.Conn) net.Conn {
	return &guardedConn{Conn: conn, line: make([]byte, 0, 4096)}
}

func (c *guardedConn) Read(p []byte) (int, error) {
	n, err := c.Conn.Read(p)
	accepted := 0
	for accepted < n {
		if c.total >= maxSessionBytes {
			return accepted, errInboundLimit
		}
		b := p[accepted]
		c.total++
		accepted++
		if c.literalRemaining > 0 {
			c.literalRemaining--
			continue
		}
		if scanErr := c.scanProtocolByte(b); scanErr != nil {
			return accepted - 1, scanErr
		}
	}
	return n, err
}

func (c *guardedConn) scanProtocolByte(b byte) error {
	c.line = append(c.line, b)
	if len(c.line) > maxProtocolLine {
		return errInboundLimit
	}
	if c.quoted {
		c.quotedLength++
		if c.quotedLength > maxQuotedWire {
			return errInboundLimit
		}
		if c.escaped {
			c.escaped = false
		} else if b == '\\' {
			c.escaped = true
		} else if b == '"' {
			c.quoted = false
		}
	} else {
		switch b {
		case '"':
			c.quoted = true
			c.quotedLength = 0
		case '(':
			c.depth++
			c.lists++
			if c.depth > maxProtocolDepth {
				return errInboundLimit
			}
			if c.lists > maxProtocolLists {
				return errInboundLimit
			}
		case ')':
			c.depth--
			if c.depth < 0 {
				return errInboundLimit
			}
		}
	}
	if len(c.line) >= 2 && c.line[len(c.line)-2] == '\r' && b == '\n' {
		size, err := literalSize(c.line[:len(c.line)-2])
		if err != nil {
			return err
		}
		c.literalRemaining = size
		c.line = c.line[:0]
		c.quoted = false
		c.quotedLength = 0
		c.escaped = false
		if c.literalRemaining == 0 && c.depth == 0 {
			c.lists = 0
		}
	}
	return nil
}

func literalSize(line []byte) (int64, error) {
	s := string(line)
	end := strings.LastIndexByte(s, '}')
	if end <= 0 || end != len(s)-1 {
		return 0, nil
	}
	start := strings.LastIndexByte(s[:end], '{')
	if start < 0 {
		return 0, nil
	}
	number := strings.TrimSuffix(s[start+1:end], "+")
	size, err := strconv.ParseInt(number, 10, 64)
	if err != nil || size < 0 {
		return 0, nil
	}
	if size > maxSessionBytes {
		return 0, errInboundLimit
	}
	return size, nil
}
