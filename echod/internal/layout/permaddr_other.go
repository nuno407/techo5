//go:build !linux && !dot && !spot && !panel

package layout

import "errors"

// permanentMAC: only Linux has SIOCETHTOOL.
func permanentMAC(string) (string, error) { return "", errors.New("no SIOCETHTOOL here") }
