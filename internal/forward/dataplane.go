package forward

import (
	"context"
	"log"
	"sync"

	"nft-forward/internal/nft"
	"nft-forward/internal/nft/shim"
)

// Dataplane orchestrates the kernel and userspace backends plus firewall
// integration behind one Reconcile/Counters/Close surface. It keeps a single
// rollback anchor (lastUserspace): nft is atomic so the kernel needs none, but
// a kernel failure after a successful userspace step would otherwise strand
// the userspace layer ahead of the daemon's logical state (refreshOnce only
// re-applies on an actual resolved-IP change, so it would not self-correct).
type Dataplane struct {
	kernel    kernelReconciler
	userspace *userspaceBackend
	fw        firewall

	mu            sync.Mutex
	lastUserspace []nft.Rule
}

// Config wires dependencies. Shims defaults to the built-in registry.
type Config struct {
	Iface string
	Shims *shim.Registry
}

func New(cfg Config) *Dataplane {
	shims := cfg.Shims
	if shims == nil {
		shims = shim.DefaultRegistry()
	}
	return &Dataplane{
		kernel:    kernelBackend{iface: cfg.Iface},
		userspace: newUserspaceBackend(),
		fw:        firewall{shims: shims},
	}
}

// Reconcile partitions rules and applies userspace first, then kernel, then
// firewall (best-effort). A hard kernel failure rolls userspace back to the
// last good set.
func (d *Dataplane) Reconcile(ctx context.Context, rules []nft.Rule) error {
	kernelRules, userspaceRules, err := Partition(rules)
	if err != nil {
		return err
	}

	d.mu.Lock()
	defer d.mu.Unlock()

	if err := d.userspace.Reconcile(userspaceRules); err != nil {
		return err
	}
	if err := d.kernel.Reconcile(kernelRules); err != nil {
		if rbErr := d.userspace.Reconcile(d.lastUserspace); rbErr != nil {
			log.Printf("dataplane: userspace rollback after kernel failure also failed: %v", rbErr)
		}
		return err
	}
	if err := d.fw.Sync(kernelRules, listenPortsOf(userspaceRules)); err != nil {
		log.Printf("dataplane: firewall sync: %v", err)
	}
	d.lastUserspace = append([]nft.Rule(nil), userspaceRules...)
	return nil
}

func (d *Dataplane) Counters() ([]Counter, error) {
	kc, err := d.kernel.Counters()
	if err != nil {
		return nil, err
	}
	d.mu.Lock()
	uc := d.userspace.Counters()
	d.mu.Unlock()
	return append(kc, uc...), nil
}

func (d *Dataplane) Close(ctx context.Context) error {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.userspace.Close()
	return d.fw.Cleanup()
}

// DetectedShims exposes firewall detection for the daemon's startup
// FORWARD-policy warning.
func (d *Dataplane) DetectedShims() []string {
	return d.fw.DetectedNames()
}

func (d *Dataplane) SetPoolSize(n int) {
	d.userspace.SetPoolSize(n)
}

func listenPortsOf(rules []nft.Rule) []shim.ListenPort {
	out := make([]shim.ListenPort, 0, len(rules))
	for _, r := range rules {
		out = append(out, shim.ListenPort{Proto: "tcp", Port: r.SrcPort})
	}
	return out
}
