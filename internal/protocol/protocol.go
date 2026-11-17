// Package protocol is the Liveless Rooms wire format: RB3Enhanced's
// net_liveless_online.h as a PowerPC packs it, so integers are big-endian and
// IPv4 addresses are their four bytes in order. Encoding and decoding only.
//
// Every frame is a 6-byte header (magic "LL", a reserved 0, the packet type and
// the body's size) and then the body. Each direction numbers its packet types
// from 0, so a type means nothing without knowing which way it went.
package protocol

import (
	"bytes"
	"crypto/hmac"
	"crypto/sha1"
	"encoding/binary"
	"errors"
	"fmt"
	"net/netip"
)

const (
	Port            = 19532
	Magic           = 0x4C4C // "LL"
	ProtocolVersion = 0
	HeaderSize      = 6
	MaxBody         = 0x300
	ProofSize       = 0x228
	HashSize        = 20
	KeySize         = 16
	// Codes travel in an eight-byte field, NUL-padded when shorter.
	MaxCodeLength = 8
)

// Client to server.
const (
	ClientHelloType = 0
	ClientLoginType = 1
	PongType        = 2
	JoinRequestType = 3
)

// Server to client.
const (
	ServerHelloType  = 0
	LoggedInType     = 1
	PingType         = 2
	JoinResponseType = 3
	JoinDeniedType   = 4
	NATPunchType     = 5
)

// band3's own packets, which RB3Enhanced's protocol doesn't have. RB3Enhanced
// numbers each direction's packets up from 0, so these are well clear of both
// directions' numbers. A server sends ServerFeatures only to band3 (a hello
// version starting "idio-sync/band3"), and band3 sends PortCheckRequest only to
// a server that sent it, so neither ever reaches a game or server that doesn't
// know them.
const (
	ServerFeaturesType   = 0xB0 // server to client, after LoggedIn
	PortCheckRequestType = 0xB1 // client to server
)

// ServerFeatures.Features: what the server does beyond RB3Enhanced's protocol.
const FeaturePortCheck = 1 << 0

// A port check's token, and the UDP datagram it comes back in: ProbeMagic, then
// the token.
const (
	TokenSize  = 16
	ProbeMagic = "B3PC"
	ProbeSize  = len(ProbeMagic) + TokenSize
)

// JoinResponse.JoinType: a code the player entered, or a join from outside the game.
const (
	JoinByCode   = 0
	JoinExternal = 1
)

// JoinDenied.Reason: the only one there is.
const DeniedCodeNotFound = 0

// Body sizes, as RB3Enhanced packs the structs.
const (
	clientHelloSize  = 1 + 1 + 3 + 0x30
	serverHelloSize  = 1 + 1 + KeySize
	clientLoginSize  = 8 + 16 + 4 + ProofSize
	loggedInSize     = 4 + MaxCodeLength
	joinRequestSize  = MaxCodeLength
	joinResponseSize = 1 + 16 + 8 + 4 + 4
	joinDeniedSize   = 1
	natPunchSize     = 4
	featuresSize     = 4
	portCheckSize    = 2 + TokenSize

	gamertagField = 16
	versionField  = 0x30
	languageField = 3
)

type ClientHello struct {
	Protocol uint8
	Emulator bool   // the client can't sign its proof with a console key
	Language string // three letters
	Version  string // the client's build tag, up to 47 characters
}

type ServerHello struct {
	Allowed    bool
	NeedsProof bool
	ProofKey   [KeySize]byte
}

type ClientLogin struct {
	XUID     uint64
	Gamertag string     // up to 15 characters
	LocalIP  netip.Addr // the address on the player's own network
	// A console's signature, or on an emulator the 20-byte hash followed by zeros.
	Proof [ProofSize]byte
}

type LoggedIn struct {
	PublicIP netip.Addr
	Code     string
}

type JoinRequest struct {
	Code string
}

type JoinResponse struct {
	JoinType  uint8
	User      string
	XUID      uint64
	PublicIP  netip.Addr
	PrivateIP netip.Addr
}

type JoinDenied struct {
	Reason uint8
}

type NATPunch struct {
	PublicIP netip.Addr
}

// ServerFeatures says what the server does beyond RB3Enhanced's protocol, as
// FeaturePortCheck and any later bits.
type ServerFeatures struct {
	Features uint32
}

// PortCheckRequest asks the server to send ProbeMagic and Token in one UDP
// datagram to Port on the address the request came from, so the game can tell
// whether players elsewhere reach it there.
type PortCheckRequest struct {
	Port  uint16
	Token [TokenSize]byte
}

// Frame builds a whole packet: header then body.
func Frame(typ uint8, body []byte) []byte {
	out := make([]byte, HeaderSize, HeaderSize+len(body))
	binary.BigEndian.PutUint16(out[0:], Magic)
	out[2] = 0
	out[3] = typ
	binary.BigEndian.PutUint16(out[4:], uint16(len(body)))
	return append(out, body...)
}

// Ping and Pong are the same six bytes, one each way.
var (
	PingFrame = Frame(PingType, nil)
	PongFrame = Frame(PongType, nil)
)

var (
	ErrShortHeader = errors.New("too short for a header")
	ErrBadMagic    = errors.New("bad magic")
	ErrTooBig      = errors.New("body over the limit")
)

// ParseHeader reads a frame's type and body size from its first 6 bytes.
func ParseHeader(data []byte) (typ uint8, size int, err error) {
	if len(data) < HeaderSize {
		return 0, 0, ErrShortHeader
	}
	if binary.BigEndian.Uint16(data) != Magic {
		return 0, 0, fmt.Errorf("%w %04X", ErrBadMagic, binary.BigEndian.Uint16(data))
	}
	size = int(binary.BigEndian.Uint16(data[4:]))
	if size > MaxBody {
		return 0, 0, fmt.Errorf("%w (%d bytes)", ErrTooBig, size)
	}
	return data[3], size, nil
}

// FrameReader cuts a TCP stream into frames on the header's size, however the
// bytes arrive. Once a header has the wrong magic or too big a size the stream
// can't be resynchronised, so it stays Broken and the connection should go.
type FrameReader struct {
	buf    []byte
	broken bool
	err    error
}

func (r *FrameReader) Feed(data []byte) {
	if !r.broken {
		r.buf = append(r.buf, data...)
	}
}

// Next returns the next whole frame's type and body, or ok=false until more arrives.
func (r *FrameReader) Next() (typ uint8, body []byte, ok bool) {
	if r.broken || len(r.buf) < HeaderSize {
		return 0, nil, false
	}
	typ, size, err := ParseHeader(r.buf)
	if err != nil {
		r.broken, r.err = true, err
		return 0, nil, false
	}
	if len(r.buf) < HeaderSize+size {
		return 0, nil, false
	}
	body = bytes.Clone(r.buf[HeaderSize : HeaderSize+size])
	r.buf = r.buf[HeaderSize+size:]
	if len(r.buf) == 0 {
		r.buf = nil
	}
	return typ, body, true
}

func (r *FrameReader) Broken() bool { return r.broken }

// Err is why the reader broke.
func (r *FrameReader) Err() error { return r.err }

// Buffered is what's waiting for the rest of its frame (or broke the reader).
func (r *FrameReader) Buffered() []byte { return r.buf }

// cstr is text cut so a NUL still fits in a field of room bytes, then NUL-padded.
func cstr(text string, room int) []byte {
	field := make([]byte, room)
	copy(field, text[:min(len(text), room-1)])
	return field
}

// fixed is text cut to the field and NUL-padded, with no room kept for a NUL.
func fixed(text string, room int) []byte {
	field := make([]byte, room)
	copy(field, text)
	return field
}

// text is a char[] field up to its first NUL, or its end.
func text(field []byte) string {
	if i := bytes.IndexByte(field, 0); i >= 0 {
		field = field[:i]
	}
	return string(bytes.ToValidUTF8(field, []byte("�")))
}

func ip4(addr netip.Addr) []byte {
	if !addr.Is4() && !addr.Is4In6() {
		return make([]byte, 4)
	}
	b := addr.Unmap().As4()
	return b[:]
}

func addr4(b []byte) netip.Addr {
	return netip.AddrFrom4([4]byte(b[:4]))
}

func EncodeClientHello(h ClientHello) []byte {
	body := make([]byte, 0, clientHelloSize)
	body = append(body, h.Protocol, b2u(h.Emulator))
	body = append(body, fixed(h.Language, languageField)...)
	body = append(body, cstr(h.Version, versionField)...)
	return Frame(ClientHelloType, body)
}

// DecodeClientHello and the other decoders take a longer body, as RB3Enhanced
// would, but not a shorter one: ok is false for that.
func DecodeClientHello(body []byte) (h ClientHello, ok bool) {
	if len(body) < clientHelloSize {
		return h, false
	}
	h.Protocol = body[0]
	h.Emulator = body[1] != 0
	h.Language = text(body[2:5])
	h.Version = text(body[5 : 5+versionField])
	return h, true
}

func EncodeServerHello(h ServerHello) []byte {
	body := make([]byte, 0, serverHelloSize)
	body = append(body, b2u(h.Allowed), b2u(h.NeedsProof))
	body = append(body, h.ProofKey[:]...)
	return Frame(ServerHelloType, body)
}

func DecodeServerHello(body []byte) (h ServerHello, ok bool) {
	if len(body) < serverHelloSize {
		return h, false
	}
	h.Allowed = body[0] != 0
	h.NeedsProof = body[1] != 0
	copy(h.ProofKey[:], body[2:2+KeySize])
	return h, true
}

func EncodeClientLogin(l ClientLogin) []byte {
	body := make([]byte, 0, clientLoginSize)
	body = binary.BigEndian.AppendUint64(body, l.XUID)
	body = append(body, cstr(l.Gamertag, gamertagField)...)
	body = append(body, ip4(l.LocalIP)...)
	body = append(body, l.Proof[:]...)
	return Frame(ClientLoginType, body)
}

func DecodeClientLogin(body []byte) (l ClientLogin, ok bool) {
	if len(body) < clientLoginSize {
		return l, false
	}
	l.XUID = binary.BigEndian.Uint64(body)
	l.Gamertag = text(body[8:24])
	l.LocalIP = addr4(body[24:28])
	copy(l.Proof[:], body[28:28+ProofSize])
	return l, true
}

func EncodeLoggedIn(l LoggedIn) []byte {
	body := make([]byte, 0, loggedInSize)
	body = append(body, ip4(l.PublicIP)...)
	body = append(body, fixed(l.Code, MaxCodeLength)...)
	return Frame(LoggedInType, body)
}

func DecodeLoggedIn(body []byte) (l LoggedIn, ok bool) {
	if len(body) < loggedInSize {
		return l, false
	}
	l.PublicIP = addr4(body)
	l.Code = text(body[4:12])
	return l, true
}

func EncodeJoinRequest(r JoinRequest) []byte {
	return Frame(JoinRequestType, fixed(r.Code, MaxCodeLength))
}

func DecodeJoinRequest(body []byte) (r JoinRequest, ok bool) {
	if len(body) < joinRequestSize {
		return r, false
	}
	r.Code = text(body[:MaxCodeLength])
	return r, true
}

func EncodeJoinResponse(r JoinResponse) []byte {
	body := make([]byte, 0, joinResponseSize)
	body = append(body, r.JoinType)
	body = append(body, cstr(r.User, gamertagField)...)
	body = binary.BigEndian.AppendUint64(body, r.XUID)
	body = append(body, ip4(r.PublicIP)...)
	body = append(body, ip4(r.PrivateIP)...)
	return Frame(JoinResponseType, body)
}

func DecodeJoinResponse(body []byte) (r JoinResponse, ok bool) {
	if len(body) < joinResponseSize {
		return r, false
	}
	r.JoinType = body[0]
	r.User = text(body[1:17])
	r.XUID = binary.BigEndian.Uint64(body[17:])
	r.PublicIP = addr4(body[25:29])
	r.PrivateIP = addr4(body[29:33])
	return r, true
}

func EncodeJoinDenied(d JoinDenied) []byte {
	return Frame(JoinDeniedType, []byte{d.Reason})
}

func DecodeJoinDenied(body []byte) (d JoinDenied, ok bool) {
	if len(body) < joinDeniedSize {
		return d, false
	}
	return JoinDenied{Reason: body[0]}, true
}

func EncodeNATPunch(p NATPunch) []byte {
	return Frame(NATPunchType, ip4(p.PublicIP))
}

func DecodeNATPunch(body []byte) (p NATPunch, ok bool) {
	if len(body) < natPunchSize {
		return p, false
	}
	return NATPunch{PublicIP: addr4(body)}, true
}

func EncodeServerFeatures(f ServerFeatures) []byte {
	return Frame(ServerFeaturesType, binary.BigEndian.AppendUint32(nil, f.Features))
}

func DecodeServerFeatures(body []byte) (f ServerFeatures, ok bool) {
	if len(body) < featuresSize {
		return f, false
	}
	return ServerFeatures{Features: binary.BigEndian.Uint32(body)}, true
}

func EncodePortCheckRequest(r PortCheckRequest) []byte {
	body := make([]byte, 0, portCheckSize)
	body = binary.BigEndian.AppendUint16(body, r.Port)
	body = append(body, r.Token[:]...)
	return Frame(PortCheckRequestType, body)
}

func DecodePortCheckRequest(body []byte) (r PortCheckRequest, ok bool) {
	if len(body) < portCheckSize {
		return r, false
	}
	r.Port = binary.BigEndian.Uint16(body)
	copy(r.Token[:], body[2:2+TokenSize])
	return r, true
}

// PortProbe is the datagram a port check sends: not a frame, just the magic
// and the token.
func PortProbe(token [TokenSize]byte) []byte {
	return append([]byte(ProbeMagic), token[:]...)
}

// DecodePortProbe is the token in a port check's datagram; ok is false for
// anything else, a longer datagram included.
func DecodePortProbe(data []byte) (token [TokenSize]byte, ok bool) {
	if len(data) != ProbeSize || string(data[:len(ProbeMagic)]) != ProbeMagic {
		return token, false
	}
	copy(token[:], data[len(ProbeMagic):])
	return token, true
}

// LoginProof is what an emulated game puts at the start of its login's proof:
// HMAC-SHA1 keyed with the ServerHello's key over the server address as the
// player typed it (no NUL, no port) and the XUID's 8 big-endian bytes. A
// console signs this hash with its key instead.
func LoginProof(key [KeySize]byte, address string, xuid uint64) [HashSize]byte {
	mac := hmac.New(sha1.New, key[:])
	mac.Write([]byte(address))
	mac.Write(binary.BigEndian.AppendUint64(nil, xuid))
	var out [HashSize]byte
	copy(out[:], mac.Sum(nil))
	return out
}

// ProofOK is whether an emulator's proof is right for address: the hash, then
// nothing but zeros.
func ProofOK(key [KeySize]byte, address string, l ClientLogin) bool {
	want := LoginProof(key, address, l.XUID)
	if !hmac.Equal(l.Proof[:HashSize], want[:]) {
		return false
	}
	for _, b := range l.Proof[HashSize:] {
		if b != 0 {
			return false
		}
	}
	return true
}

// RoomsXUID is the XUID band3 logs in with: a 64-bit FNV-1a of the name in the
// low 48 bits under the 0x0009 prefix, never 0 there.
func RoomsXUID(username string) uint64 {
	h := uint64(0xCBF29CE484222325)
	for i := 0; i < len(username); i++ {
		h ^= uint64(username[i])
		h *= 0x100000001B3
	}
	low := h & 0x0000FFFFFFFFFFFF
	if low == 0 {
		low = 1
	}
	return 0x0009000000000000 | low
}

func b2u(b bool) byte {
	if b {
		return 1
	}
	return 0
}
