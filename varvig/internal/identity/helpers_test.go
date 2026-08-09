package identity

import (
	"crypto/ed25519"
	"encoding/base64"
	"encoding/binary"
	"strings"
)

// buildTestOpenSSHKey encodes an unencrypted OpenSSH Ed25519 private key,
// mirroring what `ssh-keygen -t ed25519` writes, so identity resolution can be
// tested against a realistic file without shelling out.
func buildTestOpenSSHKey(priv ed25519.PrivateKey, pub ed25519.PublicKey, comment string) []byte {
	str := func(b, s []byte) []byte {
		var n [4]byte
		binary.BigEndian.PutUint32(n[:], uint32(len(s)))
		return append(append(b, n[:]...), s...)
	}
	u32 := func(b []byte, v uint32) []byte {
		return append(b, byte(v>>24), byte(v>>16), byte(v>>8), byte(v))
	}

	pubBlob := str(str(nil, []byte("ssh-ed25519")), pub)

	var privSec []byte
	privSec = u32(privSec, 0xdeadbeef)
	privSec = u32(privSec, 0xdeadbeef)
	privSec = str(privSec, []byte("ssh-ed25519"))
	privSec = str(privSec, pub)
	privSec = str(privSec, priv)
	privSec = str(privSec, []byte(comment))
	for i := 1; len(privSec)%8 != 0; i++ {
		privSec = append(privSec, byte(i))
	}

	var body []byte
	body = append(body, "openssh-key-v1\x00"...)
	body = str(body, []byte("none"))
	body = str(body, []byte("none"))
	body = str(body, nil)
	body = u32(body, 1)
	body = str(body, pubBlob)
	body = str(body, privSec)

	b64 := base64.StdEncoding.EncodeToString(body)
	var sb strings.Builder
	sb.WriteString("-----BEGIN OPENSSH PRIVATE KEY-----\n")
	for i := 0; i < len(b64); i += 70 {
		end := i + 70
		if end > len(b64) {
			end = len(b64)
		}
		sb.WriteString(b64[i:end] + "\n")
	}
	sb.WriteString("-----END OPENSSH PRIVATE KEY-----\n")
	return []byte(sb.String())
}
