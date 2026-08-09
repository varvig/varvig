package sshkey

import (
	"crypto/ed25519"
	"errors"
	"fmt"
	"io"
	"net"
	"time"
)

// ssh-agent protocol message numbers (RFC draft-miller-ssh-agent).
const (
	agentcRequestIdentities = 11
	agentIdentitiesAnswer   = 12
	agentcSignRequest       = 13
	agentSignResponse       = 14
	agentFailure            = 5
)

// Agent is a connection to a running ssh-agent, reached through the socket named
// by SSH_AUTH_SOCK. Supporting the agent means hardware keys and agent
// forwarding work for free (auth design §2.1): the private key never leaves the
// agent, and Varvig only ever asks it to sign.
type Agent struct {
	conn net.Conn
}

// DialAgent connects to the ssh-agent at socketPath (typically $SSH_AUTH_SOCK).
func DialAgent(socketPath string) (*Agent, error) {
	if socketPath == "" {
		return nil, errors.New("sshkey: no ssh-agent socket (SSH_AUTH_SOCK unset)")
	}
	conn, err := net.DialTimeout("unix", socketPath, 5*time.Second)
	if err != nil {
		return nil, err
	}
	return &Agent{conn: conn}, nil
}

// Close releases the agent connection.
func (a *Agent) Close() error { return a.conn.Close() }

// AgentIdentity is one key held by the agent: its parsed Ed25519 public key,
// the raw wire blob the agent expects back in a sign request, and the comment.
type AgentIdentity struct {
	PublicKey PublicKey
	Blob      []byte
	Comment   string
}

// Identities lists the agent's keys, skipping any that are not Ed25519 (Varvig
// signs with Ed25519 only). The raw blob is retained so a later Sign call names
// the exact key the agent advertised.
func (a *Agent) Identities() ([]AgentIdentity, error) {
	if err := a.write([]byte{agentcRequestIdentities}); err != nil {
		return nil, err
	}
	resp, err := a.read()
	if err != nil {
		return nil, err
	}
	if len(resp) == 0 || resp[0] != agentIdentitiesAnswer {
		return nil, fmt.Errorf("sshkey: unexpected agent reply %v", firstByte(resp))
	}
	r := reader{b: resp[1:]}
	n, err := r.uint32()
	if err != nil {
		return nil, err
	}
	var out []AgentIdentity
	for i := uint32(0); i < n; i++ {
		blob, err := r.string()
		if err != nil {
			return nil, err
		}
		comment, err := r.string()
		if err != nil {
			return nil, err
		}
		pk, err := ParsePublicBlob(blob)
		if err != nil {
			continue // not an Ed25519 key; ignore
		}
		pk.Comment = string(comment)
		out = append(out, AgentIdentity{
			PublicKey: pk,
			Blob:      append([]byte(nil), blob...),
			Comment:   string(comment),
		})
	}
	return out, nil
}

// Sign asks the agent to sign data with the key named by keyBlob and returns the
// raw 64-byte Ed25519 signature (unwrapped from the SSH signature envelope).
func (a *Agent) Sign(keyBlob, data []byte) ([]byte, error) {
	var req []byte
	req = append(req, agentcSignRequest)
	req = appendString(req, keyBlob)
	req = appendString(req, data)
	req = appendUint32(req, 0) // flags: 0 is correct for Ed25519
	if err := a.write(req); err != nil {
		return nil, err
	}
	resp, err := a.read()
	if err != nil {
		return nil, err
	}
	if len(resp) == 0 || resp[0] == agentFailure {
		return nil, errors.New("sshkey: ssh-agent refused to sign")
	}
	if resp[0] != agentSignResponse {
		return nil, fmt.Errorf("sshkey: unexpected agent sign reply %d", resp[0])
	}
	// The reply is a single string: the SSH signature envelope.
	r := reader{b: resp[1:]}
	env, err := r.string()
	if err != nil {
		return nil, err
	}
	// Envelope = string(format) string(signature). For Ed25519 the inner
	// signature is exactly the 64 raw bytes.
	er := reader{b: env}
	format, err := er.string()
	if err != nil {
		return nil, err
	}
	if string(format) != KeyTypeEd25519 {
		return nil, fmt.Errorf("%w: agent signed with %q, want %s", ErrUnsupportedType, format, KeyTypeEd25519)
	}
	sig, err := er.string()
	if err != nil {
		return nil, err
	}
	if len(sig) != ed25519.SignatureSize {
		return nil, fmt.Errorf("%w: signature is %d bytes, want %d", ErrMalformed, len(sig), ed25519.SignatureSize)
	}
	return append([]byte(nil), sig...), nil
}

// write frames a payload as uint32(len) || payload and sends it.
func (a *Agent) write(payload []byte) error {
	var frame []byte
	frame = appendUint32(frame, uint32(len(payload)))
	frame = append(frame, payload...)
	_ = a.conn.SetWriteDeadline(time.Now().Add(10 * time.Second))
	_, err := a.conn.Write(frame)
	return err
}

// read reads one length-prefixed agent message.
func (a *Agent) read() ([]byte, error) {
	_ = a.conn.SetReadDeadline(time.Now().Add(10 * time.Second))
	var hdr [4]byte
	if _, err := io.ReadFull(a.conn, hdr[:]); err != nil {
		return nil, err
	}
	n := uint32(hdr[0])<<24 | uint32(hdr[1])<<16 | uint32(hdr[2])<<8 | uint32(hdr[3])
	if n == 0 || n > 1<<20 {
		return nil, fmt.Errorf("sshkey: implausible agent message length %d", n)
	}
	buf := make([]byte, n)
	if _, err := io.ReadFull(a.conn, buf); err != nil {
		return nil, err
	}
	return buf, nil
}

func appendUint32(b []byte, v uint32) []byte {
	return append(b, byte(v>>24), byte(v>>16), byte(v>>8), byte(v))
}

func firstByte(b []byte) int {
	if len(b) == 0 {
		return -1
	}
	return int(b[0])
}
