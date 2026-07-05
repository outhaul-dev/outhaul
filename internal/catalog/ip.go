package catalog

import "net"

// DetectServerIP guesses the host's outbound IPv4 address for generated
// sslip.io domains. Dialing UDP sends no packet — it just asks the kernel
// which local address would route to a public one. Returns "" when the host
// has no route (generated domains then need editing, nothing breaks).
func DetectServerIP() string {
	conn, err := net.Dial("udp4", "1.1.1.1:53")
	if err != nil {
		return ""
	}
	defer conn.Close()
	if addr, ok := conn.LocalAddr().(*net.UDPAddr); ok {
		return addr.IP.String()
	}
	return ""
}
