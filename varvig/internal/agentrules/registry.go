// Package agentrules generates the agent-facing rules files written by
// `varvig init --agent-rules` (see the feature spec). Two files, two policies:
//
//   - VARVIG-AGENTS.md is generated and overwritten in full, every time. It is
//     rendered from the registries in this file — the single source of truth for
//     the agent-facing surface — so it can never describe a flag that no longer
//     exists.
//   - AGENTS.md gets a small pointer block appended once and is never rewritten.
//
// Everything here is deterministic: same registries + same repo facts ⇒
// byte-identical output. No timestamps, no hostnames, sorted iteration.
package agentrules

// IntentSchemaVersion is the version of the intent (provenance) object schema the
// rules describe. It is bumped when the intent object's agent-facing shape
// changes, so a generated file states which schema it was written against.
const IntentSchemaVersion = 1

// Command is one entry in the CLI surface. AgentFacing entries are rendered into
// VARVIG-AGENTS.md; the rest must carry a NoRules reason explaining why an agent
// driving varvig does not need them. The completeness test fails the build if a
// command is neither rendered nor excused, which is the actual anti-drift
// mechanism — a new command cannot ship without a rules decision.
type Command struct {
	Name        string
	Summary     string
	Usage       string
	AgentFacing bool
	NoRules     string
	Flags       []Flag
}

// Flag is an agent-relevant flag on a command. Same rule as Command: rendered if
// AgentFacing, otherwise excused with NoRules.
type Flag struct {
	Name        string
	Summary     string
	AgentFacing bool
	NoRules     string
}

// IntentField is one field of the intent/provenance object. The agent-facing
// ones are the fields an agent is responsible for; the rest are filled by the
// gate or the harness and are excused with NoRules.
type IntentField struct {
	Name        string
	Summary     string
	Required    bool
	AgentFacing bool
	NoRules     string
}

// ErrorEntry is one agent-facing runtime error, rendered as what/why/do. The
// stable Code is documentation: agents read these strings far more often than
// any docs site.
type ErrorEntry struct {
	Code string
	What string
	Why  string
	Do   string
}

// CredentialRule is one row of the credential scope table: a capability and
// whether a propose-only task credential has it.
type CredentialRule struct {
	Capability string
	Allowed    bool
	Note       string
}

// Commands is the full CLI surface, agent-facing or excused. It mirrors the
// dispatch table in package main; a coverage test there asserts every real
// command appears here, so this registry and the CLI cannot drift apart.
var Commands = []Command{
	{
		Name: "read", AgentFacing: true,
		Summary: "Query repository state as JSON — objects, trees, blobs, changes, logs, refs, and proposals.",
		Usage:   "varvig read <object|tree|blob|change|log|refs|proposals> [args]",
	},
	{
		Name: "cat-object", AgentFacing: true,
		Summary: "Print one object's content or a summary of its fields, by hash.",
		Usage:   "varvig cat-object <id>",
	},
	{
		Name: "show-ref", AgentFacing: true,
		Summary: "List refs, or resolve one ref name to its current hash.",
		Usage:   "varvig show-ref [name]",
	},
	{
		Name: "log", AgentFacing: true,
		Summary: "Walk the change DAG from HEAD (or a given ref/hash), newest first.",
		Usage:   "varvig log [ref|id]",
	},
	{
		Name: "verify", AgentFacing: true,
		Summary: "Check provenance and signatures on the changes reachable from a ref/hash.",
		Usage:   "varvig verify [ref|id]",
	},
	{
		Name: "affected", AgentFacing: true,
		Summary: "Show which files a change touched and, transitively, which files depend on them. Check this before proposing.",
		Usage:   "varvig affected [<base> <new>]",
	},
	{
		Name: "task", AgentFacing: true,
		Summary: "Mint an ephemeral, scoped, propose-only task credential and a sparse checkout of its read set.",
		Usage:   "varvig task start [--scope S] [--ttl DUR] [--base REF] [dir]",
		Flags: []Flag{
			{Name: "--scope", AgentFacing: true, Summary: "path prefix you may read and propose within; the capability boundary"},
			{Name: "--ttl", AgentFacing: true, Summary: "how long the credential lives before it expires"},
			{Name: "--base", AgentFacing: true, Summary: "the change your task reads and proposes from (defaults to HEAD)"},
		},
	},
	{
		Name: "mcp", AgentFacing: true,
		Summary: "Open the MCP gate over stdio: the scoped, propose-only interface an agent harness drives. Reads are logged into provenance; you may propose, never promote.",
		Usage:   "varvig mcp [--scope S] [--ttl DUR] [--base REF]",
		Flags: []Flag{
			{Name: "--scope", AgentFacing: true, Summary: "path prefix the gate confines every read and proposed path to"},
			{Name: "--ttl", AgentFacing: true, Summary: "credential lifetime; the gate refuses all calls once expired"},
			{Name: "--base", AgentFacing: true, Summary: "the pinned change the task reads and proposes from"},
		},
	},

	// --- excused: not part of the agent-facing surface ---
	{Name: "init", NoRules: "repository and rules-file setup; run by a human or CI, not by an agent driving changes"},
	{Name: "version", NoRules: "diagnostic; prints the build version"},
	{Name: "whoami", NoRules: "diagnostic; prints the active principal and fingerprint"},
	{Name: "key", NoRules: "identity setup; a human creates keys, an agent is issued a task credential"},
	{Name: "trust", NoRules: "trust-store administration; a human manages allowed keys"},
	{Name: "commit", NoRules: "advances HEAD directly; an agent proposes through the gate and never commits"},
	{Name: "checkout", NoRules: "overwrites the working tree for an edit-then-commit flow a propose-only agent cannot complete; `task start` already materializes the read set"},
	{Name: "promote", NoRules: "promotion is the human-gated step; a propose-only credential can never promote"},
	{Name: "update-ref", NoRules: "moves a ref directly; agents are propose-only and cannot move refs"},
	{Name: "write-tree", NoRules: "plumbing; the propose path writes the tree for you"},
	{Name: "hash-object", NoRules: "plumbing; the gate hashes content as part of a proposal"},
	{Name: "reflog", NoRules: "audit and recovery; a human inspects ref history"},
	{Name: "merge", NoRules: "three-way merge into HEAD; a promotion-side operation, human-driven"},
	{Name: "note", NoRules: "attaches notes via ref updates; outside the propose-only surface"},
	{Name: "attest", NoRules: "governance decisions (approve/veto/policy); a director signs these, and a propose-only agent neither approves nor promotes"},
	{Name: "spec", NoRules: "speculation pool is orchestrator-driven; candidates are added and scored around the agent, not by it"},
	{Name: "tickets", NoRules: "ticket scheduling metadata (scope) and derived blocking; computed by the orchestrator/scheduler around the agent, not driven by a propose-only task"},
	{Name: "hook", NoRules: "configures acceptance gates; a human or admin sets the gates an agent's proposals must pass"},
	{Name: "git-export", NoRules: "git interop; human or CI"},
	{Name: "git-import", NoRules: "git interop; human or CI"},
	{Name: "serve", NoRules: "runs a peer/read-API server; an operator concern"},
	{Name: "clone", NoRules: "peer replication; an operator/CI concern, not part of a scoped task"},
	{Name: "fetch", NoRules: "peer replication; operator/CI"},
	{Name: "push", NoRules: "peer replication; operator/CI"},
	{Name: "daemon", NoRules: "runs the local credential daemon; started by the harness/operator, not driven as a tool"},
	{Name: "gc", NoRules: "repository maintenance; an operator concern"},
	{Name: "conform", NoRules: "format-conformance self-check; run by CI"},
}

// IntentFields describes the intent (provenance) object recorded with every
// proposal. Agent-facing fields are the agent's responsibility; the rest are
// filled by the gate or harness and excused, so the file stays short while the
// completeness test still covers the whole schema.
var IntentFields = []IntentField{
	{Name: "message", Required: true, AgentFacing: true,
		Summary: "the intent of the change: what you were asked to do and what this proposal does. Never leave it empty."},
	{Name: "reasoning", AgentFacing: true,
		Summary: "the plan you followed to produce the change; recorded so a reviewer can judge intent, not just diff."},

	{Name: "authority", NoRules: "set by the gate to your task credential's fingerprint; you cannot forge it"},
	{Name: "context_read", NoRules: "the read set; the gate folds in every hash you resolved, automatically"},
	{Name: "model", NoRules: "generator pinning recorded by the harness"},
	{Name: "model_version", NoRules: "generator pinning recorded by the harness"},
	{Name: "sampling", NoRules: "generator pinning recorded by the harness"},
	{Name: "tool_permissions", NoRules: "capabilities in effect, recorded by the harness"},
	{Name: "tool_hash", NoRules: "hash of the tool binary, recorded by the harness"},
}

// Errors is the agent-facing error catalog rendered into the rules file. Each is
// a real refusal an agent hits while driving the gate, with a recovery action.
var Errors = []ErrorEntry{
	{Code: "VRV-SCOPE-001",
		What: "A read or proposed path is outside your task scope.",
		Why:  "The capability is the read set: a task may only touch paths under its scope.",
		Do:   "Stay within scope, or ask for a task credential with a wider --scope. Do not attempt to widen it yourself."},
	{Code: "VRV-CRED-001",
		What: "Your task credential has expired.",
		Why:  "Credentials are short-lived; expiry is how revocation works, so no call succeeds afterward.",
		Do:   "Request a fresh task credential and re-run your reads before proposing again."},
	{Code: "VRV-PROMO-001",
		What: "A promotion or ref move was refused.",
		Why:  "A propose-only credential can never move a ref; promotion is a separate, human-gated step.",
		Do:   "Leave your work as a proposal. A human or the promotion pipeline decides what lands."},
	{Code: "VRV-GATE-001",
		What: "An acceptance gate vetoed the change.",
		Why:  "The repo's gates (e.g. a pre-commit hook) must pass before a proposal can land.",
		Do:   "Read the gate's output, fix the cause, and propose again. Never try to bypass or disable a gate."},
	{Code: "VRV-CONF-001",
		What: "Your proposal conflicts with a base that has moved.",
		Why:  "The change you proposed from is no longer the tip; landing it would need a merge.",
		Do:   "Re-read from the current base and rebuild your proposal on top of it."},
}

// CredentialScope is the propose-only credential capability table, rendered
// verbatim. These are product constants — they hold regardless of version.
var CredentialScope = []CredentialRule{
	{Capability: "Read files and history within your scope", Allowed: true},
	{Capability: "Read anything outside your scope", Allowed: false, Note: "the scope is the boundary"},
	{Capability: "Propose a signed, speculative change", Allowed: true},
	{Capability: "Promote a proposal / move a ref", Allowed: false, Note: "human-gated"},
	{Capability: "Force, rewrite, or amend existing history", Allowed: false},
	{Capability: "Delete objects, refs, or history", Allowed: false},
	{Capability: "Keep credentials indefinitely", Allowed: false, Note: "they expire on a short TTL"},
}
