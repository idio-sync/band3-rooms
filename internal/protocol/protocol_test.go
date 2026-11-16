package protocol

import (
	"bufio"
	"bytes"
	"crypto/hmac"
	"crypto/sha1"
	"encoding/binary"
	"encoding/hex"
	"net/netip"
	"os"
	"strings"
	"testing"
)

// testdata/liveless_rooms_packets.txt is band3's tests/golden/liveless_rooms_packets.txt,
// copied as is: the vectors band3's C++ codec and its Python mock server are held to.
func readGolden(t *testing.T) map[string][]byte {
	t.Helper()
	f, err := os.Open("../../testdata/liveless_rooms_packets.txt")
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	vectors := map[string][]byte{}
	scan := bufio.NewScanner(f)
	scan.Buffer(make([]byte, 64*1024), 64*1024)
	for scan.Scan() {
		line := strings.TrimSpace(scan.Text())
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		name, value, found := strings.Cut(line, " ")
		if !found {
			t.Fatalf("bad golden line %q", line)
		}
		b, err := hex.DecodeString(strings.TrimSpace(value))
		if err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		vectors[name] = b
	}
	if err := scan.Err(); err != nil {
		t.Fatal(err)
	}
	return vectors
}

var (
	goldenKey  = [KeySize]byte{0, 1, 2, 3, 4, 5, 6, 7, 8, 9, 10, 11, 12, 13, 14, 15}
	hostXUID   = uint64(0x0009000000000001)
	localIP    = netip.MustParseAddr("192.168.1.2")
	loopbackIP = netip.MustParseAddr("127.0.0.1")
)

// splitFrame checks the header's size against the frame.
func splitFrame(t *testing.T, data []byte) (uint8, []byte) {
	t.Helper()
	typ, size, err := ParseHeader(data)
	if err != nil {
		t.Fatal(err)
	}
	if len(data) != HeaderSize+size {
		t.Fatalf("frame of %d bytes says %d body bytes", len(data), size)
	}
	return typ, data[HeaderSize:]
}

// roundtrip decodes a golden frame, encodes it back, and checks a body one
// byte short isn't the packet.
func roundtrip[T any](t *testing.T, golden []byte, wantType uint8,
	decode func([]byte) (T, bool), encode func(T) []byte) T {
	t.Helper()
	typ, body := splitFrame(t, golden)
	if typ != wantType {
		t.Fatalf("type %d, want %d", typ, wantType)
	}
	msg, ok := decode(body)
	if !ok {
		t.Fatal("didn't decode")
	}
	if got := encode(msg); !bytes.Equal(got, golden) {
		t.Fatalf("encoded back as\n%X\nwant\n%X", got, golden)
	}
	if len(body) > 0 {
		if _, ok := decode(body[:len(body)-1]); ok {
			t.Fatal("a body one byte short decoded")
		}
	}
	return msg
}

func TestGoldenClientHello(t *testing.T) {
	g := readGolden(t)
	h := roundtrip(t, g["client_hello"], ClientHelloType, DecodeClientHello, EncodeClientHello)
	if h != (ClientHello{Protocol: 0, Emulator: true, Language: "eng", Version: "band3-test"}) {
		t.Fatalf("%+v", h)
	}
}

func TestGoldenServerHello(t *testing.T) {
	g := readGolden(t)
	h := roundtrip(t, g["server_hello"], ServerHelloType, DecodeServerHello, EncodeServerHello)
	if h != (ServerHello{Allowed: true, NeedsProof: true, ProofKey: goldenKey}) {
		t.Fatalf("%+v", h)
	}
}

func TestGoldenClientLogin(t *testing.T) {
	g := readGolden(t)
	l := roundtrip(t, g["client_login"], ClientLoginType, DecodeClientLogin, EncodeClientLogin)
	if l.XUID != hostXUID || l.Gamertag != "host" || l.LocalIP != localIP {
		t.Fatalf("%+v", l)
	}
	if !bytes.Equal(l.Proof[:HashSize], g["proof"]) {
		t.Fatalf("proof %X", l.Proof[:HashSize])
	}
	if !ProofOK(goldenKey, "127.0.0.1", l) {
		t.Fatal("the golden proof didn't check out")
	}
	if ProofOK(goldenKey, "127.0.0.2", l) {
		t.Fatal("a proof for another address checked out")
	}
	// the exact bytes RB3E sends: XUID, gamertag, LAN address, then the hash
	f := g["client_login"]
	if len(f) != 6+580 || !bytes.Equal(f[30:34], []byte{192, 168, 1, 2}) || !bytes.Equal(f[34:54], g["proof"]) {
		t.Fatal("login layout")
	}
	// anything after the hash isn't an emulator's proof
	l.Proof[HashSize] = 1
	if ProofOK(goldenKey, "127.0.0.1", l) {
		t.Fatal("a proof with bytes after the hash checked out")
	}
}

func TestGoldenLoggedIn(t *testing.T) {
	g := readGolden(t)
	l := roundtrip(t, g["server_logged_in"], LoggedInType, DecodeLoggedIn, EncodeLoggedIn)
	if l != (LoggedIn{PublicIP: loopbackIP, Code: "ABCD2345"}) {
		t.Fatalf("%+v", l)
	}
}

func TestGoldenJoinRequest(t *testing.T) {
	g := readGolden(t)
	r := roundtrip(t, g["join_request"], JoinRequestType, DecodeJoinRequest, EncodeJoinRequest)
	if r.Code != "ABCD2345" {
		t.Fatalf("%+v", r)
	}
}

func TestGoldenJoinResponse(t *testing.T) {
	g := readGolden(t)
	r := roundtrip(t, g["join_response"], JoinResponseType, DecodeJoinResponse, EncodeJoinResponse)
	want := JoinResponse{JoinType: JoinByCode, User: "host", XUID: hostXUID, PublicIP: loopbackIP, PrivateIP: localIP}
	if r != want {
		t.Fatalf("%+v", r)
	}
}

func TestGoldenJoinDenied(t *testing.T) {
	g := readGolden(t)
	d := roundtrip(t, g["join_denied"], JoinDeniedType, DecodeJoinDenied, EncodeJoinDenied)
	if d.Reason != DeniedCodeNotFound {
		t.Fatalf("%+v", d)
	}
}

func TestGoldenNATPunch(t *testing.T) {
	g := readGolden(t)
	p := roundtrip(t, g["nat_punch"], NATPunchType, DecodeNATPunch, EncodeNATPunch)
	if p.PublicIP != loopbackIP {
		t.Fatalf("%+v", p)
	}
}

func TestGoldenPingPong(t *testing.T) {
	g := readGolden(t)
	if !bytes.Equal(PingFrame, g["ping"]) || !bytes.Equal(PongFrame, g["pong"]) {
		t.Fatal("ping/pong bytes")
	}
	if typ, body := splitFrame(t, PingFrame); typ != PingType || len(body) != 0 {
		t.Fatal("ping frame")
	}
	if PingType != PongType {
		t.Fatal("ping and pong are the same type")
	}
}

func TestGoldenProof(t *testing.T) {
	g := readGolden(t)
	p := LoginProof(goldenKey, "127.0.0.1", hostXUID)
	if !bytes.Equal(p[:], g["proof"]) {
		t.Fatalf("%X", p)
	}
}

func TestGoldenXUIDs(t *testing.T) {
	g := readGolden(t)
	for _, name := range []string{"host", "joiner", "User"} {
		got := binary.BigEndian.AppendUint64(nil, RoomsXUID(name))
		if !bytes.Equal(got, g["xuid_"+name]) {
			t.Errorf("%s: %X, want %X", name, got, g["xuid_"+name])
		}
	}
}

// Go's crypto is used as is; these pin the vectors band3's own SHA-1 is held to.
func TestGoldenSHA1Vectors(t *testing.T) {
	g := readGolden(t)
	sum := sha1.Sum([]byte("abc"))
	if !bytes.Equal(sum[:], g["sha1_abc"]) {
		t.Error("sha1_abc")
	}
	mac := hmac.New(sha1.New, bytes.Repeat([]byte{0x0b}, 20))
	mac.Write([]byte("Hi There"))
	if !bytes.Equal(mac.Sum(nil), g["hmac_rfc2202_1"]) {
		t.Error("hmac_rfc2202_1")
	}
	mac = hmac.New(sha1.New, []byte("Jefe"))
	mac.Write([]byte("what do ya want for nothing?"))
	if !bytes.Equal(mac.Sum(nil), g["hmac_rfc2202_2"]) {
		t.Error("hmac_rfc2202_2")
	}
}

// Every vector in the file is one a test above checks, so a new one isn't missed.
func TestGoldenAllVectorsKnown(t *testing.T) {
	known := map[string]bool{
		"client_hello": true, "server_hello": true, "client_login": true, "server_logged_in": true,
		"join_request": true, "join_response": true, "join_denied": true, "nat_punch": true,
		"ping": true, "pong": true, "proof": true, "xuid_host": true, "xuid_joiner": true,
		"xuid_User": true, "sha1_abc": true, "hmac_rfc2202_1": true, "hmac_rfc2202_2": true,
	}
	for name := range readGolden(t) {
		if !known[name] {
			t.Errorf("golden vector %q has no test", name)
		}
	}
}

func TestShortCodesKeepTheEightByteField(t *testing.T) {
	f := EncodeJoinRequest(JoinRequest{Code: "LCMEE"})
	want, _ := hex.DecodeString("4C4C000300084C434D4545000000")
	if !bytes.Equal(f, want) {
		t.Fatalf("%X", f)
	}
	_, body := splitFrame(t, f)
	if r, _ := DecodeJoinRequest(body); r.Code != "LCMEE" {
		t.Fatal(r.Code)
	}
}

func TestStringsAreCutToTheirFields(t *testing.T) {
	f := EncodeClientLogin(ClientLogin{XUID: 1, Gamertag: strings.Repeat("x", 20), LocalIP: netip.MustParseAddr("10.0.0.1")})
	_, body := splitFrame(t, f)
	if l, _ := DecodeClientLogin(body); l.Gamertag != strings.Repeat("x", 15) {
		t.Fatal(l.Gamertag)
	}
	f = EncodeClientHello(ClientHello{Emulator: true, Language: "en", Version: strings.Repeat("v", 60)})
	_, body = splitFrame(t, f)
	if h, _ := DecodeClientHello(body); h.Version != strings.Repeat("v", 47) || h.Language != "en" {
		t.Fatalf("%+v", h)
	}
	// RB3Enhanced's strncpy can fill the whole version field with no NUL
	full := bytes.Repeat([]byte{'w'}, versionField)
	body = append([]byte{0, 1, 'e', 'n', 'g'}, full...)
	if h, _ := DecodeClientHello(body); h.Version != string(full) {
		t.Fatal(h.Version)
	}
}

func TestALongerBodyIsAccepted(t *testing.T) {
	if d, ok := DecodeJoinDenied([]byte("\x00extra")); !ok || d.Reason != 0 {
		t.Fatal("longer body")
	}
}

func TestFrameReaderSplitsAndJoins(t *testing.T) {
	g := readGolden(t)
	stream := append(append(bytes.Clone(g["client_hello"]), g["ping"]...), g["join_request"]...)
	var r FrameReader
	var types []uint8
	var last []byte
	for i := 0; i < len(stream); i += 7 {
		r.Feed(stream[i:min(i+7, len(stream))])
		for {
			typ, body, ok := r.Next()
			if !ok {
				break
			}
			types = append(types, typ)
			last = body
		}
	}
	if !bytes.Equal(types, []uint8{ClientHelloType, PongType, JoinRequestType}) || string(last) != "ABCD2345" {
		t.Fatalf("%v %q", types, last)
	}
	if r.Broken() {
		t.Fatal("broken")
	}
}

func TestFrameReaderBreaksOnBadMagicOrSize(t *testing.T) {
	var r FrameReader
	r.Feed([]byte("LX\x00\x00\x00\x00"))
	if _, _, ok := r.Next(); ok || !r.Broken() {
		t.Fatal("bad magic")
	}
	r = FrameReader{}
	r.Feed([]byte{0x4C, 0x4C, 0, 1, 0x03, 0x01}) // 0x301 bytes, one over
	if _, _, ok := r.Next(); ok || !r.Broken() {
		t.Fatal("too big")
	}
	r = FrameReader{}
	r.Feed([]byte{0x4C, 0x4C, 0, 1, 0x03, 0x00}) // exactly the limit is fine
	if _, _, ok := r.Next(); ok || r.Broken() {
		t.Fatal("at the limit")
	}
}

func TestNonIPv4AddressesEncodeAsZero(t *testing.T) {
	f := EncodeNATPunch(NATPunch{PublicIP: netip.MustParseAddr("::1")})
	if !bytes.Equal(f[HeaderSize:], []byte{0, 0, 0, 0}) {
		t.Fatalf("%X", f)
	}
	f = EncodeNATPunch(NATPunch{PublicIP: netip.MustParseAddr("::ffff:10.1.2.3")})
	if !bytes.Equal(f[HeaderSize:], []byte{10, 1, 2, 3}) {
		t.Fatalf("%X", f)
	}
}
