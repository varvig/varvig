package wire

import (
	"fmt"
	"io"
)

// payload is a small builder for message payloads using the same minimal
// varint / length-prefix discipline as the object layer.
type payload struct{ b []byte }

func newPayload() *payload { return &payload{} }

// data returns the accumulated payload bytes.
func (p *payload) data() []byte { return p.b }

func (p *payload) uvarint(v uint64) { p.b = appendUvarint(p.b, v) }

// bytes writes a length-prefixed byte string.
func (p *payload) bytes(v []byte) {
	p.b = appendUvarint(p.b, uint64(len(v)))
	p.b = append(p.b, v...)
}

func (p *payload) string(s string) { p.bytes([]byte(s)) }

func (p *payload) uvarintList(vs []uint64) {
	p.uvarint(uint64(len(vs)))
	for _, v := range vs {
		p.uvarint(v)
	}
}

func (p *payload) stringList(ss []string) {
	p.uvarint(uint64(len(ss)))
	for _, s := range ss {
		p.string(s)
	}
}

func (p *payload) bytesList(vs [][]byte) {
	p.uvarint(uint64(len(vs)))
	for _, v := range vs {
		p.bytes(v)
	}
}

// reader consumes a message payload.
type reader struct {
	b []byte
	i int
}

func newReader(b []byte) *reader { return &reader{b: b} }

func (r *reader) uvarint() (uint64, error) {
	v, n, err := readUvarint(r.b[r.i:])
	if err != nil {
		return 0, err
	}
	r.i += n
	return v, nil
}

func (r *reader) bytes() ([]byte, error) {
	n, err := r.uvarint()
	if err != nil {
		return nil, err
	}
	if n > uint64(len(r.b)-r.i) {
		return nil, fmt.Errorf("wire: truncated field")
	}
	v := r.b[r.i : r.i+int(n)]
	r.i += int(n)
	return append([]byte(nil), v...), nil
}

func (r *reader) string() (string, error) {
	b, err := r.bytes()
	return string(b), err
}

func (r *reader) uvarintList() ([]uint64, error) {
	n, err := r.uvarint()
	if err != nil {
		return nil, err
	}
	out := make([]uint64, 0, n)
	for i := uint64(0); i < n; i++ {
		v, err := r.uvarint()
		if err != nil {
			return nil, err
		}
		out = append(out, v)
	}
	return out, nil
}

func (r *reader) stringList() ([]string, error) {
	n, err := r.uvarint()
	if err != nil {
		return nil, err
	}
	out := make([]string, 0, n)
	for i := uint64(0); i < n; i++ {
		s, err := r.string()
		if err != nil {
			return nil, err
		}
		out = append(out, s)
	}
	return out, nil
}

func (r *reader) bytesList() ([][]byte, error) {
	n, err := r.uvarint()
	if err != nil {
		return nil, err
	}
	out := make([][]byte, 0, n)
	for i := uint64(0); i < n; i++ {
		b, err := r.bytes()
		if err != nil {
			return nil, err
		}
		out = append(out, b)
	}
	return out, nil
}

// --- varint helpers (minimal encoding, shared discipline with the object layer) ---

func appendUvarint(b []byte, v uint64) []byte {
	for v >= 0x80 {
		b = append(b, byte(v)|0x80)
		v >>= 7
	}
	return append(b, byte(v))
}

func readUvarint(b []byte) (val uint64, n int, err error) {
	var x uint64
	var s uint
	for i := 0; i < len(b); i++ {
		ch := b[i]
		if i == 10 {
			return 0, 0, fmt.Errorf("wire: varint overflow")
		}
		if ch < 0x80 {
			if i == 9 && ch > 1 {
				return 0, 0, fmt.Errorf("wire: varint overflow")
			}
			if ch == 0 && i != 0 {
				return 0, 0, fmt.Errorf("wire: non-minimal varint")
			}
			return x | uint64(ch)<<s, i + 1, nil
		}
		x |= uint64(ch&0x7f) << s
		s += 7
	}
	return 0, 0, fmt.Errorf("wire: truncated varint")
}

func readUvarintReader(r io.ByteReader) (uint64, error) {
	var x uint64
	var s uint
	for i := 0; ; i++ {
		ch, err := r.ReadByte()
		if err != nil {
			return 0, err
		}
		if i == 10 {
			return 0, fmt.Errorf("wire: varint overflow")
		}
		if ch < 0x80 {
			if i == 9 && ch > 1 {
				return 0, fmt.Errorf("wire: varint overflow")
			}
			if ch == 0 && i != 0 {
				return 0, fmt.Errorf("wire: non-minimal varint")
			}
			return x | uint64(ch)<<s, nil
		}
		x |= uint64(ch&0x7f) << s
		s += 7
	}
}
