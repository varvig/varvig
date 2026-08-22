// Package p2p implements Varvig's peer-to-peer sync on top of the wire protocol.
// Any peer is a full replica (design §2): the same binary is both the peer
// that serves an open port and the peer that dials one (§3.1, "no separate
// server product, only a peer with an open port").
//
// Sync is a Merkle-DAG reachability transfer. Because objects are immutable and
// content-addressed, a receiver already holding an object holds its whole
// closure, so the sender prunes the walk at anything the receiver advertises as
// "have" and streams only the genuinely missing objects. Ref updates use the
// same atomic compare-and-swap as local writes — a push is force-with-lease
// across the network (§2).
package p2p

import (
	"bytes"
	"compress/zlib"
	"context"
	"fmt"
	"io"
	"strings"
	"time"

	"github.com/dividebyzero/claude-experiments/varvig/internal/hook"
	"github.com/dividebyzero/claude-experiments/varvig/internal/multihash"
	"github.com/dividebyzero/claude-experiments/varvig/internal/object"
	"github.com/dividebyzero/claude-experiments/varvig/internal/pin"
	"github.com/dividebyzero/claude-experiments/varvig/internal/repo"
	"github.com/dividebyzero/claude-experiments/varvig/internal/wire"
)

// ObjectStore is the object-store surface p2p needs.
type ObjectStore interface {
	GetRaw(multihash.Multihash) ([]byte, error)
	PutVerified(multihash.Multihash, []byte) error
	Has(multihash.Multihash) bool
	Get(multihash.Multihash) (*object.Object, error)
}

// localHello advertises this build's protocol, capabilities, and hashes.
func localHello() wire.Hello {
	return wire.Hello{
		Proto:  wire.Proto,
		Caps:   []string{wire.CapDeflate, wire.CapArtifactRef, wire.CapPin, wire.CapNotesSync},
		Hashes: []uint64{uint64(multihash.BLAKE3), uint64(multihash.SHA2_256)},
	}
}

// handshake exchanges Hello concurrently (so it cannot deadlock on a
// synchronous transport) and returns the negotiated capability set.
func handshake(conn *wire.Conn, hello wire.Hello) (map[string]bool, error) {
	errc := make(chan error, 1)
	go func() { errc <- conn.WriteHandshake(hello) }()
	peer, rerr := conn.ReadHandshake()
	werr := <-errc
	if werr != nil {
		return nil, werr
	}
	if rerr != nil {
		return nil, rerr
	}
	if peer.Proto != wire.Proto {
		return nil, fmt.Errorf("p2p: incompatible peer protocol %q", peer.Proto)
	}
	return wire.Negotiate(hello.Caps, peer.Caps), nil
}

// --- server ---

// Serve handles one peer connection until it closes: handshake, then a request
// loop. Concurrent connections are safe; ref updates serialize through CAS.
func Serve(r *repo.Repo, rw io.ReadWriter) error {
	conn := wire.NewConn(rw)
	caps, err := handshake(conn, localHello())
	if err != nil {
		return err
	}
	for {
		t, payload, err := conn.ReadFrame()
		if err == io.EOF {
			return nil
		}
		if err != nil {
			return err
		}
		switch t {
		case wire.MsgListRefs:
			if err := serveListRefs(r, conn); err != nil {
				return err
			}
		case wire.MsgGetObjects:
			if err := serveGetObjects(r.Objects, conn, caps, payload); err != nil {
				return err
			}
		case wire.MsgObject:
			if err := recvObject(r.Objects, caps, payload); err != nil {
				_ = conn.WriteError(err.Error())
				_ = conn.Flush()
				return err
			}
		case wire.MsgPush:
			if err := servePush(r, conn, payload); err != nil {
				return err
			}
		case wire.MsgPin:
			if err := servePin(r, conn, caps, payload); err != nil {
				return err
			}
		case wire.MsgUnpin:
			if err := serveUnpin(r, conn, caps, payload); err != nil {
				return err
			}
		case wire.MsgListPin:
			if err := serveListPin(r, conn, caps, payload); err != nil {
				return err
			}
		default:
			_ = conn.WriteError(fmt.Sprintf("unexpected message %d", t))
			_ = conn.Flush()
			return fmt.Errorf("p2p: unexpected message %d", t)
		}
	}
}

func serveListRefs(r *repo.Repo, conn *wire.Conn) error {
	names, err := r.Refs.List()
	if err != nil {
		return err
	}
	var out []wire.Ref
	for _, n := range names {
		id, err := r.Refs.Resolve(n)
		if err != nil {
			continue
		}
		out = append(out, wire.Ref{Name: n, ID: id})
	}
	if err := conn.WriteRefs(out); err != nil {
		return err
	}
	return conn.Flush()
}

func serveGetObjects(objs ObjectStore, conn *wire.Conn, caps map[string]bool, payload []byte) error {
	wantB, haveB, err := wire.ParseGetObjects(payload)
	if err != nil {
		return err
	}
	// Prune set: everything the receiver already has (and thus its closure).
	prune := map[string]bool{}
	for _, h := range haveB {
		markClosure(objs, multihash.Multihash(h), prune)
	}
	sent := map[string]bool{}
	var queue []multihash.Multihash
	for _, w := range wantB {
		queue = append(queue, multihash.Multihash(w))
	}
	for len(queue) > 0 {
		id := queue[len(queue)-1]
		queue = queue[:len(queue)-1]
		key := id.Hex()
		if prune[key] || sent[key] {
			continue
		}
		raw, err := objs.GetRaw(id)
		if err != nil {
			_ = conn.WriteError(fmt.Sprintf("missing object %s", key))
			_ = conn.Flush()
			return fmt.Errorf("p2p: serve missing object %s", key)
		}
		if err := conn.WriteObject(id, encodeObjPayload(caps, raw)); err != nil {
			return err
		}
		sent[key] = true
		obj, err := object.Decode(raw)
		if err != nil {
			return err
		}
		links, err := obj.Links()
		if err != nil {
			return err
		}
		queue = append(queue, links...)
	}
	if err := conn.WriteDone(); err != nil {
		return err
	}
	return conn.Flush()
}

func recvObject(objs ObjectStore, caps map[string]bool, payload []byte) error {
	id, body, err := wire.ParseObject(payload)
	if err != nil {
		return err
	}
	raw, err := decodeObjPayload(caps, body)
	if err != nil {
		return err
	}
	return objs.PutVerified(multihash.Multihash(id), raw)
}

func servePush(r *repo.Repo, conn *wire.Conn, payload []byte) error {
	name, old, newv, err := wire.ParsePush(payload)
	if err != nil {
		return err
	}
	// The pushed tip's closure must be fully present before advancing the ref.
	if !hasClosure(r.Objects, multihash.Multihash(newv)) {
		_ = conn.WriteError("push rejected: incomplete object closure")
		return conn.Flush()
	}
	var oldMH, newMH multihash.Multihash
	if len(old) > 0 {
		oldMH = multihash.Multihash(old)
	}
	if len(newv) > 0 {
		newMH = multihash.Multihash(newv)
	}

	// A push is a ref update: run server-side ref-update hooks as a policy gate
	// (design §2). A veto refuses the push before the ref moves.
	hookCtx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	if err := hook.EvaluateRefUpdate(hookCtx, r, name, oldMH, newMH); err != nil {
		_ = conn.WriteError(fmt.Sprintf("push rejected: %v", err))
		return conn.Flush()
	}
	if err := r.Refs.CompareAndSwap(name, oldMH, newMH, "p2p-push", "push"); err != nil {
		_ = conn.WriteError(fmt.Sprintf("push rejected: %v", err))
		return conn.Flush()
	}
	_ = hook.NotifyRefUpdate(hookCtx, r, name, oldMH, newMH)
	if err := conn.WriteOK(); err != nil {
		return err
	}
	return conn.Flush()
}

// --- pin protocol (federation §3) ---
//
// A pin only ever writes under refs/pins/<peer>/ and never moves a head, so it
// grants disk, not promotion (§7.6: pin does not permit escalation). Refusal is
// a normal, visible response — quota or unknown_object — so a requester learns
// it must hold the state itself rather than assuming it is safe upstream.

func servePin(r *repo.Repo, conn *wire.Conn, caps map[string]bool, payload []byte) error {
	if !caps[wire.CapPin] {
		_ = conn.WriteError("refused: pin capability not negotiated")
		return conn.Flush()
	}
	peerID, hashB, notAfter, _, err := wire.ParsePin(payload)
	if err != nil {
		return err
	}
	hash := multihash.Multihash(hashB)
	// Cannot pin what the peer does not hold.
	if !r.Objects.Has(hash) {
		_ = conn.WriteError("refused: unknown_object")
		return conn.Flush()
	}
	// Quota: never let one peer exhaust another's disk (§3).
	live, err := livePins(r, peerID)
	if err != nil {
		return err
	}
	already := false
	for _, p := range live {
		if multihash.Multihash(p.Hash).Equal(hash) {
			already = true
			break
		}
	}
	if !already && len(live) >= pin.MaxPerPeer {
		_ = conn.WriteError("refused: quota")
		return conn.Flush()
	}
	// Replace any existing pin for (peer, hash) so re-pinning just extends expiry.
	if err := removePins(r, peerID, hash); err != nil {
		return err
	}
	name := pin.RefName(peerID, int64(notAfter), hash)
	if err := r.Refs.CompareAndSwap(name, nil, hash, "p2p-pin", "pin"); err != nil {
		_ = conn.WriteError(fmt.Sprintf("refused: %v", err))
		return conn.Flush()
	}
	if err := conn.WriteOK(); err != nil {
		return err
	}
	return conn.Flush()
}

func serveUnpin(r *repo.Repo, conn *wire.Conn, caps map[string]bool, payload []byte) error {
	if !caps[wire.CapPin] {
		_ = conn.WriteError("refused: pin capability not negotiated")
		return conn.Flush()
	}
	peerID, hashB, err := wire.ParseUnpin(payload)
	if err != nil {
		return err
	}
	if err := removePins(r, peerID, multihash.Multihash(hashB)); err != nil {
		_ = conn.WriteError(fmt.Sprintf("refused: %v", err))
		return conn.Flush()
	}
	if err := conn.WriteOK(); err != nil {
		return err
	}
	return conn.Flush()
}

func serveListPin(r *repo.Repo, conn *wire.Conn, caps map[string]bool, payload []byte) error {
	if !caps[wire.CapPin] {
		_ = conn.WriteError("refused: pin capability not negotiated")
		return conn.Flush()
	}
	peerID, err := wire.ParseListPin(payload)
	if err != nil {
		return err
	}
	live, err := livePins(r, peerID)
	if err != nil {
		return err
	}
	if err := conn.WritePins(live); err != nil {
		return err
	}
	return conn.Flush()
}

// livePins returns a peer's unexpired pins, reaping any expired pin refs it
// finds along the way (expiry does the revocation work — §3).
func livePins(r *repo.Repo, peerID string) ([]wire.Pin, error) {
	names, err := r.Refs.List()
	if err != nil {
		return nil, err
	}
	prefix := pin.PeerPrefix(peerID)
	now := time.Now().Unix()
	var out []wire.Pin
	for _, n := range names {
		if !strings.HasPrefix(n, prefix) {
			continue
		}
		_, notAfter, hash, ok := pin.Parse(n)
		if !ok {
			continue
		}
		if notAfter <= now {
			// Expired: reap the ref so it stops being a GC root and stops
			// showing up in listings.
			cur, _ := r.Refs.Resolve(n)
			_ = r.Refs.Delete(n, cur, "p2p-pin", "expired")
			continue
		}
		out = append(out, wire.Pin{Hash: hash, NotAfter: uint64(notAfter)})
	}
	return out, nil
}

// removePins deletes every pin ref a peer holds for a given hash.
func removePins(r *repo.Repo, peerID string, hash multihash.Multihash) error {
	names, err := r.Refs.List()
	if err != nil {
		return err
	}
	prefix := pin.PeerPrefix(peerID)
	for _, n := range names {
		if !strings.HasPrefix(n, prefix) {
			continue
		}
		_, _, h, ok := pin.Parse(n)
		if !ok || !h.Equal(hash) {
			continue
		}
		cur, err := r.Refs.Resolve(n)
		if err != nil {
			continue
		}
		if err := r.Refs.Delete(n, cur, "p2p-pin", "unpin"); err != nil {
			return err
		}
	}
	return nil
}

// --- client ---

// Client is a dialed peer session after a successful handshake.
type Client struct {
	conn *wire.Conn
	caps map[string]bool
}

// Dial performs the handshake as the initiating peer.
func Dial(rw io.ReadWriter) (*Client, error) {
	conn := wire.NewConn(rw)
	caps, err := handshake(conn, localHello())
	if err != nil {
		return nil, err
	}
	return &Client{conn: conn, caps: caps}, nil
}

// Caps returns the negotiated capability set (useful for tests and diagnostics).
func (c *Client) Caps() map[string]bool { return c.caps }

// ListRefs asks the peer to advertise its refs.
func (c *Client) ListRefs() ([]wire.Ref, error) {
	if err := c.conn.WriteListRefs(); err != nil {
		return nil, err
	}
	if err := c.conn.Flush(); err != nil {
		return nil, err
	}
	t, payload, err := c.conn.ReadFrame()
	if err != nil {
		return nil, err
	}
	if t == wire.MsgError {
		return nil, fmt.Errorf("p2p: %s", wire.ParseError(payload))
	}
	if t != wire.MsgRefs {
		return nil, fmt.Errorf("p2p: expected Refs, got %d", t)
	}
	return wire.ParseRefs(payload)
}

// Fetch requests the closure of want (pruned by have) and stores every received
// object, verifying it. It then checks that want's closure is fully present.
func (c *Client) Fetch(objs ObjectStore, want, have []multihash.Multihash) error {
	if err := c.conn.WriteGetObjects(toBytes(want), toBytes(have)); err != nil {
		return err
	}
	if err := c.conn.Flush(); err != nil {
		return err
	}
	for {
		t, payload, err := c.conn.ReadFrame()
		if err != nil {
			return err
		}
		switch t {
		case wire.MsgObject:
			if err := recvObject(objs, c.caps, payload); err != nil {
				return err
			}
		case wire.MsgDone:
			for _, w := range want {
				if !hasClosure(objs, w) {
					return fmt.Errorf("p2p: fetch incomplete, missing closure of %s", w)
				}
			}
			return nil
		case wire.MsgError:
			return fmt.Errorf("p2p: %s", wire.ParseError(payload))
		default:
			return fmt.Errorf("p2p: unexpected message %d during fetch", t)
		}
	}
}

// Push sends the objects the peer is missing for newTip's closure, then asks
// the peer to compare-and-swap name from old to newTip. old is what the peer
// currently holds for name (learned via ListRefs); a mismatch is rejected —
// force-with-lease across the network.
func (c *Client) Push(objs ObjectStore, name string, old, newTip multihash.Multihash) error {
	prune := map[string]bool{}
	if old != nil {
		markClosure(objs, old, prune)
	}
	// Walk newTip's closure locally, sending anything not pruned.
	sent := map[string]bool{}
	queue := []multihash.Multihash{newTip}
	for len(queue) > 0 {
		id := queue[len(queue)-1]
		queue = queue[:len(queue)-1]
		key := id.Hex()
		if prune[key] || sent[key] {
			continue
		}
		raw, err := objs.GetRaw(id)
		if err != nil {
			return fmt.Errorf("p2p: push cannot read %s: %w", key, err)
		}
		obj, err := object.Decode(raw)
		if err != nil {
			return err
		}
		// Federation §1.4 write gate: never write an artifact-ref into a repo
		// synced with a peer that does not understand artifact-ref reachability,
		// or that peer will GC away external state this peer considers pinned.
		if obj.Type() == object.TypeArtifactRef && !c.caps[wire.CapArtifactRef] {
			return fmt.Errorf("p2p: refusing to push artifact-ref %s: peer does not advertise the %q capability", key, wire.CapArtifactRef)
		}
		if err := c.conn.WriteObject(id, encodeObjPayload(c.caps, raw)); err != nil {
			return err
		}
		sent[key] = true
		links, err := obj.Links()
		if err != nil {
			return err
		}
		queue = append(queue, links...)
	}
	if err := c.conn.WritePush(name, bytesOrNil(old), bytesOrNil(newTip)); err != nil {
		return err
	}
	if err := c.conn.Flush(); err != nil {
		return err
	}
	t, payload, err := c.conn.ReadFrame()
	if err != nil {
		return err
	}
	switch t {
	case wire.MsgOK:
		return nil
	case wire.MsgError:
		return fmt.Errorf("p2p: %s", wire.ParseError(payload))
	default:
		return fmt.Errorf("p2p: unexpected push reply %d", t)
	}
}

// --- pin protocol client (federation §3) ---

// ErrPinRefused is returned when a peer refuses a pin. Refusal is a normal,
// expected response — quota exhausted or the object is unknown — not a fault;
// callers use it to decide to hold the state themselves (§3).
type ErrPinRefused struct{ Reason string }

func (e ErrPinRefused) Error() string { return "p2p: pin refused: " + e.Reason }

// Pin asks the peer to retain hash until notAfter (unix secs). A refusal comes
// back as ErrPinRefused so the caller can distinguish "held elsewhere" from a
// transport fault.
func (c *Client) Pin(peerID string, hash multihash.Multihash, notAfter int64, reason string) error {
	if !c.caps[wire.CapPin] {
		return fmt.Errorf("p2p: peer does not advertise %q", wire.CapPin)
	}
	if notAfter <= 0 {
		return fmt.Errorf("p2p: pin requires a not_after expiry (§3)")
	}
	if err := c.conn.WritePin(peerID, hash, uint64(notAfter), reason); err != nil {
		return err
	}
	if err := c.conn.Flush(); err != nil {
		return err
	}
	return c.readAck("pin")
}

// Unpin releases a pin the peer holds for hash.
func (c *Client) Unpin(peerID string, hash multihash.Multihash) error {
	if !c.caps[wire.CapPin] {
		return fmt.Errorf("p2p: peer does not advertise %q", wire.CapPin)
	}
	if err := c.conn.WriteUnpin(peerID, hash); err != nil {
		return err
	}
	if err := c.conn.Flush(); err != nil {
		return err
	}
	return c.readAck("unpin")
}

// ListPin returns the peer's live (unexpired) pins for peerID.
func (c *Client) ListPin(peerID string) ([]wire.Pin, error) {
	if !c.caps[wire.CapPin] {
		return nil, fmt.Errorf("p2p: peer does not advertise %q", wire.CapPin)
	}
	if err := c.conn.WriteListPin(peerID); err != nil {
		return nil, err
	}
	if err := c.conn.Flush(); err != nil {
		return nil, err
	}
	t, payload, err := c.conn.ReadFrame()
	if err != nil {
		return nil, err
	}
	switch t {
	case wire.MsgPins:
		return wire.ParsePins(payload)
	case wire.MsgError:
		return nil, fmt.Errorf("p2p: %s", wire.ParseError(payload))
	default:
		return nil, fmt.Errorf("p2p: unexpected listpin reply %d", t)
	}
}

// readAck reads a single OK/Error reply, mapping a "refused:" error to
// ErrPinRefused so refusal stays a first-class, non-fault outcome.
func (c *Client) readAck(op string) error {
	t, payload, err := c.conn.ReadFrame()
	if err != nil {
		return err
	}
	switch t {
	case wire.MsgOK:
		return nil
	case wire.MsgError:
		msg := wire.ParseError(payload)
		if reason, ok := strings.CutPrefix(msg, "refused: "); ok {
			return ErrPinRefused{Reason: reason}
		}
		return fmt.Errorf("p2p: %s", msg)
	default:
		return fmt.Errorf("p2p: unexpected %s reply %d", op, t)
	}
}

// --- shared closure helpers ---

// markClosure marks id and everything reachable from it that is present in objs.
func markClosure(objs ObjectStore, id multihash.Multihash, set map[string]bool) {
	key := id.Hex()
	if set[key] || !objs.Has(id) {
		return
	}
	set[key] = true
	obj, err := objs.Get(id)
	if err != nil {
		return
	}
	links, err := obj.Links()
	if err != nil {
		return
	}
	for _, l := range links {
		markClosure(objs, l, set)
	}
}

// hasClosure reports whether id and its entire reachable closure are present.
func hasClosure(objs ObjectStore, id multihash.Multihash) bool {
	if !objs.Has(id) {
		return false
	}
	obj, err := objs.Get(id)
	if err != nil {
		return false
	}
	links, err := obj.Links()
	if err != nil {
		return false
	}
	for _, l := range links {
		if !hasClosure(objs, l) {
			return false
		}
	}
	return true
}

func toBytes(ids []multihash.Multihash) [][]byte {
	out := make([][]byte, 0, len(ids))
	for _, id := range ids {
		out = append(out, id)
	}
	return out
}

func bytesOrNil(m multihash.Multihash) []byte {
	if m == nil {
		return nil
	}
	return m
}

// --- object payload (de)compression, gated on the negotiated deflate cap ---

func encodeObjPayload(caps map[string]bool, raw []byte) []byte {
	if !caps[wire.CapDeflate] {
		return raw
	}
	var buf bytes.Buffer
	zw := zlib.NewWriter(&buf)
	zw.Write(raw)
	zw.Close()
	return buf.Bytes()
}

func decodeObjPayload(caps map[string]bool, wireBytes []byte) ([]byte, error) {
	if !caps[wire.CapDeflate] {
		return wireBytes, nil
	}
	zr, err := zlib.NewReader(bytes.NewReader(wireBytes))
	if err != nil {
		return nil, err
	}
	defer zr.Close()
	return io.ReadAll(zr)
}
