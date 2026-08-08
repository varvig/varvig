// Package wire is Varvig's peer-to-peer sync protocol. It is deliberately split
// into a frozen core and a negotiated layer (design §4.2):
//
//   - The stream magic, the frame format, the Hello exchange, and the core
//     message set are FROZEN. They are the always-available fallback: any two
//     peers, however far apart in version, can complete a sync using only
//     these.
//   - Everything optional is a named capability advertised in Hello. Peers
//     intersect their advertised capability sets; the intersection is what the
//     session uses. There is no protocol version number with implied
//     semantics — only feature tokens — so an old peer and a new peer always
//     converge on a working subset instead of failing a numeric comparison.
//
// The frame format reuses the object layer's varint discipline: minimal
// unsigned varints, length-prefixed payloads.
package wire

import (
	"bufio"
	"fmt"
	"io"
)

// Magic prefixes every stream. A peer that does not send it is not speaking
// this protocol; the four bytes also give future protocol families a branch
// point without disturbing this one.
const Magic = "VWR1"

// Proto is the frozen core protocol token exchanged in Hello.
const Proto = "varvig/1"

// maxFrame bounds a single frame's payload so a malformed or hostile peer
// cannot force an unbounded allocation.
const maxFrame = 1 << 30

// MsgType identifies a frame's message. The values here are frozen.
type MsgType uint64

const (
	MsgHello      MsgType = 1
	MsgListRefs   MsgType = 2
	MsgRefs       MsgType = 3
	MsgGetObjects MsgType = 4 // fetch request: want[] + have[]
	MsgObject     MsgType = 5 // one object, either direction
	MsgDone       MsgType = 6 // end of an object stream
	MsgPush       MsgType = 7 // request a ref CAS after streaming objects
	MsgOK         MsgType = 8 // generic acknowledgement
	MsgError      MsgType = 9
)

// Capability tokens for the negotiated layer. Core behavior needs none of
// these; they only add optional behavior.
const (
	// CapDeflate: object payloads are zlib-compressed on the wire.
	CapDeflate = "deflate"
)

// Conn is a framed message connection over an arbitrary transport.
type Conn struct {
	r *bufio.Reader
	w *bufio.Writer
}

// NewConn wraps a transport (a TCP socket, a pipe, anything).
func NewConn(rw io.ReadWriter) *Conn {
	return &Conn{r: bufio.NewReader(rw), w: bufio.NewWriter(rw)}
}

// Flush pushes buffered writes to the transport.
func (c *Conn) Flush() error { return c.w.Flush() }

func (c *Conn) writeFrame(t MsgType, payload []byte) error {
	var hdr []byte
	hdr = appendUvarint(hdr, uint64(t))
	hdr = appendUvarint(hdr, uint64(len(payload)))
	if _, err := c.w.Write(hdr); err != nil {
		return err
	}
	_, err := c.w.Write(payload)
	return err
}

// ReadFrame reads the next message type and its raw payload.
func (c *Conn) ReadFrame() (MsgType, []byte, error) {
	t, err := readUvarintReader(c.r)
	if err != nil {
		return 0, nil, err
	}
	n, err := readUvarintReader(c.r)
	if err != nil {
		return 0, nil, err
	}
	if n > maxFrame {
		return 0, nil, fmt.Errorf("wire: frame too large (%d bytes)", n)
	}
	payload := make([]byte, n)
	if _, err := io.ReadFull(c.r, payload); err != nil {
		return 0, nil, err
	}
	return MsgType(t), payload, nil
}

// --- handshake ---

// Hello is the first message each peer sends. Proto identifies the frozen core
// family; Caps advertises optional capabilities; Hashes advertises the
// multihash codes the peer can verify.
type Hello struct {
	Proto  string
	Caps   []string
	Hashes []uint64
}

// WriteHandshake sends the stream magic followed by a Hello frame and flushes.
func (c *Conn) WriteHandshake(h Hello) error {
	if _, err := c.w.WriteString(Magic); err != nil {
		return err
	}
	pw := newPayload()
	pw.string(h.Proto)
	pw.stringList(h.Caps)
	pw.uvarintList(h.Hashes)
	if err := c.writeFrame(MsgHello, pw.data()); err != nil {
		return err
	}
	return c.Flush()
}

// ReadHandshake validates the stream magic and parses the peer's Hello.
func (c *Conn) ReadHandshake() (Hello, error) {
	magic := make([]byte, len(Magic))
	if _, err := io.ReadFull(c.r, magic); err != nil {
		return Hello{}, err
	}
	if string(magic) != Magic {
		return Hello{}, fmt.Errorf("wire: bad stream magic %q", magic)
	}
	t, payload, err := c.ReadFrame()
	if err != nil {
		return Hello{}, err
	}
	if t != MsgHello {
		return Hello{}, fmt.Errorf("wire: expected Hello, got message %d", t)
	}
	pr := newReader(payload)
	var h Hello
	if h.Proto, err = pr.string(); err != nil {
		return Hello{}, err
	}
	if h.Caps, err = pr.stringList(); err != nil {
		return Hello{}, err
	}
	if h.Hashes, err = pr.uvarintList(); err != nil {
		return Hello{}, err
	}
	return h, nil
}

// --- typed message writers ---

func (c *Conn) WriteListRefs() error { return c.writeFrame(MsgListRefs, nil) }

// Ref is a name/identity pair advertised by a peer.
type Ref struct {
	Name string
	ID   []byte
}

func (c *Conn) WriteRefs(refs []Ref) error {
	pw := newPayload()
	pw.uvarint(uint64(len(refs)))
	for _, r := range refs {
		pw.string(r.Name)
		pw.bytes(r.ID)
	}
	return c.writeFrame(MsgRefs, pw.data())
}

func ParseRefs(payload []byte) ([]Ref, error) {
	pr := newReader(payload)
	n, err := pr.uvarint()
	if err != nil {
		return nil, err
	}
	refs := make([]Ref, 0, n)
	for i := uint64(0); i < n; i++ {
		name, err := pr.string()
		if err != nil {
			return nil, err
		}
		id, err := pr.bytes()
		if err != nil {
			return nil, err
		}
		refs = append(refs, Ref{Name: name, ID: id})
	}
	return refs, nil
}

func (c *Conn) WriteGetObjects(want, have [][]byte) error {
	pw := newPayload()
	pw.bytesList(want)
	pw.bytesList(have)
	return c.writeFrame(MsgGetObjects, pw.data())
}

func ParseGetObjects(payload []byte) (want, have [][]byte, err error) {
	pr := newReader(payload)
	if want, err = pr.bytesList(); err != nil {
		return nil, nil, err
	}
	if have, err = pr.bytesList(); err != nil {
		return nil, nil, err
	}
	return want, have, nil
}

func (c *Conn) WriteObject(id, payload []byte) error {
	pw := newPayload()
	pw.bytes(id)
	pw.bytes(payload)
	return c.writeFrame(MsgObject, pw.data())
}

func ParseObject(payload []byte) (id, body []byte, err error) {
	pr := newReader(payload)
	if id, err = pr.bytes(); err != nil {
		return nil, nil, err
	}
	if body, err = pr.bytes(); err != nil {
		return nil, nil, err
	}
	return id, body, nil
}

func (c *Conn) WriteDone() error { return c.writeFrame(MsgDone, nil) }
func (c *Conn) WriteOK() error   { return c.writeFrame(MsgOK, nil) }

func (c *Conn) WritePush(name string, old, new []byte) error {
	pw := newPayload()
	pw.string(name)
	pw.bytes(old)
	pw.bytes(new)
	return c.writeFrame(MsgPush, pw.data())
}

func ParsePush(payload []byte) (name string, old, new []byte, err error) {
	pr := newReader(payload)
	if name, err = pr.string(); err != nil {
		return "", nil, nil, err
	}
	if old, err = pr.bytes(); err != nil {
		return "", nil, nil, err
	}
	if new, err = pr.bytes(); err != nil {
		return "", nil, nil, err
	}
	return name, old, new, nil
}

func (c *Conn) WriteError(msg string) error {
	pw := newPayload()
	pw.string(msg)
	return c.writeFrame(MsgError, pw.data())
}

func ParseError(payload []byte) string {
	pr := newReader(payload)
	s, _ := pr.string()
	return s
}

// Negotiate returns the intersection of two capability sets. Both peers,
// running this on the same two Hellos, compute the identical negotiated set.
func Negotiate(local, remote []string) map[string]bool {
	want := make(map[string]bool, len(remote))
	for _, c := range remote {
		want[c] = true
	}
	out := map[string]bool{}
	for _, c := range local {
		if want[c] {
			out[c] = true
		}
	}
	return out
}
