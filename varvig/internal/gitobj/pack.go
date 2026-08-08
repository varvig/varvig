package gitobj

import (
	"bytes"
	"compress/zlib"
	"encoding/binary"
	"fmt"
	"io"
	"os"
	"path/filepath"
)

// Packfile support. A normally-cloned Git repository stores its objects in
// packfiles (objects/pack/*.pack) rather than loose, so importing one requires
// reading packs and resolving deltas. This reads every pack fully into memory,
// resolving ofs- and ref-deltas against their bases (in-pack or loose). That is
// simple and correct; lazy idx-based access is a later optimization (design
// §4.3, on-disk layout is a cache).

// git pack object type codes.
const (
	pkCommit   = 1
	pkTree     = 2
	pkBlob     = 3
	pkTag      = 4
	pkOfsDelta = 6
	pkRefDelta = 7
)

func pkKind(t byte) (Kind, bool) {
	switch t {
	case pkCommit:
		return KindCommit, true
	case pkTree:
		return KindTree, true
	case pkBlob:
		return KindBlob, true
	case pkTag:
		return "tag", true
	default:
		return "", false
	}
}

// packedObject is a fully resolved object read from a pack.
type packedObject struct {
	kind Kind
	body []byte
}

// rawEntry is one object as it appears in a pack, before delta resolution.
type rawEntry struct {
	offset     int64
	typ        byte
	inflated   []byte // object body (non-delta) or delta stream (delta)
	baseOffset int64  // for ofs-delta: absolute offset of the base
	baseOID    OID    // for ref-delta
}

// loadPacks reads and resolves every pack under gitDir/objects/pack, returning a
// map from object identity to its resolved kind and body. looseLookup resolves
// ref-delta bases that live as loose objects rather than in a pack.
func loadPacks(gitDir string, looseLookup func(OID) (Kind, []byte, bool)) (map[string]packedObject, error) {
	packs, _ := filepath.Glob(filepath.Join(gitDir, "objects", "pack", "*.pack"))
	out := map[string]packedObject{}
	if len(packs) == 0 {
		return out, nil
	}

	// byOffset is keyed per pack file (index) then offset, for ofs-delta bases.
	type key struct {
		pack int
		off  int64
	}
	resolvedAt := map[key]packedObject{}
	var pending []struct {
		pack int
		e    rawEntry
	}

	for pi, pf := range packs {
		data, err := os.ReadFile(pf)
		if err != nil {
			return nil, err
		}
		entries, err := parsePack(data)
		if err != nil {
			return nil, fmt.Errorf("gitobj: %s: %w", filepath.Base(pf), err)
		}
		for _, e := range entries {
			pending = append(pending, struct {
				pack int
				e    rawEntry
			}{pi, e})
		}
	}

	record := func(pack int, off int64, kind Kind, body []byte) {
		obj := packedObject{kind: kind, body: body}
		resolvedAt[key{pack, off}] = obj
		out[HashObject(kind, body).Hex()] = obj
	}

	// Resolve iteratively: non-deltas immediately, deltas once their base is
	// available. Loop until a full pass resolves nothing new.
	for {
		progress := false
		var still []struct {
			pack int
			e    rawEntry
		}
		for _, p := range pending {
			e := p.e
			switch {
			case e.typ != pkOfsDelta && e.typ != pkRefDelta:
				kind, ok := pkKind(e.typ)
				if !ok {
					return nil, fmt.Errorf("gitobj: unknown pack object type %d", e.typ)
				}
				record(p.pack, e.offset, kind, e.inflated)
				progress = true
			case e.typ == pkOfsDelta:
				base, ok := resolvedAt[key{p.pack, e.baseOffset}]
				if !ok {
					still = append(still, p)
					continue
				}
				body, err := applyDelta(base.body, e.inflated)
				if err != nil {
					return nil, err
				}
				record(p.pack, e.offset, base.kind, body)
				progress = true
			case e.typ == pkRefDelta:
				base, ok := out[e.baseOID.Hex()]
				if !ok {
					if k, b, found := looseLookup(e.baseOID); found {
						base = packedObject{kind: k, body: b}
						ok = true
					}
				}
				if !ok {
					still = append(still, p)
					continue
				}
				body, err := applyDelta(base.body, e.inflated)
				if err != nil {
					return nil, err
				}
				record(p.pack, e.offset, base.kind, body)
				progress = true
			}
		}
		pending = still
		if len(pending) == 0 {
			break
		}
		if !progress {
			return nil, fmt.Errorf("gitobj: %d pack delta(s) with unresolvable bases", len(pending))
		}
	}
	return out, nil
}

// parsePack decodes a packfile into its raw entries (deltas unresolved).
func parsePack(data []byte) ([]rawEntry, error) {
	if len(data) < 12 || string(data[:4]) != "PACK" {
		return nil, fmt.Errorf("bad pack magic")
	}
	if v := binary.BigEndian.Uint32(data[4:8]); v != 2 {
		return nil, fmt.Errorf("unsupported pack version %d", v)
	}
	count := binary.BigEndian.Uint32(data[8:12])
	pos := int64(12)
	entries := make([]rawEntry, 0, count)
	for i := uint32(0); i < count; i++ {
		start := pos
		typ, _, n := parseObjHeader(data[pos:])
		if n == 0 {
			return nil, fmt.Errorf("truncated object header")
		}
		pos += int64(n)

		e := rawEntry{offset: start, typ: typ}
		switch typ {
		case pkOfsDelta:
			neg, m := parseOfsOffset(data[pos:])
			if m == 0 {
				return nil, fmt.Errorf("truncated ofs-delta offset")
			}
			pos += int64(m)
			e.baseOffset = start - neg
			body, consumed, err := inflate(data[pos:])
			if err != nil {
				return nil, err
			}
			e.inflated = body
			pos += int64(consumed)
		case pkRefDelta:
			if pos+20 > int64(len(data)) {
				return nil, fmt.Errorf("truncated ref-delta base")
			}
			copy(e.baseOID[:], data[pos:pos+20])
			pos += 20
			body, consumed, err := inflate(data[pos:])
			if err != nil {
				return nil, err
			}
			e.inflated = body
			pos += int64(consumed)
		default:
			body, consumed, err := inflate(data[pos:])
			if err != nil {
				return nil, err
			}
			e.inflated = body
			pos += int64(consumed)
		}
		entries = append(entries, e)
	}
	return entries, nil
}

// parseObjHeader decodes a pack object's (type, size) header, returning the
// number of header bytes consumed.
func parseObjHeader(b []byte) (typ byte, size int64, n int) {
	if len(b) == 0 {
		return 0, 0, 0
	}
	c := b[0]
	typ = (c >> 4) & 7
	size = int64(c & 0x0f)
	shift := uint(4)
	i := 1
	for c&0x80 != 0 {
		if i >= len(b) {
			return 0, 0, 0
		}
		c = b[i]
		size |= int64(c&0x7f) << shift
		shift += 7
		i++
	}
	return typ, size, i
}

// parseOfsOffset decodes the negative base offset of an ofs-delta.
func parseOfsOffset(b []byte) (off int64, n int) {
	if len(b) == 0 {
		return 0, 0
	}
	c := b[0]
	off = int64(c & 0x7f)
	i := 1
	for c&0x80 != 0 {
		if i >= len(b) {
			return 0, 0
		}
		c = b[i]
		off = ((off + 1) << 7) | int64(c&0x7f)
		i++
	}
	return off, i
}

// inflate zlib-decompresses one stream, returning the bytes consumed from src.
// src is wrapped in a bytes.Reader (an io.ByteReader), so flate does not read
// past the stream and the consumed count is exact.
func inflate(src []byte) (out []byte, consumed int, err error) {
	br := bytes.NewReader(src)
	zr, err := zlib.NewReader(br)
	if err != nil {
		return nil, 0, err
	}
	defer zr.Close()
	out, err = io.ReadAll(zr)
	if err != nil {
		return nil, 0, err
	}
	return out, len(src) - br.Len(), nil
}

// applyDelta reconstructs an object from its base and a git delta stream.
func applyDelta(base, delta []byte) ([]byte, error) {
	pos := 0
	readSize := func() (int64, bool) {
		var size int64
		var shift uint
		for {
			if pos >= len(delta) {
				return 0, false
			}
			c := delta[pos]
			pos++
			size |= int64(c&0x7f) << shift
			shift += 7
			if c&0x80 == 0 {
				return size, true
			}
		}
	}
	baseSize, ok := readSize()
	if !ok || baseSize != int64(len(base)) {
		return nil, fmt.Errorf("gitobj: delta base size mismatch")
	}
	resultSize, ok := readSize()
	if !ok {
		return nil, fmt.Errorf("gitobj: delta truncated result size")
	}
	out := make([]byte, 0, resultSize)
	for pos < len(delta) {
		op := delta[pos]
		pos++
		switch {
		case op&0x80 != 0: // copy from base
			var off, size int64
			for b := uint(0); b < 4; b++ {
				if op&(1<<b) != 0 {
					off |= int64(delta[pos]) << (8 * b)
					pos++
				}
			}
			for b := uint(0); b < 3; b++ {
				if op&(1<<(4+b)) != 0 {
					size |= int64(delta[pos]) << (8 * b)
					pos++
				}
			}
			if size == 0 {
				size = 0x10000
			}
			if off+size > int64(len(base)) {
				return nil, fmt.Errorf("gitobj: delta copy out of range")
			}
			out = append(out, base[off:off+size]...)
		case op != 0: // insert literal
			if pos+int(op) > len(delta) {
				return nil, fmt.Errorf("gitobj: delta insert out of range")
			}
			out = append(out, delta[pos:pos+int(op)]...)
			pos += int(op)
		default:
			return nil, fmt.Errorf("gitobj: reserved delta opcode 0")
		}
	}
	if int64(len(out)) != resultSize {
		return nil, fmt.Errorf("gitobj: delta result size mismatch")
	}
	return out, nil
}
