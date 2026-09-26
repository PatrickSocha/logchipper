package syslog

import (
	"bufio"
	"errors"
	"fmt"
	"log"
	"net"
	"regexp"
	"strconv"
	"strings"
	"time"

	"logchipper/internal/allowlist"
	"logchipper/internal/db"
	"logchipper/internal/handler"
)

// RFC 3164: <priority>Mon Jan _2 15:04:05 hostname tag[pid]: message
// Also matches ISO 8601: <priority>YYYY-MM-DDTHH:MM:SS±HH:MM hostname tag[pid]: message
var rfc3164 = regexp.MustCompile(`^<(\d+)>([^\s]+(?:\s+\d+\s+[\d:]+)?)\s+(\S+)\s+(\S+?)(?:\[(\d+)\])?:\s*(.*)$`)

type Server struct {
	Allow  *allowlist.List
	DB     *db.DB
	Broker *handler.Broker

	MaxMessage int // max bytes per message/line
	MaxConns   int // max concurrent TCP connections

	sem chan struct{}
}

func levelFromPriority(pri int) string {
	// priority = facility*8 + severity
	severity := pri & 0x07
	switch severity {
	case 0:
		return "error" // Emergency
	case 1:
		return "error" // Alert
	case 2:
		return "error" // Critical
	case 3:
		return "error" // Error
	case 4:
		return "warn" // Warning
	case 5:
		return "notice" // Notice
	case 6:
		return "info" // Informational
	case 7:
		return "debug" // Debug
	default:
		return "info"
	}
}

func (s *Server) parseLine(raw string) db.Event {
	raw = strings.TrimSpace(raw)
	m := rfc3164.FindStringSubmatch(raw)

	e := db.Event{
		CreatedAt: time.Now(),
		Level:     "info",
	}

	if m == nil {
		// Fallback: treat entire line as message
		e.Message = raw
		e.Source = "syslog"
		return e
	}

	pri, _ := strconv.Atoi(m[1])
	e.Level = levelFromPriority(pri)

	tag := strings.TrimRight(m[4], ":")
	msg := m[6]

	e.Source = tag
	e.Message = msg

	// Use the sender's timestamp as given; if it can't be parsed, e.CreatedAt
	// keeps the receipt time set above.
	if ts, err := time.Parse(time.RFC3339, m[2]); err == nil {
		e.CreatedAt = ts
	} else if ts, err := time.Parse("Jan  2 15:04:05", m[2]); err == nil {
		now := time.Now()
		e.CreatedAt = time.Date(now.Year(), ts.Month(), ts.Day(),
			ts.Hour(), ts.Minute(), ts.Second(), 0, time.Local)
	} else if ts, err := time.Parse("Jan _2 15:04:05", m[2]); err == nil {
		now := time.Now()
		e.CreatedAt = time.Date(now.Year(), ts.Month(), ts.Day(),
			ts.Hour(), ts.Minute(), ts.Second(), 0, time.Local)
	}

	return e
}

func (s *Server) handle(raw string) {
	if strings.TrimSpace(raw) == "" {
		return
	}
	e := s.parseLine(raw)
	id, err := s.DB.Insert(e)
	if err != nil {
		log.Printf("syslog insert error: %v", err)
		return
	}
	e.ID = id
	s.Broker.Publish(e)
}

func (s *Server) ListenUDP(addr string) error {
	conn, err := net.ListenPacket("udp", addr)
	if err != nil {
		return fmt.Errorf("syslog udp listen %s: %w", addr, err)
	}
	log.Printf("syslog UDP listening on %s", addr)
	go func() {
		buf := make([]byte, 65536)
		for {
			n, from, err := conn.ReadFrom(buf)
			if err != nil {
				log.Printf("syslog udp read: %v", err)
				continue
			}
			if !s.Allow.AllowsAddr(from.String()) {
				continue
			}
			s.handle(string(buf[:n]))
		}
	}()
	return nil
}

func (s *Server) ListenTCP(addr string) error {
	ln, err := net.Listen("tcp", addr)
	if err != nil {
		return fmt.Errorf("syslog tcp listen %s: %w", addr, err)
	}
	log.Printf("syslog TCP listening on %s", addr)
	s.sem = make(chan struct{}, s.MaxConns)
	go func() {
		for {
			conn, err := ln.Accept()
			if err != nil {
				if errors.Is(err, net.ErrClosed) {
					return
				}
				log.Printf("syslog tcp accept: %v", err)
				time.Sleep(100 * time.Millisecond)
				continue
			}
			if !s.Allow.AllowsAddr(conn.RemoteAddr().String()) {
				conn.Close()
				continue
			}
			select {
			case s.sem <- struct{}{}:
				go func() {
					defer func() { <-s.sem }()
					s.handleTCPConn(conn)
				}()
			default:
				conn.Close() // over MaxConns
			}
		}
	}()
	return nil
}

// handleTCPConn reads newline-delimited messages. A line longer than
// MaxMessage, or a stalled client, closes the connection.
func (s *Server) handleTCPConn(conn net.Conn) {
	defer conn.Close()
	const idle = 60 * time.Second
	conn.SetDeadline(time.Now().Add(idle))

	sc := bufio.NewScanner(conn)
	sc.Buffer(make([]byte, 0, 4096), s.MaxMessage)
	for sc.Scan() {
		s.handle(sc.Text())
		conn.SetDeadline(time.Now().Add(idle))
	}
	if err := sc.Err(); errors.Is(err, bufio.ErrTooLong) {
		log.Printf("syslog tcp: line from %s exceeds %d bytes, closing", conn.RemoteAddr(), s.MaxMessage)
	}
}
