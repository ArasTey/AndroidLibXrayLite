package libv2ray

import (
	"fmt"
	"sync"

	"github.com/amnezia-vpn/amneziawg-go/v3/conn"
	"github.com/amnezia-vpn/amneziawg-go/v3/device"

	"golang.org/x/sys/unix"
)

// Standalone AmneziaWG tunnel, mirroring the AmneziaVPN Android client:
// the VPN service's TUN fd is handed to amneziawg-go directly together with
// the full UAPI config (device junk lines + peer lines). Used for AWG
// profiles instead of routing them through the Xray wireguard outbound.
var (
	awgMu  sync.Mutex
	awgDev *device.Device
)

// AwgTurnOn brings up an AmneziaWG tunnel on the given VpnService TUN fd.
// uapiConfig is the full UAPI set string (device lines + peer lines).
func AwgTurnOn(fd int32, uapiConfig string, mtu int32) error {
	awgMu.Lock()
	defer awgMu.Unlock()
	AwgTurnOff()

	if fd <= 0 {
		return fmt.Errorf("awg tunnel: invalid TUN fd %d", fd)
	}
	// Wrap a duplicate of the VpnService TUN fd — it is already a fully
	// configured TUN and /dev/net/tun ioctls are EACCES for an unprivileged
	// Android app. Duplicating keeps Go and Kotlin ownership independent.
	dupFd, err := unix.Dup(int(fd))
	if err != nil {
		return fmt.Errorf("awg tunnel: dup fd: %w", err)
	}
	t := newAwgFdTun(int32(dupFd), int(mtu))
	logger := device.NewLogger(device.LogLevelError, "AwgTunnel")
	dev := device.NewDevice(t, conn.NewDefaultBind(), logger)
	if err := dev.IpcSet(uapiConfig); err != nil {
		dev.Close()
		return fmt.Errorf("awg tunnel: ipc set: %w", err)
	}
	if err := dev.Up(); err != nil {
		dev.Close()
		return fmt.Errorf("awg tunnel: up: %w", err)
	}
	awgDev = dev
	return nil
}

// AwgTurnOff stops the running AmneziaWG tunnel, if any.
func AwgTurnOff() {
	if awgDev != nil {
		fmt.Println("AwgTunnel: turning off")
		awgDev.Close()
		awgDev = nil
		fmt.Println("AwgTunnel: off")
	} else {
		fmt.Println("AwgTunnel: nothing to turn off")
	}
}

// AwgIsRunning reports whether a standalone tunnel is active.
func AwgIsRunning() bool {
	awgMu.Lock()
	defer awgMu.Unlock()
	return awgDev != nil
}
