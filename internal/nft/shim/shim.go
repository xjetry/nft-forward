// Package shim implements per-firewall compatibility layers that inject
// daemon-managed accept rules into well-known user-extension chains
// (e.g. Docker's DOCKER-USER, ufw's ufw-user-forward). This lets
// nft-forward keep working on hosts where some other tool has set the
// FORWARD chain default policy to drop.
//
// Every shim is best-effort: a failure inside a shim never blocks the
// core nft_forward table apply. Each shim's Detect should be cheap so
// callers can poll it on every Apply.
package shim

import (
	"bytes"
	"os/exec"
	"strings"

	"nft-forward/internal/nft"
)

// OwnerComment is the literal string tagged on every rule that any shim
// inserts into a foreign chain. Cleanup walks the chain and deletes by
// this exact comment.
const OwnerComment = "nft-forward managed"

// FirewallState carries what the shims need to make the host pass our traffic:
// FORWARD accepts for kernel DNAT targets, and INPUT accepts for userspace TCP
// listen ports.
type FirewallState struct {
	ForwardRules []nft.Rule
	ListenPorts  []ListenPort
}

// ListenPort is one userspace listener the firewall must allow inbound.
type ListenPort struct {
	Proto string // "tcp"
	Port  int
}

// ForwardShim is one firewall-tool integration. Implementations live
// alongside this file (docker_user.go, ufw.go, ...).
type ForwardShim interface {
	// Name returns a short identifier used in logs.
	Name() string

	// Detect returns true when this shim's target chain exists right
	// now. Cheap; called on every Sync.
	Detect() bool

	// Sync makes the target chain(s) reflect state: deletes any leftover
	// owner-tagged rules, inserts current ones. No-op when Detect is
	// false. Idempotent.
	Sync(state FirewallState) error

	// Cleanup deletes every owner-tagged rule from the target chain(s).
	// No-op when Detect is false. Idempotent.
	Cleanup() error
}

// nftRunner runs `nft <args>` and returns combined stdout. Production
// callers use defaultNftRunner; tests substitute a fake.
type nftRunner func(args ...string) (string, error)

// nftScriptRunner pipes `script` into `nft -f -`. Production callers
// use defaultNftScriptRunner; tests substitute a fake.
type nftScriptRunner func(script string) error

// cleanupChain lists one chain, finds all owner-tagged rules, and emits a
// delete-only script to remove them. Returns nil when the chain is absent
// or already clean. Shared by every shim's Cleanup method.
func cleanupChain(runNft nftRunner, runScript nftScriptRunner, family, table, chain string) error {
	out, err := runNft("-a", "list", "chain", family, table, chain)
	if err != nil {
		return nil // chain absent; nothing to clean
	}
	stale := parseShimHandles(out)
	if len(stale) == 0 {
		return nil
	}
	var script string
	for _, h := range stale {
		script += formatDelete(family, table, chain, h)
	}
	return runScript(script)
}

func defaultNftRunner(args ...string) (string, error) {
	cmd := exec.Command("nft", args...)
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	err := cmd.Run()
	if err != nil {
		// Caller decides whether the error means "chain missing" or
		// something fatal; we surface stderr in the wrapped error.
		return stdout.String(), &nftError{args: args, err: err, stderr: stderr.String()}
	}
	return stdout.String(), nil
}

func defaultNftScriptRunner(script string) error {
	cmd := exec.Command("nft", "-f", "-")
	cmd.Stdin = bytes.NewBufferString(script)
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		return &nftError{args: []string{"-f", "-"}, err: err, stderr: stderr.String()}
	}
	return nil
}

type nftError struct {
	args   []string
	err    error
	stderr string
}

func (e *nftError) Error() string {
	joined := strings.Join(e.args, " ")
	if e.stderr == "" {
		return "nft " + joined + ": " + e.err.Error()
	}
	return "nft " + joined + ": " + e.err.Error() + ": " + e.stderr
}

// Registry holds the built-in shims and dispatches Sync/Cleanup across
// all of them. The set of shims is fixed at construction time; we don't
// support dynamic registration because the list is small and known.
type Registry struct {
	shims []ForwardShim
}

// DefaultRegistry returns the built-in shim set: docker-user, ufw.
// Tests can construct a Registry literal with arbitrary stubs.
func DefaultRegistry() *Registry {
	return &Registry{
		shims: []ForwardShim{
			NewDockerUserShim(),
			NewUfwShim(),
		},
	}
}

// Names lists the shim Name()s in registration order, for logging.
func (r *Registry) Names() []string {
	names := make([]string, 0, len(r.shims))
	for _, s := range r.shims {
		names = append(names, s.Name())
	}
	return names
}

// DetectedNames returns the names of shims whose Detect() returns true
// right now. Cheap; used by daemon startup probe.
func (r *Registry) DetectedNames() []string {
	var names []string
	for _, s := range r.shims {
		if s.Detect() {
			names = append(names, s.Name())
		}
	}
	return names
}

// SyncAll runs Sync on every detected shim. A failure in one shim does
// not skip the others — failures are aggregated and returned at the
// end so the caller can log them. Detect failures are not surfaced.
func (r *Registry) SyncAll(state FirewallState) error {
	var errs []string
	for _, s := range r.shims {
		if !s.Detect() {
			continue
		}
		if err := s.Sync(state); err != nil {
			errs = append(errs, s.Name()+": "+err.Error())
		}
	}
	if len(errs) == 0 {
		return nil
	}
	return &aggregateError{errs}
}

// CleanupAll mirrors SyncAll for shutdown / uninstall paths.
func (r *Registry) CleanupAll() error {
	var errs []string
	for _, s := range r.shims {
		if !s.Detect() {
			continue
		}
		if err := s.Cleanup(); err != nil {
			errs = append(errs, s.Name()+": "+err.Error())
		}
	}
	if len(errs) == 0 {
		return nil
	}
	return &aggregateError{errs}
}

type aggregateError struct {
	errs []string
}

func (e *aggregateError) Error() string {
	return "shim: " + strings.Join(e.errs, "; ")
}
