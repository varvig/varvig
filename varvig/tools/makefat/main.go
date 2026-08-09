// Command makefat combines several single-architecture Mach-O executables into
// one universal ("fat") Mach-O binary.
//
// It exists so the release workflow can produce the darwin-universal artifact on
// a Linux runner, where Apple's `lipo` is unavailable (varvig-release-automation
// §4, §6). The fat container is a small, well-documented header followed by the
// unchanged input slices; makefat writes it directly rather than depending on
// the macOS toolchain.
//
//	makefat -o varvig-darwin-universal varvig-darwin-amd64 varvig-darwin-arm64
package main

import (
	"debug/macho"
	"encoding/binary"
	"flag"
	"fmt"
	"os"
	"sort"
)

// fatMagic and the 32-bit fat_arch layout are the classic universal-binary
// container (mach-o/fat.h). 32-bit offsets are sufficient: our slices are a few
// megabytes each, far below the 4 GiB where fat_arch_64 becomes necessary.
const (
	fatMagic  = 0xCAFEBABE
	alignBits = 14 // 2^14 = 16384, the alignment Apple uses for arm64/x86_64 slices
	align     = 1 << alignBits
)

type slice struct {
	cpu    macho.Cpu
	subCpu uint32
	data   []byte
	offset uint32
}

func main() {
	out := flag.String("o", "", "output path for the universal binary")
	flag.Parse()
	if *out == "" || flag.NArg() < 2 {
		fmt.Fprintln(os.Stderr, "usage: makefat -o OUTPUT INPUT1 INPUT2 [INPUT...]")
		os.Exit(2)
	}
	if err := run(*out, flag.Args()); err != nil {
		fmt.Fprintf(os.Stderr, "makefat: %v\n", err)
		os.Exit(1)
	}
}

func run(out string, inputs []string) error {
	var slices []slice
	seen := map[string]bool{}
	for _, in := range inputs {
		data, err := os.ReadFile(in)
		if err != nil {
			return err
		}
		f, err := macho.NewFile(newReaderAt(data))
		if err != nil {
			return fmt.Errorf("%s: not a Mach-O file: %w", in, err)
		}
		key := fmt.Sprintf("%d/%d", f.Cpu, f.SubCpu&^0xff000000)
		if seen[key] {
			return fmt.Errorf("%s: duplicate architecture %s", in, f.Cpu)
		}
		seen[key] = true
		slices = append(slices, slice{cpu: f.Cpu, subCpu: f.SubCpu, data: data})
	}
	// Deterministic order (by cputype) so the same inputs always yield the same
	// container — important for a reproducible, attestable release artifact.
	sort.Slice(slices, func(i, j int) bool { return slices[i].cpu < slices[j].cpu })

	// The header is fatMagic + nfat_arch + one fat_arch per slice; slice data
	// begins after it, each aligned up to `align`.
	headerLen := 8 + 20*len(slices)
	offset := alignUp(uint32(headerLen))
	for i := range slices {
		slices[i].offset = offset
		offset += uint32(len(slices[i].data))
		offset = alignUp(offset)
	}

	f, err := os.Create(out)
	if err != nil {
		return err
	}
	defer f.Close()

	be := binary.BigEndian
	hdr := make([]byte, headerLen)
	be.PutUint32(hdr[0:], fatMagic)
	be.PutUint32(hdr[4:], uint32(len(slices)))
	for i, s := range slices {
		p := hdr[8+20*i:]
		be.PutUint32(p[0:], uint32(s.cpu))
		be.PutUint32(p[4:], s.subCpu)
		be.PutUint32(p[8:], s.offset)
		be.PutUint32(p[12:], uint32(len(s.data)))
		be.PutUint32(p[16:], alignBits)
	}
	if _, err := f.Write(hdr); err != nil {
		return err
	}
	for _, s := range slices {
		if _, err := f.WriteAt(s.data, int64(s.offset)); err != nil {
			return err
		}
	}
	// The output must be executable: it is a program.
	if err := f.Chmod(0o755); err != nil {
		return err
	}
	fmt.Printf("makefat: wrote %s (%d architectures)\n", out, len(slices))
	return nil
}

func alignUp(n uint32) uint32 {
	if r := n % align; r != 0 {
		return n + (align - r)
	}
	return n
}

// readerAt adapts an in-memory byte slice to io.ReaderAt for debug/macho.
type readerAt struct{ b []byte }

func newReaderAt(b []byte) *readerAt { return &readerAt{b: b} }

func (r *readerAt) ReadAt(p []byte, off int64) (int, error) {
	if off < 0 || off >= int64(len(r.b)) {
		return 0, fmt.Errorf("offset %d out of range", off)
	}
	n := copy(p, r.b[off:])
	if n < len(p) {
		return n, fmt.Errorf("short read")
	}
	return n, nil
}
