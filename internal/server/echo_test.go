package server

import (
	"fmt"
	"testing"
	"time"

	"github.com/idio-sync/band3-rooms/internal/protocol"
)

// band3 learns the server's version and features from its hello alone, with
// no login: what its connection test shows.
func TestBand3IsToldWhatTheServerIsAfterHello(t *testing.T) {
	ts := start(t, func(c *Config) { c.Version = "v0.3.0" })
	c := ts.connect()
	h, info := c.helloInfo(true, band3Version)
	if !h.Allowed {
		t.Fatal("not allowed")
	}
	if info != (protocol.ServerInfo{Features: protocol.FeaturePortCheck | protocol.FeatureEcho, Version: "v0.3.0"}) {
		t.Fatalf("%+v", info)
	}
	// and logging in after it goes on as before
	c.send(loginFrame(h, "127.0.0.1", "host", "192.168.1.2"))
	expect(c, protocol.LoggedInType, protocol.DecodeLoggedIn)
	f := expect(c, protocol.ServerFeaturesType, protocol.DecodeServerFeatures)
	if f.Features != protocol.FeaturePortCheck|protocol.FeatureEcho {
		t.Fatalf("%+v", f)
	}
}

// RB3Enhanced would hang up on ServerInfo, so nothing but band3 gets it; nor
// does a band3 that's turned away.
func TestServerInfoIsForBand3Only(t *testing.T) {
	ts := start(t, func(c *Config) { c.DenyVersions = []string{"idio-sync/band3 old"} })
	for _, version := range []string{"3.2.0", "RB3Enhanced 1.1", "band3-test"} {
		c := ts.connect()
		c.hello(true, version)
		if !c.silent(200 * time.Millisecond) {
			t.Fatalf("%q was sent something after its hello", version)
		}
	}
	c := ts.connect()
	if h := c.hello(true, "idio-sync/band3 old"); h.Allowed {
		t.Fatal("allowed")
	}
	// the server hangs up after a turned-away hello, with nothing more
	if typ, body, closed := c.recv(time.Second); !closed {
		t.Fatalf("a band3 turned away was sent type %d (%X) after its hello", typ, body)
	}
}

func (c *client) echo(sequence uint32) {
	c.send(protocol.EncodeEchoRequest(protocol.EchoRequest{Sequence: sequence}))
}

func TestEchoesBeforeAndAfterLogin(t *testing.T) {
	ts := start(t, nil)
	c := ts.connect()
	h := c.hello(true, band3Version)
	for _, sequence := range []uint32{1, 0xFFFFFFFF} {
		c.echo(sequence)
		if r := expect(c, protocol.EchoReplyType, protocol.DecodeEchoReply); r.Sequence != sequence {
			t.Fatalf("%+v", r)
		}
	}
	c.send(loginFrame(h, "127.0.0.1", "host", "192.168.1.2"))
	expect(c, protocol.LoggedInType, protocol.DecodeLoggedIn)
	expect(c, protocol.ServerFeaturesType, protocol.DecodeServerFeatures)
	c.echo(7)
	if r := expect(c, protocol.EchoReplyType, protocol.DecodeEchoReply); r.Sequence != 7 {
		t.Fatalf("%+v", r)
	}
}

// An echo from what isn't band3 is any unknown type: logged, ignored, and the
// connection kept.
func TestOthersEchoIsAnUnknownType(t *testing.T) {
	ts := start(t, nil)
	c := ts.connect()
	c.loginAs("3.2.0", "rb3e", "192.168.1.3")
	c.echo(1)
	ts.waitLog(fmt.Sprintf(`unknown type %d from #1 ("rb3e"), ignored`, protocol.EchoRequestType))
	c.join("NOPE2")
	expect(c, protocol.JoinDeniedType, protocol.DecodeJoinDenied)
}

func TestEchoesPerConnectionAreLimited(t *testing.T) {
	ts := start(t, nil)
	c := ts.connect()
	c.hello(true, band3Version)
	for i := range EchoLimit {
		c.echo(uint32(i))
		expect(c, protocol.EchoReplyType, protocol.DecodeEchoReply)
	}
	for range 3 {
		c.echo(99)
	}
	if !c.silent(200 * time.Millisecond) {
		t.Fatal("an echo over the limit was answered")
	}
	ts.waitLog(fmt.Sprintf("echo from #1: over %d, ignored from now on", EchoLimit))
	if n := ts.log.count("over 32"); n != 1 {
		t.Fatalf("logged %d times", n)
	}
}

func TestShortEchoIsClosed(t *testing.T) {
	ts := start(t, nil)
	c := ts.connect()
	c.hello(true, band3Version)
	c.send(protocol.Frame(protocol.EchoRequestType, []byte{1, 2, 3}))
	c.expectClosed()
	ts.waitLog("close #1: short EchoRequest (3 bytes)")
}
