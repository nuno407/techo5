//go:build linux && !dot && !spot && !panel

package layout

import (
	"fmt"
	"net"
	"unsafe"

	"golang.org/x/sys/unix"
)

// permanentMAC asks the driver for iface's permanent address over SIOCETHTOOL.
func permanentMAC(iface string) (string, error) {
	const (
		ethtoolGPermAddr = 0x20
		maxAddr          = 32
	)
	// struct ethtool_perm_addr: cmd, size, then the address.
	var req struct {
		cmd, size uint32
		data      [maxAddr]byte
	}
	req.cmd, req.size = ethtoolGPermAddr, maxAddr

	fd, err := unix.Socket(unix.AF_INET, unix.SOCK_DGRAM|unix.SOCK_CLOEXEC, 0)
	if err != nil {
		return "", err
	}
	defer unix.Close(fd)

	// struct ifreq: the name, then a pointer to the request in the union.
	var ifr struct {
		name [unix.IFNAMSIZ]byte
		data unsafe.Pointer
		_    [24 - unsafe.Sizeof(uintptr(0))]byte
	}
	copy(ifr.name[:], iface)
	ifr.data = unsafe.Pointer(&req)
	if _, _, errno := unix.Syscall(unix.SYS_IOCTL, uintptr(fd), unix.SIOCETHTOOL, uintptr(unsafe.Pointer(&ifr))); errno != 0 {
		return "", fmt.Errorf("SIOCETHTOOL %s: %w", iface, errno)
	}
	if req.size != 6 {
		return "", fmt.Errorf("SIOCETHTOOL %s: a %d-byte address", iface, req.size)
	}
	return net.HardwareAddr(req.data[:6]).String(), nil
}
