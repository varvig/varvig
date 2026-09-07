package main

// The assertion shell: how an execution layer contributes what it believes.
//
// internal/assertion was a library with no caller until this verb existed, which
// meant an agent or a factory could not actually contribute an assertion. The
// gate deliberately does not get a write verb for this — see the note on
// `varvig assert add` below — so the CLI is the shell, which also keeps the
// gate's capability set a strict subset of the CLI's (U3).

import (
	"errors"
	"fmt"
	"os"
	"strings"
	"time"

	"github.com/dividebyzero/claude-experiments/varvig/internal/assertion"
	"github.com/dividebyzero/claude-experiments/varvig/internal/attest"
	"github.com/dividebyzero/claude-experiments/varvig/internal/core"
	"github.com/dividebyzero/claude-experiments/varvig/internal/edge"
	"github.com/dividebyzero/claude-experiments/varvig/internal/graphnode"
	"github.com/dividebyzero/claude-experiments/varvig/internal/identity"
	"github.com/dividebyzero/claude-experiments/varvig/internal/multihash"
	"github.com/dividebyzero/claude-experiments/varvig/internal/repo"
)

func cmdAssert(args []string) error {
	if len(args) == 0 {
		return errors.New("usage: varvig assert add|promote|list ...")
	}
	r, err := repo.Open(".")
	if err != nil {
		return err
	}
	switch args[0] {
	case "add":
		return assertAdd(r, args[1:])
	case "promote":
		return assertPromote(r, args[1:])
	case "list":
		return assertList(r, args[1:])
	}
	return fmt.Errorf("unknown assert subcommand %q", args[0])
}

// assertAdd records a claim.
//
// The asserting principal is the identity's fingerprint, not an argument: an
// assertion's whole warrant is who said it, and letting a caller type that in
// would make the field decorative. Strength is required rather than defaulted,
// because a default would silently overclaim authority on every call that forgot
// to think about it.
func assertAdd(r *repo.Repo, args []string) error {
	var from, to, strengthArg, model, modelVersion, sampling string
	var edgeType string
	for i := 0; i < len(args); i++ {
		switch args[i] {
		case "--from", "--to", "--strength", "--model", "--model-version", "--sampling":
			if i+1 >= len(args) {
				return fmt.Errorf("%s needs a value", args[i])
			}
			v := args[i+1]
			switch args[i] {
			case "--from":
				from = v
			case "--to":
				to = v
			case "--strength":
				strengthArg = v
			case "--model":
				model = v
			case "--model-version":
				modelVersion = v
			case "--sampling":
				sampling = v
			}
			i++
		default:
			if edgeType != "" {
				return fmt.Errorf("unexpected argument %q", args[i])
			}
			edgeType = args[i]
		}
	}
	if edgeType == "" || from == "" || to == "" || strengthArg == "" {
		return errors.New("usage: varvig assert add <edge-type> --from <endpoint> --to <endpoint> " +
			"--strength weak|delegated|strong [--model M --model-version V --sampling S]")
	}
	strength, err := parseStrength(strengthArg)
	if err != nil {
		return err
	}
	src, err := resolveEndpoint(r, from)
	if err != nil {
		return fmt.Errorf("--from %q: %w", from, err)
	}
	dst, err := resolveEndpoint(r, to)
	if err != nil {
		return fmt.Errorf("--to %q: %w", to, err)
	}
	observed, err := graphRev(r, nil)
	if err != nil {
		return err
	}
	id, err := identity.Resolve("", os.Getenv)
	if err != nil {
		return err
	}
	claim := assertion.Claim{
		Source: src, Target: dst, Type: edgeType,
		ObservedUnder: observed,
		Principal:     attest.Fingerprint(id.Signer.Public()),
		Strength:      strength,
	}

	var noteID multihash.Multihash
	switch {
	case model != "" || modelVersion != "" || sampling != "":
		// Partial model provenance is refused by the library, which is the point:
		// an assertion whose model is half-named cannot be audited or distrusted
		// selectively later.
		noteID, err = assertion.FromInference(r, claim,
			assertion.Inference{Model: model, ModelVersion: modelVersion, Sampling: sampling}, time.Now().Unix())
	default:
		noteID, err = assertion.FromPrincipal(r, claim, time.Now().Unix())
	}
	if err != nil {
		return err
	}
	fmt.Printf("asserted %s: %s -> %s\n", edgeType, src.Key(), dst.Key())
	fmt.Printf("  edge %s (unpromoted; assertions never gate a merge)\n", noteID.Hex())
	return nil
}

// assertPromote signs an attestation naming the edge's hash. It is the only path
// from claim to durable project knowledge, and it is signed and auditable.
func assertPromote(r *repo.Repo, args []string) error {
	if len(args) == 0 {
		return errors.New("usage: varvig assert promote <edge-id> --strength weak|delegated|strong [--rationale R]")
	}
	target, err := multihash.ParseHex(args[0])
	if err != nil {
		return fmt.Errorf("edge id %q: %w", args[0], err)
	}
	var strengthArg, rationale string
	for i := 1; i < len(args); i++ {
		switch args[i] {
		case "--strength":
			if i+1 >= len(args) {
				return errors.New("--strength needs a value")
			}
			strengthArg, i = args[i+1], i+1
		case "--rationale":
			if i+1 >= len(args) {
				return errors.New("--rationale needs a value")
			}
			rationale, i = args[i+1], i+1
		default:
			return fmt.Errorf("unexpected argument %q", args[i])
		}
	}
	if strengthArg == "" {
		return errors.New("varvig assert promote: --strength is required")
	}
	strength, err := parseStrength(strengthArg)
	if err != nil {
		return err
	}
	id, err := identity.Resolve("", os.Getenv)
	if err != nil {
		return err
	}
	att, err := assertion.Promote(r, target, id.Signer, strength, rationale, time.Now().Unix())
	if err != nil {
		return err
	}
	fmt.Printf("promoted edge %s\n", target.Hex())
	fmt.Printf("  attestation %s (%s, bound to the edge's hash)\n", att.Hex(), strength)
	return nil
}

// assertList shows the edges recorded against an anchor, with their promotion
// status. Provenance is printed for every one, because that is what a reader
// needs in order to decide how much to trust it — and there is no score to print
// instead.
func assertList(r *repo.Repo, args []string) error {
	if len(args) != 1 {
		return errors.New("usage: varvig assert list <anchor>")
	}
	anchor, err := resolveAnchor(r, args[0])
	if err != nil {
		return err
	}
	edges, err := edge.List(r, anchor)
	if err != nil {
		return err
	}
	if len(edges) == 0 {
		fmt.Printf("no edges recorded against %s\n", anchor.Hex())
		return nil
	}
	for _, en := range edges {
		p := en.Edge.Provenance()
		promoted, approvals, err := assertion.Promoted(r, en.Note)
		if err != nil {
			return err
		}
		status := "unpromoted"
		if promoted {
			status = fmt.Sprintf("promoted (%d approval(s))", len(approvals))
		}
		fmt.Printf("%s  [%s]\n", en.Note.Hex(), en.Edge.Type())
		fmt.Printf("  %s -> %s\n", en.Edge.Source().Key(), en.Edge.Target().Key())
		fmt.Printf("  %s/%s by %s — %s\n", p.Class, p.Strength, p.Principal, status)
		if p.Model != "" {
			fmt.Printf("  model %s %s (%s)\n", p.Model, p.ModelVersion, p.Sampling)
		}
	}
	return nil
}

// resolveEndpoint turns a command-line endpoint into a graph node.
//
// A "system:id" spelling is a foreign row; anything else names something in this
// repository, resolved to its content hash. A path is accepted for convenience
// and resolved through HEAD's tree — but the node is the content it resolved to,
// never the path, because a path is a resolution and not an identity (§3.1).
func resolveEndpoint(r *repo.Repo, s string) (graphnode.Node, error) {
	if system, foreign, ok := strings.Cut(s, ":"); ok {
		if _, err := multihash.ParseHex(s); err != nil {
			return graphnode.External(system, foreign)
		}
	}
	id, err := resolveAnchor(r, s)
	if err != nil {
		return nil, err
	}
	return graphnode.Object(id)
}

// resolveAnchor resolves a ref, a hash, or a path in HEAD's tree to a content id.
func resolveAnchor(r *repo.Repo, s string) (multihash.Multihash, error) {
	if id, err := resolve(r, s); err == nil {
		return id, nil
	}
	head, err := graphRev(r, nil)
	if err != nil {
		return nil, fmt.Errorf("%q is not a ref, hash, or path (and HEAD is unreadable: %w)", s, err)
	}
	files, err := core.TreeFiles(r, head)
	if err != nil {
		return nil, err
	}
	if blob, ok := files[s]; ok {
		return blob, nil
	}
	return nil, fmt.Errorf("%q is not a ref, a hash, or a path in HEAD", s)
}
