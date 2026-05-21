package daemon

import (
	"fmt"
	"net"
	"os"
	"os/user"
	"strconv"
)

// ListenSocket binds a unix-domain socket at path with mode 0660 and
// (best-effort) the given group ownership. A stale file at path is
// removed first so daemon restarts after an unclean shutdown still
// succeed. groupName == "" or a missing group is non-fatal — the
// socket is still created, just with whatever default group the
// daemon's effective UID maps to.
func ListenSocket(path, groupName string) (net.Listener, error) {
	_ = os.Remove(path)

	l, err := net.Listen("unix", path)
	if err != nil {
		return nil, fmt.Errorf("listen unix %s: %w", path, err)
	}
	if err := os.Chmod(path, 0o660); err != nil {
		_ = l.Close()
		return nil, fmt.Errorf("chmod socket: %w", err)
	}
	if groupName != "" {
		if g, lookupErr := user.LookupGroup(groupName); lookupErr == nil {
			gid, _ := strconv.Atoi(g.Gid)
			// Ignore chown errors — non-root tests will fail this but the
			// socket itself is fine. Production runs as root.
			_ = os.Chown(path, os.Geteuid(), gid)
		}
	}
	return l, nil
}
