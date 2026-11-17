package server

import (
	"crypto/rand"
	"errors"
	"fmt"
	"io"
	"net"
	"net/netip"
	"os"
	"strings"
	"sync"
	"time"

	"github.com/idio-sync/band3-rooms/internal/protocol"
)

type outFrame struct {
	data []byte
	// the last frame: half-close after it and let the game hang up
	last bool
}

// conn is one game's connection. readLoop owns everything not marked
// otherwise; writeLoop sends what's put on out, and the pings.
type conn struct {
	s      *Server
	number uint64
	nc     net.Conn
	ip     netip.Addr
	peer   netip.AddrPort

	out       chan outFrame
	done      chan struct{}
	closeOnce sync.Once

	proofKey [protocol.KeySize]byte
	reader   protocol.FrameReader
	hello    *protocol.ClientHello
	// set before the code is registered, and read by other connections'
	// goroutines only once they've found it by its code (under s.mu)
	login  *protocol.ClientLogin
	public netip.Addr
	// set under s.mu
	code, tag string

	// the hello's version is band3's, which knows the server's own packets
	band3 bool
	// when this connection's last port check went
	portChecked time.Time
	// echoes answered (EchoLimit)
	echoes int

	misses bucket // this connection's failed joins
	// turned away: waiting for the game to hang up, then closing with this
	turnedAway string
}

func newConn(s *Server, number uint64, nc net.Conn, ip netip.Addr, peer netip.AddrPort) *conn {
	c := &conn{
		s:      s,
		number: number,
		nc:     nc,
		ip:     ip,
		peer:   peer,
		out:    make(chan outFrame, outboxSize),
		done:   make(chan struct{}),
	}
	_, _ = rand.Read(c.proofKey[:])
	if tcp, ok := nc.(*net.TCPConn); ok {
		// each frame leaves as its own segment (Go's default, said outright)
		_ = tcp.SetNoDelay(true)
	}
	return c
}

// name is how the connection's goroutine names it in logs.
func (c *conn) name() string {
	if c.login == nil {
		return fmt.Sprintf("#%d", c.number)
	}
	return fmt.Sprintf("#%d (%q)", c.number, c.login.Gamertag)
}

func (c *conn) closed() bool {
	select {
	case <-c.done:
		return true
	default:
		return false
	}
}

// close ends the connection once, from any goroutine.
func (c *conn) close(reason string) {
	c.closeOnce.Do(func() {
		close(c.done)
		c.nc.Close()
		tag := c.s.forget(c)
		name := fmt.Sprintf("#%d", c.number)
		if tag != "" {
			name = fmt.Sprintf("#%d (%q)", c.number, tag)
		}
		c.s.logf("close %s: %s", name, reason)
	})
}

// send queues a frame; a game that has fallen too far behind is closed.
func (c *conn) send(f outFrame) {
	if c.closed() {
		return
	}
	select {
	case c.out <- f:
	default:
		c.close(fmt.Sprintf("too far behind (%d frames waiting)", outboxSize))
	}
}

func (c *conn) sendFrame(data []byte) { c.send(outFrame{data: data}) }

// writeLoop sends queued frames and pings, each on its own and at least
// FrameGap apart: RB3Enhanced takes one packet per read, so two that arrive
// together lose the second.
func (c *conn) writeLoop() {
	defer c.s.wg.Done()
	var pings <-chan time.Time
	if c.s.cfg.Ping > 0 {
		t := time.NewTicker(c.s.cfg.Ping)
		defer t.Stop()
		pings = t.C
	}
	gap := time.NewTimer(0)
	defer gap.Stop()
	for {
		var f outFrame
		select {
		case <-c.done:
			return
		case f = <-c.out:
		case <-pings:
			f = outFrame{data: protocol.PingFrame}
		}
		if err := c.write(f.data); err != nil {
			c.close("send failed: " + err.Error())
			return
		}
		if f.last {
			if tcp, ok := c.nc.(*net.TCPConn); ok {
				_ = tcp.CloseWrite()
			}
			<-c.done
			return
		}
		gap.Reset(c.s.cfg.FrameGap)
		select {
		case <-c.done:
			return
		case <-gap.C:
		}
	}
}

func (c *conn) write(data []byte) error {
	_ = c.nc.SetWriteDeadline(time.Now().Add(writeTimeout))
	if _, err := c.nc.Write(data); err != nil {
		return err
	}
	if c.s.cfg.Hex {
		c.s.logf("#%d -> %X", c.number, data)
	}
	return nil
}

// readLoop reads frames and acts on them until the connection ends.
func (c *conn) readLoop() {
	defer c.s.wg.Done()
	cfg := &c.s.cfg
	started := time.Now()
	heard := started
	buf := make([]byte, 4096)
	for !c.closed() {
		if c.turnedAway == "" {
			_ = c.nc.SetReadDeadline(c.deadline(started, heard))
		}
		n, err := c.nc.Read(buf)
		if n > 0 && c.turnedAway == "" {
			heard = time.Now()
			c.reader.Feed(buf[:n])
			for c.turnedAway == "" && !c.closed() {
				typ, body, ok := c.reader.Next()
				if !ok {
					break
				}
				if cfg.Hex {
					c.s.logf("#%d <- %X", c.number, protocol.Frame(typ, body))
				}
				c.handle(typ, body)
			}
			if c.reader.Broken() && !c.closed() {
				head := c.reader.Buffered()
				c.close(fmt.Sprintf("bad magic or size: %X", head[:min(len(head), protocol.HeaderSize)]))
				return
			}
		}
		if err != nil {
			c.close(c.readFailure(err, started))
			return
		}
	}
}

// deadline is when a read gives up: the ping timeout after the last thing
// heard, or the login timeout, whichever comes first (zero: never).
func (c *conn) deadline(started, heard time.Time) time.Time {
	cfg := &c.s.cfg
	var d time.Time
	if cfg.Ping > 0 && cfg.Timeout > 0 {
		d = heard.Add(cfg.Timeout)
	}
	if c.login == nil && cfg.LoginTimeout > 0 {
		if l := started.Add(cfg.LoginTimeout); d.IsZero() || l.Before(d) {
			d = l
		}
	}
	return d
}

// readFailure is why the connection ends, from the read's error.
func (c *conn) readFailure(err error, started time.Time) string {
	cfg := &c.s.cfg
	switch {
	case c.closed():
		return "" // closed elsewhere, which said why
	case c.turnedAway != "":
		return c.turnedAway
	case errors.Is(err, os.ErrDeadlineExceeded):
		if c.login == nil && cfg.LoginTimeout > 0 && !time.Now().Before(started.Add(cfg.LoginTimeout)) {
			return fmt.Sprintf("login timeout: not logged in within %v", cfg.LoginTimeout)
		}
		c.s.logf("ping timeout %s: nothing heard in %v", c.name(), cfg.Timeout)
		return "ping timeout"
	case errors.Is(err, io.EOF):
		return "client disconnected"
	case isReset(err):
		// what a game closing with frames still unread looks like, on Windows most of all
		return "client disconnected (reset)"
	default:
		return "read failed: " + err.Error()
	}
}

func isReset(err error) bool {
	var op *net.OpError
	return errors.As(err, &op) && strings.Contains(strings.ToLower(op.Err.Error()), "reset")
}

func (c *conn) handle(typ uint8, body []byte) {
	if c.hello == nil {
		// the game speaks first, and only to say hello
		var hello protocol.ClientHello
		ok := false
		if typ == protocol.ClientHelloType {
			hello, ok = protocol.DecodeClientHello(body)
		}
		if !ok {
			c.close(fmt.Sprintf("wanted a ClientHello, got type %d of %d bytes", typ, len(body)))
			return
		}
		c.helloed(hello)
		return
	}
	switch typ {
	case protocol.ClientHelloType:
		c.s.logf("hello again from %s, ignored", c.name())
	case protocol.ClientLoginType:
		c.loginWith(body)
	case protocol.PongType:
		// heard from, which is all a pong is for
	case protocol.JoinRequestType:
		c.join(body)
	case protocol.PortCheckRequestType:
		if !c.band3 {
			// only band3 is told of port checks; for anyone else it's a type like any unknown one
			c.s.logf("unknown type %d from %s, ignored", typ, c.name())
			return
		}
		c.portCheck(body)
	case protocol.EchoRequestType:
		if !c.band3 {
			c.s.logf("unknown type %d from %s, ignored", typ, c.name())
			return
		}
		c.echo(body)
	default:
		c.s.logf("unknown type %d from %s, ignored", typ, c.name())
	}
}

func (c *conn) helloed(hello protocol.ClientHello) {
	c.hello = &hello
	kind := "console"
	if hello.Emulator {
		kind = "emulator"
	}
	c.s.logf("hello #%d: %s, %q, %q, protocol %d", c.number, kind, hello.Language, hello.Version, hello.Protocol)
	c.band3 = isBand3(hello.Version)
	if ok, why := c.s.cfg.versionAllowed(hello.Version); !ok {
		c.s.logf("turn away #%d: %s", c.number, why)
		c.turnAway(protocol.EncodeServerHello(protocol.ServerHello{}), "turned away: "+why)
		return
	}
	c.sendFrame(protocol.EncodeServerHello(protocol.ServerHello{
		Allowed: true, NeedsProof: c.s.cfg.RequireProof, ProofKey: c.proofKey,
	}))
	if c.band3 {
		// band3's own packet, which it ignores if it's older than it (and
		// RB3Enhanced would hang up on): what this server is, before any login,
		// for band3's connection test
		c.sendFrame(protocol.EncodeServerInfo(protocol.ServerInfo{Features: serverFeatures, Version: c.s.cfg.Version}))
	}
}

// what this server does beyond RB3Enhanced's protocol, as band3 is told
const serverFeatures = protocol.FeaturePortCheck | protocol.FeatureEcho

// turnAway sends a last frame, then waits a little for the game to hang up
// (both RB3Enhanced and band3 do on a ServerHello that doesn't allow them)
// before closing, so the frame isn't lost to a reset.
func (c *conn) turnAway(last []byte, reason string) {
	c.turnedAway = reason
	_ = c.nc.SetReadDeadline(time.Now().Add(lingerTimeout))
	c.send(outFrame{data: last, last: true})
}

func (c *conn) loginWith(body []byte) {
	if c.login != nil {
		c.s.logf("login again from %s, ignored", c.name())
		return
	}
	login, ok := protocol.DecodeClientLogin(body)
	if !ok {
		c.close(fmt.Sprintf("short ClientLogin (%d bytes)", len(body)))
		return
	}
	// There's no login-failed packet: a game that isn't let in loses its
	// connection. A console signs the hash with its key, which can't be
	// checked here, so consoles are taken or not as a whole.
	cfg := &c.s.cfg
	if c.hello.Emulator {
		if cfg.RequireProof && !c.proofOK(login) {
			c.s.logf("bad proof #%d (%q): not signed for %q", c.number, login.Gamertag, strings.Join(cfg.Addresses, `" or "`))
			c.close("login refused")
			return
		}
	} else if !cfg.AllowConsoles {
		c.s.logf("console login #%d (%q) refused: consoles aren't allowed", c.number, login.Gamertag)
		c.close("login refused")
		return
	}
	c.public = c.s.publicIP(c.ip)
	c.login = &login
	code, err := c.s.register(c, login.Gamertag)
	if err != nil {
		c.close(err.Error())
		return
	}
	c.s.logf("login %s: xuid %016X local %s public %s code %s", c.name(), login.XUID, login.LocalIP, c.public, code)
	c.sendFrame(protocol.EncodeLoggedIn(protocol.LoggedIn{PublicIP: c.public, Code: code}))
	if c.band3 {
		// band3's own packet; RB3Enhanced would hang up on a type it doesn't know
		c.sendFrame(protocol.EncodeServerFeatures(protocol.ServerFeatures{Features: serverFeatures}))
	}
}

func (c *conn) proofOK(login protocol.ClientLogin) bool {
	for _, address := range c.s.cfg.Addresses {
		if protocol.ProofOK(c.proofKey, address, login) {
			return true
		}
	}
	return false
}

func (c *conn) join(body []byte) {
	request, ok := protocol.DecodeJoinRequest(body)
	if !ok {
		c.close(fmt.Sprintf("short UserJoinRequest (%d bytes)", len(body)))
		return
	}
	if c.login == nil {
		c.s.logf("join from %s before logging in, ignored", c.name())
		return
	}
	// players type the code in either case
	code := strings.ToUpper(request.Code)
	s, cfg := c.s, &c.s.cfg
	now := cfg.Now()

	limited := ""
	if cfg.ConnJoinMisses > 0 && !c.misses.allows(cfg.ConnJoinMisses, now) {
		limited = "this connection"
	}
	var host *conn
	var hostName string
	var response protocol.JoinResponse
	s.mu.Lock()
	if limited == "" && cfg.IPJoinMisses > 0 {
		if b := s.ipMisses[c.ip]; b != nil && !b.allows(cfg.IPJoinMisses, now) {
			limited = "its address"
		}
	}
	if limited == "" {
		host = s.byCode[code]
		switch {
		case host == nil:
			// a miss: what someone working through codes gets, so it counts
			if cfg.IPJoinMisses > 0 {
				b := s.ipMisses[c.ip]
				if b == nil {
					b = &bucket{}
					s.ipMisses[c.ip] = b
				}
				b.take(cfg.IPJoinMisses, now)
			}
		case host != c:
			hostName = fmt.Sprintf("#%d (%q)", host.number, host.tag)
			response = protocol.JoinResponse{
				JoinType:  protocol.JoinByCode,
				User:      host.login.Gamertag,
				XUID:      host.login.XUID,
				PublicIP:  host.public,
				PrivateIP: host.login.LocalIP,
			}
		}
	}
	s.mu.Unlock()

	denied := protocol.EncodeJoinDenied(protocol.JoinDenied{Reason: protocol.DeniedCodeNotFound})
	switch {
	case limited != "":
		// turned away without looking, so guessing on learns nothing
		s.logf("deny %s -> %q: too many failed joins from %s", c.name(), code, limited)
		c.sendFrame(denied)
	case host == nil:
		if cfg.ConnJoinMisses > 0 {
			c.misses.take(cfg.ConnJoinMisses, now)
		}
		s.logf("deny %s -> %q: no such code", c.name(), code)
		c.sendFrame(denied)
	case host == c:
		s.logf("deny %s -> %q: their own code", c.name(), code)
		c.sendFrame(denied)
	default:
		// The host first, so its game is punching towards the joiner by the time
		// the joiner's arrives. Both addresses go to the joiner: a game behind
		// the same public address as the host uses the private one.
		host.sendFrame(protocol.EncodeNATPunch(protocol.NATPunch{PublicIP: c.public}))
		c.sendFrame(protocol.EncodeJoinResponse(response))
		s.logf("join %s -> %s %s: public %s local %s", c.name(), code, hostName, response.PublicIP, response.PrivateIP)
	}
}

// echo sends the request's sequence straight back, logged in or not, for band3
// to time the round trip: up to EchoLimit a connection.
func (c *conn) echo(body []byte) {
	request, ok := protocol.DecodeEchoRequest(body)
	if !ok {
		c.close(fmt.Sprintf("short EchoRequest (%d bytes)", len(body)))
		return
	}
	if c.echoes >= EchoLimit {
		if c.echoes == EchoLimit {
			c.s.logf("echo from %s: over %d, ignored from now on", c.name(), EchoLimit)
		}
		c.echoes = EchoLimit + 1
		return
	}
	c.echoes++
	c.sendFrame(protocol.EncodeEchoReply(protocol.EchoReply{Sequence: request.Sequence}))
}

// portCheck sends the token back in one UDP datagram to the port asked for, on
// the address this connection comes from and no other, so a game can tell
// whether players elsewhere reach it there. One per connection per
// PortCheckEvery.
func (c *conn) portCheck(body []byte) {
	request, ok := protocol.DecodePortCheckRequest(body)
	if !ok {
		c.close(fmt.Sprintf("short PortCheckRequest (%d bytes)", len(body)))
		return
	}
	if c.login == nil {
		c.s.logf("port check from %s before logging in, ignored", c.name())
		return
	}
	if request.Port == 0 {
		c.s.logf("port check from %s: port 0, ignored", c.name())
		return
	}
	now := c.s.cfg.Now()
	if !c.portChecked.IsZero() && now.Sub(c.portChecked) < PortCheckEvery {
		c.s.logf("port check from %s: too soon (one per %v), ignored", c.name(), PortCheckEvery)
		return
	}
	c.portChecked = now
	to := netip.AddrPortFrom(c.ip, request.Port)
	if err := c.s.cfg.SendProbe(to, protocol.PortProbe(request.Token)); err != nil {
		c.s.logf("port check %s -> %s failed: %v", c.name(), to, err)
		return
	}
	c.s.logf("port check %s -> %s", c.name(), to)
}
