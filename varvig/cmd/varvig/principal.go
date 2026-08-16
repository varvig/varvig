package main

import (
	"crypto/ed25519"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"time"

	"github.com/dividebyzero/claude-experiments/varvig/internal/identity"
	"github.com/dividebyzero/claude-experiments/varvig/internal/object"
	"github.com/dividebyzero/claude-experiments/varvig/internal/principal"
	"github.com/dividebyzero/claude-experiments/varvig/internal/repo"
)

// cmdPrincipal administers the org chart (tickets §1.4): the versioned set of
// keyholders and their kind, which governs what each one's signatures are worth
// (§2.4). It is a director/admin surface, like `trust`.
//
//	varvig principal add --name <n> --kind human|agent|bridge [--key <hex>]
//	varvig principal list
//	varvig principal remove <fingerprint>
func cmdPrincipal(args []string) error {
	if len(args) < 1 {
		return errors.New("usage: varvig principal <add|list|remove> ...")
	}
	r, err := repo.Open(".")
	if err != nil {
		return err
	}
	reg := principal.Open(r)
	switch args[0] {
	case "add":
		return principalAdd(r, reg, args[1:])
	case "list":
		return principalList(reg)
	case "remove":
		if len(args) != 2 {
			return errors.New("usage: varvig principal remove <fingerprint>")
		}
		if err := reg.Remove(args[1], author()); err != nil {
			return err
		}
		fmt.Printf("removed %s\n", args[1])
		return nil
	default:
		return fmt.Errorf("principal: unknown subcommand %q", args[0])
	}
}

func principalAdd(r *repo.Repo, reg *principal.Registry, args []string) error {
	var name, kindStr, keyHex string
	for i := 0; i < len(args); i++ {
		switch args[i] {
		case "--name":
			if i+1 >= len(args) {
				return errors.New("principal: --name requires a value")
			}
			name = args[i+1]
			i++
		case "--kind":
			if i+1 >= len(args) {
				return errors.New("principal: --kind requires a value")
			}
			kindStr = args[i+1]
			i++
		case "--key":
			if i+1 >= len(args) {
				return errors.New("principal: --key requires a value")
			}
			keyHex = args[i+1]
			i++
		default:
			return fmt.Errorf("principal: unknown argument %q", args[i])
		}
	}
	if name == "" || kindStr == "" {
		return errors.New("usage: varvig principal add --name <n> --kind human|agent|bridge [--key <hex>]")
	}
	kind, err := parseKind(kindStr)
	if err != nil {
		return err
	}

	var key ed25519.PublicKey
	if keyHex != "" {
		b, err := hex.DecodeString(keyHex)
		if err != nil || len(b) != ed25519.PublicKeySize {
			return fmt.Errorf("principal: --key must be a %d-byte hex ed25519 public key", ed25519.PublicKeySize)
		}
		key = ed25519.PublicKey(b)
	} else {
		// Self-registration: use the active identity's public key.
		id, err := identity.Resolve("", os.Getenv)
		if err != nil {
			return err
		}
		defer id.Close()
		key = id.PublicKey.Key
	}

	p := object.Principal{Key: key, Name: name, Kind: kind}
	if err := reg.Add(p, author(), time.Now().Unix()); err != nil {
		return err
	}
	fmt.Printf("added %s (%s) %s\n", name, kind, principal.Fingerprint(p))
	return nil
}

func principalList(reg *principal.Registry) error {
	list, err := reg.List()
	if err != nil {
		return err
	}
	if len(list) == 0 {
		fmt.Println("(no principals)")
		return nil
	}
	for _, p := range list {
		fmt.Printf("%-7s %-24s %s\n", p.Kind, p.Name, principal.Fingerprint(p))
	}
	return nil
}

func parseKind(s string) (object.Kind, error) {
	switch s {
	case "human":
		return object.KindHuman, nil
	case "agent":
		return object.KindAgent, nil
	case "bridge":
		return object.KindBridge, nil
	default:
		return object.KindUnknown, fmt.Errorf("principal: unknown kind %q (want human|agent|bridge)", s)
	}
}
