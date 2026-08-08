package refs

import (
	"bufio"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"

	"github.com/dividebyzero/claude-experiments/loom/internal/multihash"
)

// zeroSentinel marks the absence of a value in a reflog line (creation has no
// old value; deletion has no new value).
const zeroSentinel = "0"

// LogEntry is one recorded ref move. The log is append-only and never
// rewritten, so any prior state of a ref can always be recovered.
type LogEntry struct {
	Old     multihash.Multihash // nil for a creation
	New     multihash.Multihash // nil for a deletion
	UnixNS  int64
	Actor   string
	Message string
}

// Each line is: <oldHex|0> SP <newHex|0> SP <unixNanos> SP <actor> TAB <message>
// The actor field never contains a tab; the message never contains a newline.
func (s *Store) appendLog(name string, oldval, newval multihash.Multihash, actor, msg string) error {
	p := s.logPath(name)
	if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		return err
	}
	f, err := os.OpenFile(p, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o644)
	if err != nil {
		return err
	}
	defer f.Close()

	line := fmt.Sprintf("%s %s %d %s\t%s\n",
		hexOrZero(oldval),
		hexOrZero(newval),
		s.now().UnixNano(),
		sanitizeActor(actor),
		sanitizeMessage(msg),
	)
	if _, err := f.WriteString(line); err != nil {
		return err
	}
	return f.Sync()
}

// ReadLog returns the reflog entries for a ref, oldest first. A ref with no
// log yields an empty slice, not an error.
func (s *Store) ReadLog(name string) ([]LogEntry, error) {
	if err := validName(name); err != nil {
		return nil, err
	}
	f, err := os.Open(s.logPath(name))
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, err
	}
	defer f.Close()

	var entries []LogEntry
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 0, 64*1024), 4*1024*1024)
	for sc.Scan() {
		e, err := parseLogLine(sc.Text())
		if err != nil {
			return nil, fmt.Errorf("refs: corrupt reflog %q: %w", name, err)
		}
		entries = append(entries, e)
	}
	if err := sc.Err(); err != nil {
		return nil, err
	}
	return entries, nil
}

func parseLogLine(line string) (LogEntry, error) {
	tab := strings.IndexByte(line, '\t')
	if tab < 0 {
		return LogEntry{}, fmt.Errorf("missing message separator")
	}
	head, msg := line[:tab], line[tab+1:]
	fields := strings.SplitN(head, " ", 4)
	if len(fields) != 4 {
		return LogEntry{}, fmt.Errorf("expected 4 header fields, got %d", len(fields))
	}
	old, err := parseHexOrZero(fields[0])
	if err != nil {
		return LogEntry{}, err
	}
	newv, err := parseHexOrZero(fields[1])
	if err != nil {
		return LogEntry{}, err
	}
	ns, err := strconv.ParseInt(fields[2], 10, 64)
	if err != nil {
		return LogEntry{}, fmt.Errorf("bad timestamp: %w", err)
	}
	return LogEntry{Old: old, New: newv, UnixNS: ns, Actor: fields[3], Message: msg}, nil
}

func (s *Store) logPath(name string) string {
	return filepath.Join(s.logsDir, filepath.FromSlash(name))
}

func hexOrZero(m multihash.Multihash) string {
	if m == nil {
		return zeroSentinel
	}
	return m.Hex()
}

func parseHexOrZero(s string) (multihash.Multihash, error) {
	if s == zeroSentinel {
		return nil, nil
	}
	return multihash.ParseHex(s)
}

func sanitizeActor(a string) string {
	a = strings.ReplaceAll(a, "\t", " ")
	a = strings.ReplaceAll(a, "\n", " ")
	if a == "" {
		return "-"
	}
	return a
}

func sanitizeMessage(m string) string {
	m = strings.ReplaceAll(m, "\n", " ")
	return m
}
