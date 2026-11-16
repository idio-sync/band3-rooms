package server

import (
	"bytes"
	"context"
	"errors"
	"net"
	"net/netip"
	"os"
	"runtime"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/idio-sync/band3-rooms/internal/protocol"
)

// logs collects the server's lines for tests to wait on.
type logs struct {
	mu    sync.Mutex
	lines []string
}

func (l *logs) add(line string) {
	l.mu.Lock()
	l.lines = append(l.lines, line)
	l.mu.Unlock()
}

func (l *logs) find(text string) (string, bool) {
	l.mu.Lock()
	defer l.mu.Unlock()
	for _, line := range l.lines {
		if strings.Contains(line, text) {
			return line, true
		}
	}
	return "", false
}

func (l *logs) count(text string) int {
	l.mu.Lock()
	defer l.mu.Unlock()
	n := 0
	for _, line := range l.lines {
		if strings.Contains(line, text) {
			n++
		}
	}
	return n
}

func (l *logs) all() string {
	l.mu.Lock()
	defer l.mu.Unlock()
	return strings.Join(l.lines, "\n")
}

type testServer struct {
	t    *testing.T
	s    *Server
	addr string
	log  *logs
}

// start runs a server on an ephemeral 127.0.0.1 port, stopped when the test ends.
func start(t *testing.T, change func(*Config)) *testServer {
	t.Helper()
	cfg := Defaults()
	cfg.Addresses = []string{"127.0.0.1"}
	cfg.Ping = 0
	cfg.FrameGap = 5 * time.Millisecond
	if change != nil {
		change(&cfg)
	}
	l := &logs{}
	s, err := New(cfg, l.add)
	if err != nil {
		t.Fatal(err)
	}
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	served := make(chan error, 1)
	go func() { served <- s.Serve(ln) }()
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		if err := s.Shutdown(ctx); err != nil {
			t.Errorf("shutdown: %v", err)
		}
		if err := <-served; !errors.Is(err, ErrServerClosed) {
			t.Errorf("Serve returned %v", err)
		}
		if t.Failed() {
			t.Logf("server log:\n%s", l.all())
		}
	})
	return &testServer{t: t, s: s, addr: ln.Addr().String(), log: l}
}

func (ts *testServer) waitLog(text string) string {
	ts.t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if line, ok := ts.log.find(text); ok {
			return line
		}
		time.Sleep(10 * time.Millisecond)
	}
	ts.t.Fatalf("no log line with %q in:\n%s", text, ts.log.all())
	return ""
}

// codes makes NewCode hand these out in turn, then random ones.
func codes(list ...string) func() string {
	var mu sync.Mutex
	return func() string {
		mu.Lock()
		defer mu.Unlock()
		if len(list) == 0 {
			return RandomCode()
		}
		code := list[0]
		list = list[1:]
		return code
	}
}

// client is a scripted Liveless client.
type client struct {
	t  *testing.T
	nc net.Conn
	r  protocol.FrameReader
}

func (ts *testServer) connect() *client {
	ts.t.Helper()
	nc, err := net.DialTimeout("tcp", ts.addr, 5*time.Second)
	if err != nil {
		ts.t.Fatal(err)
	}
	ts.t.Cleanup(func() { nc.Close() })
	return &client{t: ts.t, nc: nc}
}

func (c *client) send(data []byte) {
	c.t.Helper()
	if _, err := c.nc.Write(data); err != nil {
		c.t.Fatal(err)
	}
}

// recv is the next frame, or closed=true once the server hangs up. It fails
// the test if nothing comes within timeout.
func (c *client) recv(timeout time.Duration) (typ uint8, body []byte, closed bool) {
	c.t.Helper()
	deadline := time.Now().Add(timeout)
	buf := make([]byte, 4096)
	for {
		if typ, body, ok := c.r.Next(); ok {
			return typ, body, false
		}
		if c.r.Broken() {
			c.t.Fatalf("bad frame from the server: %v", c.r.Err())
		}
		_ = c.nc.SetReadDeadline(deadline)
		n, err := c.nc.Read(buf)
		c.r.Feed(buf[:n])
		if err != nil {
			if errors.Is(err, os.ErrDeadlineExceeded) {
				c.t.Fatalf("nothing from the server in %v", timeout)
			}
			if _, _, ok := c.r.Next(); ok {
				c.t.Fatal("a frame came with the close; read it first")
			}
			return 0, nil, true
		}
	}
}

// silent is whether nothing comes for d.
func (c *client) silent(d time.Duration) bool {
	_ = c.nc.SetReadDeadline(time.Now().Add(d))
	buf := make([]byte, 1)
	_, err := c.nc.Read(buf)
	if err == nil {
		c.r.Feed(buf)
	}
	return errors.Is(err, os.ErrDeadlineExceeded)
}

func (c *client) expectClosed() {
	c.t.Helper()
	if typ, _, closed := c.recv(5 * time.Second); !closed {
		c.t.Fatalf("wanted the server to hang up, got type %d", typ)
	}
}

func expect[T any](c *client, typ uint8, decode func([]byte) (T, bool)) T {
	c.t.Helper()
	got, body, closed := c.recv(5 * time.Second)
	// pings come whenever they're due; tests that want them ask for them
	for got == protocol.PingType && typ != protocol.PingType && !closed {
		got, body, closed = c.recv(5 * time.Second)
	}
	if closed {
		c.t.Fatalf("server closed while waiting for type %d", typ)
	}
	if got != typ {
		c.t.Fatalf("wanted type %d, got %d (%X)", typ, got, body)
	}
	msg, ok := decode(body)
	if !ok {
		c.t.Fatalf("type %d too short: %X", typ, body)
	}
	return msg
}

func (c *client) hello(emulator bool, version string) protocol.ServerHello {
	c.t.Helper()
	c.send(protocol.EncodeClientHello(protocol.ClientHello{Emulator: emulator, Language: "eng", Version: version}))
	return expect(c, protocol.ServerHelloType, protocol.DecodeServerHello)
}

func loginFrame(h protocol.ServerHello, address, tag, local string) []byte {
	l := protocol.ClientLogin{XUID: protocol.RoomsXUID(tag), Gamertag: tag, LocalIP: netip.MustParseAddr(local)}
	if h.NeedsProof {
		p := protocol.LoginProof(h.ProofKey, address, l.XUID)
		copy(l.Proof[:], p[:])
	}
	return protocol.EncodeClientLogin(l)
}

// login says hello and logs in as band3 would, signed for 127.0.0.1.
func (c *client) login(tag, local string) protocol.LoggedIn {
	c.t.Helper()
	return c.loginAs("band3-test", tag, local)
}

func (c *client) loginAs(version, tag, local string) protocol.LoggedIn {
	c.t.Helper()
	h := c.hello(true, version)
	if !h.Allowed {
		c.t.Fatal("not allowed")
	}
	c.send(loginFrame(h, "127.0.0.1", tag, local))
	return expect(c, protocol.LoggedInType, protocol.DecodeLoggedIn)
}

func (c *client) join(code string) {
	c.send(protocol.EncodeJoinRequest(protocol.JoinRequest{Code: code}))
}

var loopback = netip.MustParseAddr("127.0.0.1")

func TestLogin(t *testing.T) {
	ts := start(t, nil)
	c := ts.connect()
	h := c.hello(true, "idio-sync/band3 v1.2.3")
	if !h.Allowed || !h.NeedsProof || h.ProofKey == ([16]byte{}) {
		t.Fatalf("%+v", h)
	}
	c.send(loginFrame(h, "127.0.0.1", "host", "192.168.1.2"))
	l := expect(c, protocol.LoggedInType, protocol.DecodeLoggedIn)
	if l.PublicIP != loopback || len(l.Code) != CodeLength || strings.Trim(l.Code, CodeAlphabet) != "" {
		t.Fatalf("%+v", l)
	}
	ts.waitLog(`hello #1: emulator, "eng", "idio-sync/band3 v1.2.3", protocol 0`)
	ts.waitLog(`login #1 ("host"): xuid 0009F8CC7EAF4D6F local 192.168.1.2 public 127.0.0.1 code ` + l.Code)
	if st := ts.s.Stats(); st != (Stats{Connections: 1, LoggedIn: 1}) {
		t.Fatalf("%+v", st)
	}
	c.nc.Close()
	ts.waitLog(`close #1 ("host"): client disconnected`)
	if st := ts.s.Stats(); st != (Stats{}) {
		t.Fatalf("%+v", st)
	}
}

func TestProofKeysDifferPerConnection(t *testing.T) {
	ts := start(t, nil)
	a, b := ts.connect().hello(true, "x"), ts.connect().hello(true, "x")
	if a.ProofKey == b.ProofKey {
		t.Fatal("two connections got the same proof key")
	}
}

func TestBadProofIsClosedWithoutLoggingIn(t *testing.T) {
	ts := start(t, func(c *Config) { c.Addresses = []string{"rooms.example.com"} })
	c := ts.connect()
	h := c.hello(true, "band3-test")
	// signed for the IP, not the name the server knows itself by
	c.send(loginFrame(h, "127.0.0.1", "host", "10.0.0.2"))
	c.expectClosed()
	ts.waitLog(`bad proof #1 ("host"): not signed for "rooms.example.com"`)
	ts.waitLog("close #1: login refused")
}

func TestAnyOfSeveralAddressesWorks(t *testing.T) {
	ts := start(t, func(c *Config) { c.Addresses = []string{"rooms.example.com", "127.0.0.1"} })
	ts.connect().login("host", "10.0.0.2")
}

func TestWithoutProofsAnyLoginIsTaken(t *testing.T) {
	ts := start(t, func(c *Config) { c.RequireProof, c.Addresses = false, nil })
	c := ts.connect()
	h := c.hello(true, "band3-test")
	if !h.Allowed || h.NeedsProof {
		t.Fatalf("%+v", h)
	}
	c.send(loginFrame(h, "", "host", "10.0.0.2"))
	expect(c, protocol.LoggedInType, protocol.DecodeLoggedIn)
}

func TestConsoleLogins(t *testing.T) {
	signed := protocol.ClientLogin{XUID: 5, Gamertag: "box", LocalIP: netip.MustParseAddr("10.0.0.2")}
	for i := range signed.Proof {
		signed.Proof[i] = 1
	}
	ts := start(t, nil)
	c := ts.connect()
	c.hello(false, "3.2.0")
	c.send(protocol.EncodeClientLogin(signed))
	expect(c, protocol.LoggedInType, protocol.DecodeLoggedIn)

	ts = start(t, func(c *Config) { c.AllowConsoles = false })
	c = ts.connect()
	c.hello(false, "3.2.0")
	c.send(protocol.EncodeClientLogin(signed))
	c.expectClosed()
	ts.waitLog(`console login #1 ("box") refused`)
}

func TestJoinHit(t *testing.T) {
	ts := start(t, func(c *Config) { c.NewCode = codes("HOST1", "JOIN1") })
	host, joiner := ts.connect(), ts.connect()
	if code := host.login("host", "192.168.1.2").Code; code != "HOST1" {
		t.Fatal(code)
	}
	if code := joiner.login("joiner", "192.168.1.3").Code; code != "JOIN1" {
		t.Fatal(code)
	}
	joiner.join("host1") // as typed, lower case
	punch := expect(host, protocol.NATPunchType, protocol.DecodeNATPunch)
	if punch.PublicIP != loopback {
		t.Fatalf("%+v", punch)
	}
	r := expect(joiner, protocol.JoinResponseType, protocol.DecodeJoinResponse)
	want := protocol.JoinResponse{JoinType: protocol.JoinByCode, User: "host", XUID: protocol.RoomsXUID("host"),
		PublicIP: loopback, PrivateIP: netip.MustParseAddr("192.168.1.2")}
	if r != want {
		t.Fatalf("%+v", r)
	}
	ts.waitLog(`join #2 ("joiner") -> HOST1 #1 ("host"): public 127.0.0.1 local 192.168.1.2`)
}

// Two games behind one router show the server the same address. The server
// sends the host's public and private addresses; both band3 and RB3Enhanced
// go to the private one when the public one is their own.
func TestSamePublicIPGetsTheLANAddress(t *testing.T) {
	ts := start(t, func(c *Config) { c.NewCode = codes("HOST1", "JOIN1") })
	host, joiner := ts.connect(), ts.connect()
	hostIn := host.login("host", "192.168.1.2")
	joinerIn := joiner.login("joiner", "192.168.1.3")
	joiner.join("HOST1")
	expect(host, protocol.NATPunchType, protocol.DecodeNATPunch)
	r := expect(joiner, protocol.JoinResponseType, protocol.DecodeJoinResponse)
	if r.PublicIP != joinerIn.PublicIP || r.PublicIP != hostIn.PublicIP {
		t.Fatalf("public %s, the joiner's %s", r.PublicIP, joinerIn.PublicIP)
	}
	if r.PrivateIP != netip.MustParseAddr("192.168.1.2") {
		t.Fatalf("private %s", r.PrivateIP)
	}
}

// A server on the same LAN as some players gives them its own public address,
// so players from outside are sent somewhere they can reach.
func TestLANPublicIP(t *testing.T) {
	wan := netip.MustParseAddr("203.0.113.7")
	ts := start(t, func(c *Config) {
		c.NewCode = codes("HOST1", "JOIN1")
		c.LANPublicIP = wan
	})
	host, joiner := ts.connect(), ts.connect()
	if l := host.login("host", "192.168.1.2"); l.PublicIP != wan {
		t.Fatalf("%+v", l)
	}
	joiner.login("joiner", "192.168.1.3")
	joiner.join("HOST1")
	if p := expect(host, protocol.NATPunchType, protocol.DecodeNATPunch); p.PublicIP != wan {
		t.Fatalf("%+v", p)
	}
	if r := expect(joiner, protocol.JoinResponseType, protocol.DecodeJoinResponse); r.PublicIP != wan {
		t.Fatalf("%+v", r)
	}
}

func TestJoinMissAndOwnCode(t *testing.T) {
	ts := start(t, func(c *Config) { c.NewCode = codes("JOIN1") })
	c := ts.connect()
	c.login("joiner", "192.168.1.3")
	c.join("NOPE2")
	if d := expect(c, protocol.JoinDeniedType, protocol.DecodeJoinDenied); d.Reason != protocol.DeniedCodeNotFound {
		t.Fatalf("%+v", d)
	}
	c.join("JOIN1")
	expect(c, protocol.JoinDeniedType, protocol.DecodeJoinDenied)
	ts.waitLog(`deny #1 ("joiner") -> "NOPE2": no such code`)
	ts.waitLog(`deny #1 ("joiner") -> "JOIN1": their own code`)
}

func TestJoinBeforeLoginIsIgnored(t *testing.T) {
	ts := start(t, nil)
	c := ts.connect()
	c.hello(true, "band3-test")
	c.join("ABCDE")
	ts.waitLog("join from #1 before logging in, ignored")
	if !c.silent(100 * time.Millisecond) {
		t.Fatal("the server answered")
	}
}

func TestJoinsPerConnectionAreLimited(t *testing.T) {
	ts := start(t, func(c *Config) {
		c.NewCode = codes("HOST1")
		c.ConnJoinMisses, c.IPJoinMisses = 2, 0
	})
	host, joiner := ts.connect(), ts.connect()
	host.login("host", "192.168.1.2")
	joiner.login("joiner", "192.168.1.3")
	for _, code := range []string{"AAAAA", "BBBBB", "HOST1"} {
		joiner.join(code)
		expect(joiner, protocol.JoinDeniedType, protocol.DecodeJoinDenied)
	}
	ts.waitLog(`deny #2 ("joiner") -> "HOST1": too many failed joins from this connection`)
	if !host.silent(100 * time.Millisecond) {
		t.Fatal("the host was sent something for a limited join")
	}
}

func TestJoinsPerAddressAreLimitedAcrossConnections(t *testing.T) {
	ts := start(t, func(c *Config) {
		c.NewCode = codes("HOST1")
		c.ConnJoinMisses, c.IPJoinMisses = 0, 3
	})
	host := ts.connect()
	host.login("host", "192.168.1.2")
	first := ts.connect()
	first.login("guesser", "192.168.1.3")
	for _, code := range []string{"AAAAA", "BBBBB", "CCCCC"} {
		first.join(code)
		expect(first, protocol.JoinDeniedType, protocol.DecodeJoinDenied)
	}
	first.nc.Close()
	// connecting again doesn't start over
	second := ts.connect()
	second.login("guesser", "192.168.1.3")
	second.join("HOST1")
	expect(second, protocol.JoinDeniedType, protocol.DecodeJoinDenied)
	ts.waitLog(`-> "HOST1": too many failed joins from its address`)
}

func TestHitsDontCountAgainstTheLimit(t *testing.T) {
	ts := start(t, func(c *Config) {
		c.NewCode = codes("HOST1")
		c.ConnJoinMisses, c.IPJoinMisses = 1, 1
	})
	host, joiner := ts.connect(), ts.connect()
	host.login("host", "192.168.1.2")
	joiner.login("joiner", "192.168.1.3")
	for range 3 {
		joiner.join("HOST1")
		expect(host, protocol.NATPunchType, protocol.DecodeNATPunch)
		expect(joiner, protocol.JoinResponseType, protocol.DecodeJoinResponse)
	}
}

func TestBucketRefills(t *testing.T) {
	now := time.Unix(1000, 0)
	var b bucket
	for range 3 {
		if !b.allows(3, now) {
			t.Fatal("within the limit")
		}
		b.take(3, now)
	}
	if b.allows(3, now) {
		t.Fatal("over the limit")
	}
	// one a minute / 3 = 20 s each
	if b.allows(3, now.Add(19*time.Second)) {
		t.Fatal("refilled too soon")
	}
	if !b.allows(3, now.Add(21*time.Second)) {
		t.Fatal("didn't refill")
	}
	if b.full(3, now.Add(30*time.Second)) || !b.full(3, now.Add(61*time.Second)) {
		t.Fatal("full")
	}
}

func TestVersionDenyList(t *testing.T) {
	ts := start(t, func(c *Config) { c.DenyVersions = []string{"idio-sync/band3"} })
	c := ts.connect()
	if h := c.hello(true, "IDIO-SYNC/band3 abc1234"); h.Allowed {
		t.Fatalf("%+v", h)
	}
	// the server half-closes at once, so the game sees the end straight after
	c.expectClosed()
	c.nc.Close()
	ts.waitLog("turn away #1: version is on the deny list (idio-sync/band3)")
	ts.waitLog("close #1: turned away")
	if h := ts.connect().hello(true, "3.2.0-rb3e"); !h.Allowed {
		t.Fatal("RB3Enhanced turned away")
	}
}

func TestVersionAllowList(t *testing.T) {
	ts := start(t, func(c *Config) { c.AllowVersions = []string{"idio-sync/band3"} })
	if h := ts.connect().hello(true, "3.2.0-rb3e"); h.Allowed {
		t.Fatal("RB3Enhanced let in")
	}
	ts.waitLog("version isn't on the allow list")
	ts.connect().loginAs("idio-sync/band3 abc1234", "host", "10.0.0.2")
}

func TestATurnedAwayGameThatStaysIsClosed(t *testing.T) {
	ts := start(t, func(c *Config) { c.DenyVersions = []string{"bad"} })
	c := ts.connect()
	c.hello(true, "bad")
	// it doesn't hang up, and sends more; the server closes after a while anyway
	c.send(protocol.PongFrame)
	ts.waitLog("close #1: turned away")
}

func TestPingTimeout(t *testing.T) {
	ts := start(t, func(c *Config) {
		c.Ping = 100 * time.Millisecond
		c.Timeout = 400 * time.Millisecond
	})
	answering, silent := ts.connect(), ts.connect()
	answering.login("host", "10.0.0.2")
	silent.login("joiner", "10.0.0.3")

	done := make(chan struct{})
	pongs := 0
	go func() {
		defer close(done)
		buf := make([]byte, 64)
		for {
			_ = answering.nc.SetReadDeadline(time.Now().Add(time.Second))
			n, err := answering.nc.Read(buf)
			if err != nil {
				return
			}
			for i := 0; i+protocol.HeaderSize <= n; i += protocol.HeaderSize {
				if bytes.Equal(buf[i:i+protocol.HeaderSize], protocol.PingFrame) {
					pongs++
					if _, err := answering.nc.Write(protocol.PongFrame); err != nil {
						return
					}
				}
			}
		}
	}()
	started := time.Now()
	for {
		typ, _, closed := silent.recv(2 * time.Second)
		if closed {
			break
		}
		if typ != protocol.PingType {
			t.Fatalf("type %d", typ)
		}
	}
	if took := time.Since(started); took > 1500*time.Millisecond {
		t.Fatalf("the silent game went after %v", took)
	}
	ts.waitLog(`ping timeout #2 ("joiner")`)
	ts.waitLog(`close #2 ("joiner"): ping timeout`)
	time.Sleep(500 * time.Millisecond)
	if _, ok := ts.log.find("close #1"); ok {
		t.Fatalf("the game that answers was closed:\n%s", ts.log.all())
	}
	answering.nc.Close()
	<-done
	if pongs < 3 {
		t.Fatalf("%d pongs", pongs)
	}
}

func TestLoginTimeout(t *testing.T) {
	ts := start(t, func(c *Config) { c.LoginTimeout = 200 * time.Millisecond })
	c := ts.connect()
	c.hello(true, "band3-test")
	c.expectClosed()
	ts.waitLog("close #1: login timeout")
	// a game that logs in in time stays
	ok := ts.connect()
	ok.login("host", "10.0.0.2")
	if !ok.silent(400 * time.Millisecond) {
		t.Fatal("closed after logging in")
	}
}

func TestCodesAreUniqueAmongConnectedGames(t *testing.T) {
	ts := start(t, func(c *Config) { c.NewCode = codes("AAAAA", "AAAAA", "AAAAA", "BBBBB", "AAAAA") })
	first, second := ts.connect(), ts.connect()
	if code := first.login("one", "10.0.0.2").Code; code != "AAAAA" {
		t.Fatal(code)
	}
	if code := second.login("two", "10.0.0.3").Code; code != "BBBBB" {
		t.Fatal(code)
	}
	// once the first is gone its code is free again
	first.nc.Close()
	ts.waitLog(`close #1 ("one")`)
	if code := ts.connect().login("three", "10.0.0.4").Code; code != "AAAAA" {
		t.Fatal(code)
	}
}

func TestRandomCodes(t *testing.T) {
	seen := map[string]bool{}
	for range 1000 {
		code := RandomCode()
		if len(code) != CodeLength || strings.Trim(code, CodeAlphabet) != "" {
			t.Fatal(code)
		}
		seen[code] = true
	}
	if len(seen) < 990 {
		t.Fatalf("only %d different codes in 1000", len(seen))
	}
}

func TestConnectionLimits(t *testing.T) {
	ts := start(t, func(c *Config) { c.MaxConnsPerIP = 2 })
	a, b := ts.connect(), ts.connect()
	ts.waitLog("connect #2")
	ts.connect().expectClosed()
	ts.waitLog("refuse #3 from 127.0.0.1:")
	a.login("a", "10.0.0.2")
	b.login("b", "10.0.0.3")

	ts = start(t, func(c *Config) { c.MaxConns = 1 })
	ts.connect()
	ts.waitLog("connect #1")
	ts.connect().expectClosed()
	ts.waitLog("refuse #2 from 127.0.0.1:")
	ts.waitLog("server full")
}

func TestAnythingButAHelloFirstIsClosed(t *testing.T) {
	ts := start(t, nil)
	c := ts.connect()
	c.join("ABCDE")
	c.expectClosed()
	ts.waitLog("close #1: wanted a ClientHello, got type 3 of 8 bytes")
	c = ts.connect()
	c.send([]byte("XX\x00\x00\x00\x00"))
	c.expectClosed()
	ts.waitLog("close #2: bad magic or size: 585800000000")
	c = ts.connect()
	c.send([]byte{0x4C, 0x4C, 0, 0, 0x03, 0x01})
	c.expectClosed()
	ts.waitLog("close #3: bad magic or size")
}

func TestShortPacketsAreClosed(t *testing.T) {
	ts := start(t, nil)
	c := ts.connect()
	c.hello(true, "band3-test")
	c.send(protocol.Frame(protocol.ClientLoginType, make([]byte, 100)))
	c.expectClosed()
	ts.waitLog("close #1: short ClientLogin (100 bytes)")
}

func TestALoginInPiecesIsPutBackTogether(t *testing.T) {
	ts := start(t, func(c *Config) { c.NewCode = codes("HOST1") })
	c := ts.connect()
	h := c.hello(true, "band3-test")
	f := loginFrame(h, "127.0.0.1", "host", "192.168.1.2")
	// the first piece ends inside the header's size field
	for _, piece := range [][]byte{f[:5], f[5:300], f[300:]} {
		c.send(piece)
		time.Sleep(50 * time.Millisecond)
	}
	if l := expect(c, protocol.LoggedInType, protocol.DecodeLoggedIn); l.Code != "HOST1" {
		t.Fatal(l.Code)
	}
}

// RB3Enhanced takes one packet per read, so frames going to one game have to
// arrive apart, not together in one segment.
func TestFramesQueuedTogetherArriveApart(t *testing.T) {
	ts := start(t, func(c *Config) {
		c.NewCode = codes("HOST1", "JOIN1")
		c.FrameGap = DefaultFrameGap
	})
	host, joiner := ts.connect(), ts.connect()
	host.login("host", "192.168.1.2")
	joiner.login("joiner", "192.168.1.3")
	request := protocol.EncodeJoinRequest(protocol.JoinRequest{Code: "HOST1"})
	joiner.send(append(append(bytes.Clone(request), request...), protocol.EncodeJoinRequest(protocol.JoinRequest{Code: "NOPE2"})...))

	// one read, one whole frame: as RB3Enhanced reads
	readOne := func(c *client) uint8 {
		t.Helper()
		buf := make([]byte, protocol.HeaderSize+protocol.MaxBody)
		_ = c.nc.SetReadDeadline(time.Now().Add(5 * time.Second))
		n, err := c.nc.Read(buf)
		if err != nil {
			t.Fatal(err)
		}
		typ, size, err := protocol.ParseHeader(buf[:n])
		if err != nil || n != protocol.HeaderSize+size {
			t.Fatalf("a read of %d bytes isn't one frame: %X", n, buf[:n])
		}
		return typ
	}
	var hostTypes, joinerTypes []uint8
	var wg sync.WaitGroup
	wg.Add(2)
	go func() {
		defer wg.Done()
		for range 2 {
			hostTypes = append(hostTypes, readOne(host))
		}
	}()
	go func() {
		defer wg.Done()
		for range 3 {
			joinerTypes = append(joinerTypes, readOne(joiner))
		}
	}()
	wg.Wait()
	if !bytes.Equal(hostTypes, []uint8{protocol.NATPunchType, protocol.NATPunchType}) {
		t.Fatalf("host %v", hostTypes)
	}
	if !bytes.Equal(joinerTypes, []uint8{protocol.JoinResponseType, protocol.JoinResponseType, protocol.JoinDeniedType}) {
		t.Fatalf("joiner %v", joinerTypes)
	}
}

func TestHexLogsEveryFrame(t *testing.T) {
	ts := start(t, func(c *Config) { c.Hex = true })
	ts.connect().hello(true, "band3-test")
	ts.waitLog("#1 <- 4C4C00000035")
	ts.waitLog("#1 -> 4C4C000000120101")
}

func TestGamertagsAreQuotedInLogs(t *testing.T) {
	ts := start(t, nil)
	ts.connect().login("a\nb", "10.0.0.2")
	ts.waitLog(`login #1 ("a\nb")`)
}

func TestShutdownClosesEveryConnectionAndLeavesNoGoroutines(t *testing.T) {
	before := runtime.NumGoroutine()
	cfg := Defaults()
	cfg.Addresses = []string{"127.0.0.1"}
	cfg.Ping = 50 * time.Millisecond
	cfg.Timeout = 10 * time.Second
	cfg.FrameGap = 5 * time.Millisecond
	l := &logs{}
	s, err := New(cfg, l.add)
	if err != nil {
		t.Fatal(err)
	}
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	served := make(chan error, 1)
	go func() { served <- s.Serve(ln) }()
	ts := &testServer{t: t, s: s, addr: ln.Addr().String(), log: l}
	a, b, idle := ts.connect(), ts.connect(), ts.connect()
	a.login("a", "10.0.0.2")
	b.login("b", "10.0.0.3")
	ts.waitLog("connect #3")

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := s.Shutdown(ctx); err != nil {
		t.Fatal(err)
	}
	if err := <-served; !errors.Is(err, ErrServerClosed) {
		t.Fatal(err)
	}
	for _, c := range []*client{a, b, idle} {
		for {
			if _, _, closed := c.recv(2 * time.Second); closed {
				break
			}
		}
		c.nc.Close()
	}
	if n := l.count("server stopping"); n != 3 {
		t.Fatalf("%d closes:\n%s", n, l.all())
	}
	if _, err := net.DialTimeout("tcp", ts.addr, time.Second); err == nil {
		t.Fatal("still accepting")
	}
	deadline := time.Now().Add(3 * time.Second)
	for runtime.NumGoroutine() > before && time.Now().Before(deadline) {
		time.Sleep(20 * time.Millisecond)
	}
	if n := runtime.NumGoroutine(); n > before {
		buf := make([]byte, 1<<16)
		t.Fatalf("%d goroutines, %d before:\n%s", n, before, buf[:runtime.Stack(buf, true)])
	}
}

func TestDisconnectsLeaveNoGoroutines(t *testing.T) {
	ts := start(t, func(c *Config) { c.Ping = 20 * time.Millisecond; c.Timeout = time.Second })
	before := runtime.NumGoroutine()
	for i := range 20 {
		c := ts.connect()
		if i%2 == 0 {
			c.login("x", "10.0.0.2")
		}
		c.nc.Close()
	}
	deadline := time.Now().Add(3 * time.Second)
	for (ts.s.Stats() != Stats{} || runtime.NumGoroutine() > before) && time.Now().Before(deadline) {
		time.Sleep(20 * time.Millisecond)
	}
	if st := ts.s.Stats(); st != (Stats{}) {
		t.Fatalf("%+v", st)
	}
	if n := runtime.NumGoroutine(); n > before {
		t.Fatalf("%d goroutines, %d before", n, before)
	}
}

func TestValidate(t *testing.T) {
	cfg := Defaults()
	if err := cfg.Validate(); err == nil {
		t.Fatal("proofs without an address")
	}
	cfg.Addresses = []string{"rooms.example.com"}
	if err := cfg.Validate(); err != nil {
		t.Fatal(err)
	}
	cfg.Timeout = cfg.Ping
	if err := cfg.Validate(); err == nil {
		t.Fatal("timeout no longer than ping")
	}
	cfg = Defaults()
	cfg.RequireProof = false
	cfg.LANPublicIP = netip.MustParseAddr("::1")
	if err := cfg.Validate(); err == nil {
		t.Fatal("IPv6 LAN public IP")
	}
}

func TestVersionMatching(t *testing.T) {
	cfg := Config{DenyVersions: []string{"idio-sync/band3 dev"}, AllowVersions: []string{"idio-sync/band3", "3."}}
	for version, want := range map[string]bool{
		"idio-sync/band3 abc":     true,
		"Idio-Sync/Band3 abc":     true,
		"idio-sync/band3 dev-123": false,
		"3.2.0":                   true,
		"4.0":                     false,
		"":                        false,
	} {
		if got, _ := cfg.versionAllowed(version); got != want {
			t.Errorf("%q: %v", version, got)
		}
	}
	if ok, _ := (&Config{}).versionAllowed(""); !ok {
		t.Error("no lists lets everyone in")
	}
}
