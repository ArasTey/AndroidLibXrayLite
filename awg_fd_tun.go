package libv2ray

import (
	"fmt"
	"os"
	"sync"

	"github.com/amnezia-vpn/amneziawg-go/v3/tun"
)

// awgFdTun is a minimal tun.Device that wraps an existing VpnService TUN
// file descriptor with plain read/write of single IP packets. The VpnService
// fd is already a fully configured TUN — no /dev/net/tun ioctls are used
// (those fail with EACCES inside an unprivileged Android app).
type awgFdTun struct {
	tunFile   *os.File
	mtu       int
	events    chan tun.Event
	closeOnce sync.Once
}

func newAwgFdTun(fd int32, mtu int) *awgFdTun {
	f := os.NewFile(uintptr(fd), "awg-vpn-tun")
	return &awgFdTun{
		tunFile: f,
		mtu:     mtu,
		events:  make(chan tun.Event, 4),
	}
}

func (t *awgFdTun) File() *os.File { return t.tunFile }

func (t *awgFdTun) Read(bufs [][]byte, sizes []int, offset int) (int, error) {
	if len(bufs) == 0 {
		return 0, nil
	}
	n, err := t.tunFile.Read(bufs[0][offset:])
	sizes[0] = n
	if err != nil {
		return 0, err
	}
	return 1, nil
}

func (t *awgFdTun) Write(bufs [][]byte, offset int) (int, error) {
	written := 0
	for i, buf := range bufs {
		if len(buf) <= offset {
			continue
		}
		if _, err := t.tunFile.Write(buf[offset:]); err != nil {
			return written, err
		}
		written++
		_ = i
	}
	return written, nil
}

func (t *awgFdTun) MTU() (int, error) { return t.mtu, nil }

func (t *awgFdTun) Name() (string, error) { return "awg0", nil }

func (t *awgFdTun) Events() <-chan tun.Event { return t.events }

func (t *awgFdTun) BatchSize() int { return 1 }

func (t *awgFdTun) Close() error {
	// Go owns the (duplicated) fd after Kotlin detached it — close on stop,
	// otherwise the TUN stays up and the VPN never disconnects.
	t.closeOnce.Do(func() {
		close(t.events)
		err := t.tunFile.Close()
		if err != nil {
			fmt.Println("AwgTunnel: close fd error:", err)
		} else {
			fmt.Println("AwgTunnel: TUN fd closed")
		}
	})
	return nil
}

var _ tun.Device = (*awgFdTun)(nil)
