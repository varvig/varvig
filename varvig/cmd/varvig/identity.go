package main

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"

	"github.com/dividebyzero/claude-experiments/varvig/internal/identity"
)

// cmdWhoami prints the active principal: its name, SSH SHA256 fingerprint, and
// where the key was found (auth design §2.2, "the fingerprint *is* the
// identity"). There is nothing to register and nothing to configure.
func cmdWhoami(args []string) error {
	id, err := identity.Resolve("", os.Getenv)
	if err != nil {
		return err
	}
	defer id.Close()

	name := id.PublicKey.Comment
	if name == "" {
		name = "(no name)"
	}
	fmt.Printf("%s  %s  (source: %s)\n", name, id.Fingerprint(), id.Source)
	if !id.CanSign() {
		fmt.Fprintln(os.Stderr, "note: this identity cannot sign (key is encrypted and no ssh-agent is available); "+
			"start ssh-agent and `ssh-add` your key to promote changes")
	}
	return nil
}

// cmdKey handles key management. Only `key init` exists at v1, for the rare user
// with no SSH key at all (auth design §2.3).
func cmdKey(args []string) error {
	if len(args) < 1 {
		return errors.New("usage: varvig key init --name <name>")
	}
	switch args[0] {
	case "init":
		name := ""
		rest := args[1:]
		for i := 0; i < len(rest); i++ {
			switch rest[i] {
			case "--name":
				if i+1 >= len(rest) {
					return errors.New("key init: --name requires a value")
				}
				name = rest[i+1]
				i++
			default:
				return fmt.Errorf("key init: unknown argument %q", rest[i])
			}
		}
		if name == "" {
			return errors.New("usage: varvig key init --name <name>")
		}
		home, err := os.UserHomeDir()
		if err != nil {
			return err
		}
		dir := filepath.Join(home, ".varvig", "keys")
		id, err := identity.InitKey(dir, name)
		if err != nil {
			return err
		}
		fmt.Printf("created %s\n%s  %s  (source: %s)\n",
			filepath.Join(dir, name+".key"), name, id.Fingerprint(), id.Source)
		fmt.Fprintln(os.Stderr, "note: if you already have ~/.ssh/id_ed25519, prefer that — Varvig reads it directly")
		return nil
	default:
		return fmt.Errorf("key: unknown subcommand %q (want: init)", args[0])
	}
}
