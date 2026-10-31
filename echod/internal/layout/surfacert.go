//go:build !dot && !spot && !panel

package layout

import (
	"net"
)

// The Surface RT has no idme. Its Wi-Fi card (a Marvell 88W8797 on mwifiex) carries the factory
// address, Microsoft's own, but /sys/class/net/mlan0/address is not it: NetworkManager gives the
// interface an address of its own (locally administered, addr_assign_type 3 on the unit). The
// factory one is the driver's permanent address, which ETHTOOL_GPERMADDR asks for (`ip -d link`
// shows it as permaddr). The interface's current address stands in where that cannot be read.

// surfaceRTMAC is the Wi-Fi card's factory MAC.
func surfaceRTMAC() (string, bool) {
	iface := WifiInterface()
	if mac, err := permanentMAC(iface); err == nil && MAC(mac) != "" {
		return mac, true
	}
	i, err := net.InterfaceByName(iface)
	if err != nil || len(i.HardwareAddr) != 6 {
		return "", false
	}
	return i.HardwareAddr.String(), true
}
