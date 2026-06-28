//go:build darwin && cgo

package main

/*
#include <launch.h>
#include <stdlib.h>
*/
import "C"

import (
	"log"
	"net"
	"os"
	"unsafe"
)

// launchdSocketName is the entry in the launchd job's <Sockets> dict to adopt.
// Override with $PROXY_MCP_LAUNCHD_SOCKET if the .plist names it differently.
const launchdSocketName = "Listeners"

// launchdListener adopts a launchd socket-activation fd, the macOS analogue of
// systemd's LISTEN_FDS. It returns nil (not under launchd, or no such socket)
// so buildListener falls through to systemd/self-bind. This is what gives the
// brew/launchd service idle-shutdown parity with the systemd .socket path: the
// process exits when idle (--idle-timeout) and launchd re-launches it on the
// next connection, with zero resident footprint in between.
func launchdListener() net.Listener {
	name := launchdSocketName
	if env := os.Getenv("PROXY_MCP_LAUNCHD_SOCKET"); env != "" {
		name = env
	}
	cName := C.CString(name)
	defer C.free(unsafe.Pointer(cName))

	var cFds *C.int
	var cCount C.size_t
	// launch_activate_socket is the only supported launchd checkout API; it
	// returns the listening fds the .plist's <Sockets> entry created.
	if rc := C.launch_activate_socket(cName, &cFds, &cCount); rc != 0 {
		// ENOENT (no such socket) just means "not launched this way" — stay quiet
		// and let the caller self-bind. Other errors are worth a line.
		if rc != C.int(0x2) { // ENOENT
			log.Printf("launchd: launch_activate_socket(%q) failed: errno %d", name, int(rc))
		}
		return nil
	}
	defer C.free(unsafe.Pointer(cFds))
	if cCount < 1 {
		return nil
	}
	fds := unsafe.Slice(cFds, int(cCount))
	f := os.NewFile(uintptr(fds[0]), "launchd-activation-socket")
	if f == nil {
		return nil
	}
	defer func() { _ = f.Close() }() // net.FileListener dups the fd
	ln, err := net.FileListener(f)
	if err != nil {
		log.Printf("launchd: adopting activation socket failed: %v", err)
		return nil
	}
	log.Printf("launchd activation: adopted listener on %s", ln.Addr())
	return ln
}
