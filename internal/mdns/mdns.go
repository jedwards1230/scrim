// Package mdns advertises scrim's daemon as "scrim.local" over multicast
// DNS, so it can be discovered from other machines on the LAN, and cleanly
// withdraws that advertisement on shutdown.
//
// Advertisement is gated on the daemon's bind host: it only makes sense
// (and is only started) when the daemon is bound beyond loopback -- there's
// nothing on the LAN to discover if the daemon only listens on 127.0.0.1.
package mdns

import (
	"fmt"
	"net"

	hashicorpmdns "github.com/hashicorp/mdns"
)

// ServiceHost is the mDNS hostname scrim advertises. CLI output that prints
// a URL uses this alongside the plain host:port form as a fallback, since
// mDNS resolution can be blocked on some networks.
const ServiceHost = "scrim.local"

const (
	serviceInstance = "scrim"
	serviceType     = "_http._tcp"
	serviceDomain   = "local."
	serviceInfo     = "scrim canvas daemon"
)

// Advertiser holds the running mDNS responder for one daemon lifetime. The
// zero value (a nil *Advertiser) is valid to Stop -- callers don't need to
// track whether Start/MaybeStart actually produced one before deferring
// cleanup.
type Advertiser struct {
	server *hashicorpmdns.Server
}

// IsLoopbackHost reports whether host refers only to this machine, in which
// case advertising scrim.local would be pointless (nothing on the LAN could
// resolve or reach it anyway).
//
// The rule: "127.0.0.1", "::1", "localhost", and the empty string are
// loopback. Every other literal IP is judged by net.IP.IsLoopback, which
// covers the rest of 127.0.0.0/8 but returns false for the unspecified
// addresses "0.0.0.0"/"::" -- those bind every interface, including
// LAN-reachable ones, so they count as non-loopback here even though they
// also happen to include loopback. Any other DNS name (neither "localhost"
// nor a literal IP) is treated as non-loopback: it's presumably being bound
// so something else on the network can reach it.
//
// scrim's own config never actually resolves --host to an empty string
// (config.Default's Host is always "127.0.0.1" unless overridden), so the
// empty-string case is a defensive default rather than one exercised in
// practice -- it's treated as loopback rather than as net.Listen's own
// "all interfaces" shorthand, so an ambiguous/unset host errs toward *not*
// advertising rather than surprising a caller with an unexpected LAN
// broadcast.
func IsLoopbackHost(host string) bool {
	switch host {
	case "", "localhost":
		return true
	}
	ip := net.ParseIP(host)
	if ip == nil {
		return false
	}
	return ip.IsLoopback()
}

// MaybeStart starts advertising scrim.local via mDNS if host is not a
// loopback address (see IsLoopbackHost), returning (nil, nil) when it is.
// The returned Advertiser's Stop is always safe to call, including on the
// nil returned for the loopback case, so callers can unconditionally defer
// cleanup regardless of which branch ran.
func MaybeStart(host string, port int) (*Advertiser, error) {
	if IsLoopbackHost(host) {
		return nil, nil
	}
	return Start(host, port)
}

// Start begins advertising the scrim daemon's HTTP service as "scrim.local"
// over mDNS on port, regardless of host (callers that want the loopback
// gate should use MaybeStart instead).
//
// mDNS advertisement is a discovery aid, not a functional requirement: if
// the local network stack can't bind an mDNS multicast listener (e.g. a
// sandboxed/CI environment with no multicast support), Start returns an
// error that callers should log and otherwise ignore rather than treat as
// fatal to the daemon.
func Start(host string, port int) (*Advertiser, error) {
	ips, err := advertiseIPs(host)
	if err != nil {
		return nil, fmt.Errorf("mdns: resolving addresses to advertise: %w", err)
	}

	svc, err := hashicorpmdns.NewMDNSService(
		serviceInstance, serviceType, serviceDomain,
		ServiceHost+".", port, ips, []string{serviceInfo},
	)
	if err != nil {
		return nil, fmt.Errorf("mdns: building service record: %w", err)
	}

	srv, err := hashicorpmdns.NewServer(&hashicorpmdns.Config{Zone: svc})
	if err != nil {
		return nil, fmt.Errorf("mdns: starting responder: %w", err)
	}
	return &Advertiser{server: srv}, nil
}

// Stop withdraws the mDNS advertisement. It is safe to call on a nil
// Advertiser or a nil *Advertiser.server (a no-op either way), and safe to
// call more than once.
func (a *Advertiser) Stop() error {
	if a == nil || a.server == nil {
		return nil
	}
	return a.server.Shutdown()
}

// advertiseIPs returns the IP addresses scrim should advertise itself at.
// A concrete bind address (e.g. a specific LAN IP) is advertised as-is; an
// unspecified bind address ("0.0.0.0", "::", or any other non-literal host)
// is resolved to every non-loopback address found on the machine, since the
// daemon is reachable on all of them.
func advertiseIPs(host string) ([]net.IP, error) {
	if ip := net.ParseIP(host); ip != nil && !ip.IsUnspecified() {
		return []net.IP{ip}, nil
	}
	return localNonLoopbackIPs()
}

func localNonLoopbackIPs() ([]net.IP, error) {
	addrs, err := net.InterfaceAddrs()
	if err != nil {
		return nil, fmt.Errorf("listing local interface addresses: %w", err)
	}
	var ips []net.IP
	for _, addr := range addrs {
		ipNet, ok := addr.(*net.IPNet)
		if !ok || ipNet.IP.IsLoopback() {
			continue
		}
		if ipNet.IP.To4() == nil && !ipNet.IP.IsGlobalUnicast() {
			continue // skip link-local/other non-routable IPv6 noise
		}
		ips = append(ips, ipNet.IP)
	}
	if len(ips) == 0 {
		return nil, fmt.Errorf("no non-loopback local IP addresses found to advertise")
	}
	return ips, nil
}
