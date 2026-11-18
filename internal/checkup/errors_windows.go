package checkup

import (
	"errors"
	"golang.org/x/sys/windows"
)

func platformNetworkCode(err error) string {
	switch {
	case errors.Is(err, windows.WSAEADDRINUSE):
		return "port_in_use"
	case errors.Is(err, windows.WSAEADDRNOTAVAIL):
		return "address_unavailable"
	case errors.Is(err, windows.WSAEACCES):
		return "permission_denied"
	case errors.Is(err, windows.WSAECONNREFUSED):
		return "connection_refused"
	default:
		return "network_failed"
	}
}
