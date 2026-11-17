package server

import (
	"bytes"
	"net"
	"net/netip"
	"strconv"
	"sync"
	"testing"
	"time"

	"github.com/idio-sync/band3-rooms/internal/protocol"
)

const band3Version = "idio-sync/band3 v1.2.3-4-gabcdef0"

var testToken = [protocol.TokenSize]byte{1, 2, 3, 4, 5, 6, 7, 8, 9, 10, 11, 12, 13, 14, 15, 16}

// probes stands in for the UDP socket: where each port check's datagram went.
type probes struct {
	mu   sync.Mutex
	sent []sentProbe
	now  time.Time
}

type sentProbe struct {
	to   netip.AddrPort
	data []byte
}

func (p *probes) send(to netip.AddrPort, data []byte) error {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.sent = append(p.sent, sentProbe{to, bytes.Clone(data)})
	return nil
}

func (p *probes) list() []sentProbe {
	p.mu.Lock()
	defer p.mu.Unlock()
	return append([]sentProbe(nil), p.sent...)
}

func (p *probes) clock() time.Time {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.now
}

func (p *probes) advance(d time.Duration) {
	p.mu.Lock()
	p.now = p.now.Add(d)
	p.mu.Unlock()
}

// startProbes runs a server whose port checks go to a probes, on its clock.
func startProbes(t *testing.T, change func(*Config)) (*testServer, *probes) {
	t.Helper()
	p := &probes{now: time.Unix(1000, 0)}
	ts := start(t, func(c *Config) {
		c.SendProbe = p.send
		c.Now = p.clock
		if change != nil {
			change(c)
		}
	})
	return ts, p
}

// band3Login logs in as band3 and takes the ServerFeatures that follows.
func (c *client) band3Login(tag string) protocol.ServerFeatures {
	c.t.Helper()
	c.loginAs(band3Version, tag, "192.168.1.2")
	return expect(c, protocol.ServerFeaturesType, protocol.DecodeServerFeatures)
}

func (c *client) portCheck(port uint16) {
	c.send(protocol.EncodePortCheckRequest(protocol.PortCheckRequest{Port: port, Token: testToken}))
}

func TestBand3IsToldOfPortChecks(t *testing.T) {
	ts := start(t, nil)
	for _, version := range []string{band3Version, "IDIO-SYNC/BAND3 x", "idio-sync/band3"} {
		c := ts.connect()
		c.loginAs(version, "host", "192.168.1.2")
		f := expect(c, protocol.ServerFeaturesType, protocol.DecodeServerFeatures)
		if f.Features != protocol.FeaturePortCheck|protocol.FeatureEcho {
			t.Fatalf("%q: %+v", version, f)
		}
	}
}

// RB3Enhanced closes its connection on a packet type it doesn't know, so it's
// never sent ServerFeatures; nor is anything else that isn't band3.
func TestOthersAreNotToldOfPortChecks(t *testing.T) {
	ts := start(t, nil)
	for _, version := range []string{"3.2.0", "RB3Enhanced 1.1", "band3-test", "idio-sync/band", "x idio-sync/band3"} {
		c := ts.connect()
		c.loginAs(version, "host", "192.168.1.2")
		if !c.silent(300 * time.Millisecond) {
			t.Fatalf("%q was sent something after logging in", version)
		}
	}
}

// A client that isn't band3 sending band3's request gets what any unknown
// type gets: logged and ignored, no probe, the connection kept.
func TestOthersPortCheckIsAnUnknownType(t *testing.T) {
	ts, p := startProbes(t, func(c *Config) { c.NewCode = codes("JOIN1") })
	c := ts.connect()
	c.loginAs("3.2.0", "rb3e", "192.168.1.3")
	c.portCheck(9103)
	ts.waitLog(`unknown type 177 from #1 ("rb3e"), ignored`)
	c.join("NOPE2")
	expect(c, protocol.JoinDeniedType, protocol.DecodeJoinDenied)
	if got := p.list(); len(got) != 0 {
		t.Fatalf("probes sent: %+v", got)
	}
}

func TestAPortCheckProbesTheConnectionsOwnAddress(t *testing.T) {
	udp, err := net.ListenUDP("udp4", &net.UDPAddr{IP: net.IPv4(127, 0, 0, 1)})
	if err != nil {
		t.Fatal(err)
	}
	defer udp.Close()
	port := uint16(udp.LocalAddr().(*net.UDPAddr).Port)

	ts := start(t, nil)
	c := ts.connect()
	c.band3Login("host")
	c.portCheck(port)
	_ = udp.SetReadDeadline(time.Now().Add(5 * time.Second))
	buf := make([]byte, 64)
	n, from, err := udp.ReadFromUDP(buf)
	if err != nil {
		t.Fatal(err)
	}
	token, ok := protocol.DecodePortProbe(buf[:n])
	if !ok || token != testToken {
		t.Fatalf("%X from %v", buf[:n], from)
	}
	ts.waitLog(`port check #1 ("host") -> 127.0.0.1:` + strconv.Itoa(int(port)))
}

// The request names a port and nothing else: the datagram goes to the address
// the connection came from, not the public address the game was given.
func TestAPortCheckGoesNowhereElse(t *testing.T) {
	wan := netip.MustParseAddr("203.0.113.7")
	ts, p := startProbes(t, func(c *Config) { c.LANPublicIP = wan })
	c := ts.connect()
	c.band3Login("host")
	c.portCheck(9203)
	ts.waitLog(`port check #1 ("host") -> 127.0.0.1:9203`)
	got := p.list()
	if len(got) != 1 || got[0].to != netip.AddrPortFrom(loopback, 9203) ||
		!bytes.Equal(got[0].data, protocol.PortProbe(testToken)) {
		t.Fatalf("%+v", got)
	}
}

func TestPortChecksAreOnePerConnectionPer30s(t *testing.T) {
	ts, p := startProbes(t, nil)
	c := ts.connect()
	c.band3Login("host")
	c.portCheck(9103)
	ts.waitLog(`port check #1 ("host") -> 127.0.0.1:9103`)
	p.advance(PortCheckEvery - time.Second)
	c.portCheck(9103)
	ts.waitLog(`port check from #1 ("host"): too soon (one per 30s), ignored`)
	if n := len(p.list()); n != 1 {
		t.Fatalf("%d probes", n)
	}
	// another connection has a limit of its own
	other := ts.connect()
	other.band3Login("other")
	other.portCheck(9104)
	ts.waitLog(`port check #2 ("other") -> 127.0.0.1:9104`)

	p.advance(time.Second)
	c.portCheck(9103)
	deadline := time.Now().Add(5 * time.Second)
	for len(p.list()) < 3 && time.Now().Before(deadline) {
		time.Sleep(10 * time.Millisecond)
	}
	if n := ts.log.count(`port check #1 ("host") -> 127.0.0.1:9103`); n != 2 || len(p.list()) != 3 {
		t.Fatalf("%d checks logged, %d probes", n, len(p.list()))
	}
}

func TestBadPortChecksAreIgnoredOrClosed(t *testing.T) {
	ts, p := startProbes(t, nil)
	c := ts.connect()
	c.hello(true, band3Version)
	c.portCheck(9103)
	ts.waitLog("port check from #1 before logging in, ignored")

	c = ts.connect()
	c.band3Login("host")
	c.portCheck(0)
	ts.waitLog(`port check from #2 ("host"): port 0, ignored`)
	// a bad port doesn't use up the connection's check
	c.portCheck(1)
	ts.waitLog(`port check #2 ("host") -> 127.0.0.1:1`)

	c = ts.connect()
	c.band3Login("short")
	c.send(protocol.Frame(protocol.PortCheckRequestType, make([]byte, 17)))
	c.expectClosed()
	ts.waitLog(`close #3 ("short"): short PortCheckRequest (17 bytes)`)
	if n := len(p.list()); n != 1 {
		t.Fatalf("%d probes", n)
	}
}
