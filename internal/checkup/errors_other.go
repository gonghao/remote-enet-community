//go:build !windows

package checkup

func platformNetworkCode(error) string { return "network_failed" }
