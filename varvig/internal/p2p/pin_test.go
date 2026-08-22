package p2p

import (
	"errors"
	"testing"

	"github.com/dividebyzero/claude-experiments/varvig/internal/multihash"
	"github.com/dividebyzero/claude-experiments/varvig/internal/pin"
)

const peerID = "peer-A"

// TestPinLifecycle covers §7.5's happy path: pin, list, unpin.
func TestPinLifecycle(t *testing.T) {
	server, c1, _ := seedServer(t)

	client := dialServe(t, server)
	if err := client.Pin(peerID, c1, 1<<40, "under evaluation"); err != nil {
		t.Fatalf("Pin: %v", err)
	}
	pins, err := client.ListPin(peerID)
	if err != nil {
		t.Fatalf("ListPin: %v", err)
	}
	if len(pins) != 1 || !multihash.Multihash(pins[0].Hash).Equal(c1) {
		t.Fatalf("ListPin = %+v, want one pin on c1", pins)
	}
	// The pin is an ordinary ref under refs/pins/.
	name := pin.RefName(peerID, 1<<40, c1)
	if _, err := server.Refs.Resolve(name); err != nil {
		t.Fatalf("pin ref %s not created: %v", name, err)
	}

	if err := client.Unpin(peerID, c1); err != nil {
		t.Fatalf("Unpin: %v", err)
	}
	pins, err = client.ListPin(peerID)
	if err != nil {
		t.Fatalf("ListPin after unpin: %v", err)
	}
	if len(pins) != 0 {
		t.Fatalf("pins remain after unpin: %+v", pins)
	}
}

// TestPinRequiresExpiry enforces §3: not_after is mandatory.
func TestPinRequiresExpiry(t *testing.T) {
	server, c1, _ := seedServer(t)
	client := dialServe(t, server)
	if err := client.Pin(peerID, c1, 0, ""); err == nil {
		t.Fatal("a pin with no expiry must be rejected")
	}
}

// TestPinQuotaRefusalIsVisible covers §7.5: quota refusal is surfaced as a
// first-class ErrPinRefused, not swallowed.
func TestPinQuotaRefusalIsVisible(t *testing.T) {
	old := pin.MaxPerPeer
	pin.MaxPerPeer = 1
	defer func() { pin.MaxPerPeer = old }()

	server, c1, c2 := seedServer(t)
	client := dialServe(t, server)
	if err := client.Pin(peerID, c1, 1<<40, ""); err != nil {
		t.Fatalf("first pin: %v", err)
	}
	err := client.Pin(peerID, c2, 1<<40, "")
	var refused ErrPinRefused
	if !errors.As(err, &refused) || refused.Reason != "quota" {
		t.Fatalf("second pin should be refused for quota, got: %v", err)
	}
}

// TestPinUnknownObjectRefused: a peer cannot pin what it does not hold.
func TestPinUnknownObjectRefused(t *testing.T) {
	server, _, _ := seedServer(t)
	client := dialServe(t, server)
	ghost, _ := multihash.Sum(multihash.Default, []byte("not in the store"))
	err := client.Pin(peerID, ghost, 1<<40, "")
	var refused ErrPinRefused
	if !errors.As(err, &refused) || refused.Reason != "unknown_object" {
		t.Fatalf("pinning an absent object should refuse unknown_object, got: %v", err)
	}
}

// TestPinDoesNotPermitEscalation covers §7.6: a propose-rights peer can pin but
// never promote — a pin only ever writes under refs/pins/ and never moves a head.
func TestPinDoesNotPermitEscalation(t *testing.T) {
	server, c1, c2 := seedServer(t)
	head, _ := server.Refs.Resolve("refs/heads/main")

	client := dialServe(t, server)
	if err := client.Pin(peerID, c1, 1<<40, ""); err != nil {
		t.Fatalf("Pin: %v", err)
	}

	// The head is untouched: pinning moved no branch.
	if got, _ := server.Refs.Resolve("refs/heads/main"); !got.Equal(head) || !got.Equal(c2) {
		t.Fatalf("pin moved refs/heads/main to %s", got.Hex())
	}
	// Every ref the pin created lives under refs/pins/ and nowhere else.
	names, _ := server.Refs.List()
	for _, n := range names {
		if pin.IsPinRef(n) {
			continue
		}
		if n != "refs/heads/main" {
			t.Fatalf("pin created an unexpected ref outside refs/pins/: %s", n)
		}
	}
}
