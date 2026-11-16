// Package server is a Liveless Rooms server: games log in for a code, and a
// game that sends another's code is told where to find it. It never carries
// game traffic. Behaviour follows band3's tools/liveless_rooms_mock.py.
package server

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/netip"
	"sync"
	"time"
)

// ErrServerClosed is what Serve returns once Shutdown has been called.
var ErrServerClosed = errors.New("server closed")

type Server struct {
	cfg Config
	log func(string)

	mu       sync.Mutex
	ln       net.Listener
	conns    map[*conn]struct{}
	perIP    map[netip.Addr]int
	byCode   map[string]*conn
	ipMisses map[netip.Addr]*bucket
	count    uint64
	shutting bool

	wg       sync.WaitGroup // every connection's goroutines, and the sweeper
	stop     chan struct{}
	stopOnce sync.Once
}

// Stats is what the health endpoint reports.
type Stats struct {
	Connections int `json:"connections"`
	LoggedIn    int `json:"logged_in"`
}

// New makes a server; log gets one line per event, without a timestamp.
func New(cfg Config, log func(string)) (*Server, error) {
	if err := cfg.Validate(); err != nil {
		return nil, err
	}
	if cfg.FrameGap <= 0 {
		cfg.FrameGap = DefaultFrameGap
	}
	if cfg.NewCode == nil {
		cfg.NewCode = RandomCode
	}
	if cfg.Now == nil {
		cfg.Now = time.Now
	}
	if log == nil {
		log = func(string) {}
	}
	s := &Server{
		cfg:      cfg,
		log:      log,
		conns:    map[*conn]struct{}{},
		perIP:    map[netip.Addr]int{},
		byCode:   map[string]*conn{},
		ipMisses: map[netip.Addr]*bucket{},
		stop:     make(chan struct{}),
	}
	s.wg.Add(1)
	go s.sweep()
	return s, nil
}

func (s *Server) logf(format string, args ...any) {
	s.log(fmt.Sprintf(format, args...))
}

// Serve accepts connections on ln until Shutdown, then returns ErrServerClosed.
func (s *Server) Serve(ln net.Listener) error {
	s.mu.Lock()
	if s.shutting {
		s.mu.Unlock()
		ln.Close()
		return ErrServerClosed
	}
	s.ln = ln
	s.mu.Unlock()

	var delay time.Duration
	for {
		nc, err := ln.Accept()
		if err != nil {
			if s.isShutting() {
				return ErrServerClosed
			}
			if errors.Is(err, net.ErrClosed) {
				return err
			}
			// out of file descriptors and the like: wait and try again, as net/http does
			delay = min(max(2*delay, 5*time.Millisecond), time.Second)
			s.logf("accept failed: %v; trying again in %v", err, delay)
			select {
			case <-time.After(delay):
			case <-s.stop:
			}
			continue
		}
		delay = 0
		s.accept(nc)
	}
}

func (s *Server) isShutting() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.shutting
}

func (s *Server) accept(nc net.Conn) {
	var peer netip.AddrPort
	if tcp, ok := nc.RemoteAddr().(*net.TCPAddr); ok {
		peer = tcp.AddrPort()
	}
	ip := peer.Addr().Unmap()

	s.mu.Lock()
	s.count++
	number := s.count
	why := ""
	switch {
	case s.shutting:
		why = "server stopping"
	case !ip.Is4():
		// every address in the protocol is IPv4
		why = "not an IPv4 address"
	case s.cfg.MaxConns > 0 && len(s.conns) >= s.cfg.MaxConns:
		why = fmt.Sprintf("server full (%d connections)", len(s.conns))
	case s.cfg.MaxConnsPerIP > 0 && s.perIP[ip] >= s.cfg.MaxConnsPerIP:
		why = fmt.Sprintf("%d connections from that address already", s.perIP[ip])
	}
	if why != "" {
		s.mu.Unlock()
		nc.Close()
		s.logf("refuse #%d from %s: %s", number, peer, why)
		return
	}
	c := newConn(s, number, nc, ip, peer)
	s.conns[c] = struct{}{}
	s.perIP[ip]++
	s.wg.Add(2)
	s.mu.Unlock()

	s.logf("connect #%d from %s", number, peer)
	go c.writeLoop()
	go c.readLoop()
}

// forget takes a closed connection out of the server's tables.
func (s *Server) forget(c *conn) (tag string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	delete(s.conns, c)
	if s.perIP[c.ip]--; s.perIP[c.ip] <= 0 {
		delete(s.perIP, c.ip)
	}
	if c.code != "" && s.byCode[c.code] == c {
		delete(s.byCode, c.code)
	}
	return c.tag
}

// Shutdown stops accepting, closes every connection, and waits for their
// goroutines to end or ctx to be done.
func (s *Server) Shutdown(ctx context.Context) error {
	s.mu.Lock()
	s.shutting = true
	ln := s.ln
	conns := make([]*conn, 0, len(s.conns))
	for c := range s.conns {
		conns = append(conns, c)
	}
	s.mu.Unlock()

	if ln != nil {
		ln.Close()
	}
	s.stopOnce.Do(func() { close(s.stop) })
	for _, c := range conns {
		c.close("server stopping")
	}
	done := make(chan struct{})
	go func() {
		s.wg.Wait()
		close(done)
	}()
	select {
	case <-done:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

// Stats counts connections and logged-in games.
func (s *Server) Stats() Stats {
	s.mu.Lock()
	defer s.mu.Unlock()
	return Stats{Connections: len(s.conns), LoggedIn: len(s.byCode)}
}

// sweep forgets per-address join counts that have refilled, so the table only
// holds addresses that missed lately.
func (s *Server) sweep() {
	defer s.wg.Done()
	t := time.NewTicker(time.Minute)
	defer t.Stop()
	for {
		select {
		case <-s.stop:
			return
		case <-t.C:
			now := s.cfg.Now()
			s.mu.Lock()
			for ip, b := range s.ipMisses {
				if b.full(s.cfg.IPJoinMisses, now) {
					delete(s.ipMisses, ip)
				}
			}
			s.mu.Unlock()
		}
	}
}

// publicIP is the address a game is told it has, and that joiners are sent to.
func (s *Server) publicIP(ip netip.Addr) netip.Addr {
	if s.cfg.LANPublicIP.IsValid() && (ip.IsPrivate() || ip.IsLoopback() || ip.IsLinkLocalUnicast()) {
		return s.cfg.LANPublicIP
	}
	return ip
}

// register gives a logged-in connection a code no one connected has.
func (s *Server) register(c *conn, tag string) (string, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	for range 1000 {
		code := s.cfg.NewCode()
		if _, taken := s.byCode[code]; !taken && code != "" {
			c.code, c.tag = code, tag
			s.byCode[code] = c
			return code, nil
		}
	}
	return "", errors.New("no free code")
}
