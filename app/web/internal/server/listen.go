package server

import (
	"errors"
	"fmt"
	"net"
	"net/netip"
	"strings"
)

// RejectUnprotectedListen refuses plain HTTP except on a literal loopback IP or
// the literal hostname "localhost". Hostnames are not resolved: a DNS name that
// happens to point at 127.0.0.1 is still rejected. tlsEnabled means this process
// terminates TLS. proxyTerminated is the explicit tls_terminated_by_proxy mode;
// trusted proxy IPs alone are not TLS. Proxy mode still requires the operator to
// firewall direct access to the backend. It is not end-to-end TLS.
func RejectUnprotectedListen(address string, tlsEnabled, proxyTerminated bool) error {
	if tlsEnabled || proxyTerminated {
		return nil
	}
	host, _, err := net.SplitHostPort(address)
	if err != nil {
		return fmt.Errorf("invalid listen address %q: %w", address, err)
	}
	if loopbackDevHost(host) {
		return nil
	}
	return errors.New("non-loopback HTTP requires TLS certificates or tls_terminated_by_proxy")
}

func loopbackDevHost(host string) bool {
	if strings.EqualFold(host, "localhost") {
		return true
	}
	addr, err := netip.ParseAddr(host)
	return err == nil && addr.IsLoopback()
}
