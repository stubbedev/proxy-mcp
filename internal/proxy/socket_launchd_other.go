//go:build !darwin || !cgo

package proxy

import "net"

// launchdListener is darwin-only and needs cgo (launch_activate_socket). Without
// both there is nothing to adopt, so buildListener falls through to
// systemd/self-bind.
func launchdListener() net.Listener { return nil }
