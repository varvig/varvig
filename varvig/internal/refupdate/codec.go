package refupdate

import "fmt"

// Minimal, strict varint and length-prefix helpers — the same canonical
// discipline the object format uses (minimal encodings only, no trailing
// bytes), kept local so this package shares no mutable state with others.

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
			return 0, 0, fmt.Errorf("%w: varint overflow", ErrMalformed)
		}
		if ch < 0x80 {
			if i == 9 && ch > 1 {
				return 0, 0, fmt.Errorf("%w: varint overflow", ErrMalformed)
			}
			if ch == 0 && i != 0 {
				return 0, 0, fmt.Errorf("%w: non-minimal varint", ErrMalformed)
			}
			return x | uint64(ch)<<s, i + 1, nil
		}
		x |= uint64(ch&0x7f) << s
		s += 7
	}
	return 0, 0, fmt.Errorf("%w: truncated varint", ErrMalformed)
}

func appendBytes(b, v []byte) []byte {
	b = appendUvarint(b, uint64(len(v)))
	return append(b, v...)
}

type cursor struct {
	b []byte
	i int
}

func (c *cursor) uvarint() (uint64, error) {
	v, n, err := readUvarint(c.b[c.i:])
	if err != nil {
		return 0, err
	}
	c.i += n
	return v, nil
}

func (c *cursor) take(n uint64) ([]byte, error) {
	if n > uint64(len(c.b)-c.i) {
		return nil, fmt.Errorf("%w: truncated", ErrMalformed)
	}
	s := c.b[c.i : c.i+int(n)]
	c.i += int(n)
	return s, nil
}

func (c *cursor) takeBytes() ([]byte, error) {
	n, err := c.uvarint()
	if err != nil {
		return nil, err
	}
	return c.take(n)
}

func (c *cursor) empty() bool { return c.i == len(c.b) }
