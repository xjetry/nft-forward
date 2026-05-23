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

	"nft-forward/internal/nft"
)

// OwnerComment is the literal string tagged on every rule that any shim
// inserts into a foreign chain. Cleanup walks the chain and deletes by
// this exact comment.
const OwnerComment = "nft-forward managed"

// ForwardShim is one firewall-tool integration. Implementations live
// alongside this file (docker_user.go, ufw.go, ...).
type ForwardShim interface {
	// Name returns a short identifier used in logs.
	Name() string

	// Detect returns true when this shim's target chain exists right
	// now. Cheap; called on every Sync.
	Detect() bool

	// Sync makes the target chain reflect rules: deletes any leftover
	// owner-tagged rule, inserts current ones. No-op when Detect is
	// false. Idempotent.
	Sync(rules []nft.Rule) error

	// Cleanup deletes every owner-tagged rule from the target chain.
	// No-op when Detect is false. Idempotent.
	Cleanup() error
}

// nftRunner runs `nft <args>` and returns combined stdout. Production
// callers use defaultNftRunner; tests substitute a fake.
type nftRunner func(args ...string) (string, error)

// nftScriptRunner pipes `script` into `nft -f -`. Production callers
// use defaultNftScriptRunner; tests substitute a fake.
type nftScriptRunner func(script string) error

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
	if e.stderr == "" {
		return "nft " + joinArgs(e.args) + ": " + e.err.Error()
	}
	return "nft " + joinArgs(e.args) + ": " + e.err.Error() + ": " + e.stderr
}

func joinArgs(a []string) string {
	out := ""
	for i, s := range a {
		if i > 0 {
			out += " "
		}
		out += s
	}
	return out
}
