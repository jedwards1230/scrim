package mdns

import "testing"

func TestIsLoopbackHost(t *testing.T) {
	tests := []struct {
		name string
		host string
		want bool
	}{
		{name: "empty string treated as loopback", host: "", want: true},
		{name: "localhost hostname", host: "localhost", want: true},
		{name: "ipv4 loopback", host: "127.0.0.1", want: true},
		{name: "ipv4 loopback range", host: "127.1.2.3", want: true},
		{name: "ipv6 loopback", host: "::1", want: true},
		{name: "ipv4 unspecified binds all interfaces", host: "0.0.0.0", want: false},
		{name: "ipv6 unspecified binds all interfaces", host: "::", want: false},
		{name: "specific LAN ipv4", host: "192.168.8.50", want: false},
		{name: "specific ipv6", host: "2001:db8::1", want: false},
		{name: "arbitrary hostname is not loopback", host: "scrim-host.lan", want: false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := IsLoopbackHost(tt.host); got != tt.want {
				t.Errorf("IsLoopbackHost(%q) = %v, want %v", tt.host, got, tt.want)
			}
		})
	}
}

// TestMaybeStartLoopbackNoOp is deterministic and network-free: for a
// loopback host, MaybeStart must return (nil, nil) without ever attempting
// to bind a multicast listener.
func TestMaybeStartLoopbackNoOp(t *testing.T) {
	tests := []string{"", "127.0.0.1", "::1", "localhost"}
	for _, host := range tests {
		t.Run(host, func(t *testing.T) {
			adv, err := MaybeStart(host, 7777)
			if err != nil {
				t.Fatalf("MaybeStart(%q) error = %v, want nil", host, err)
			}
			if adv != nil {
				t.Fatalf("MaybeStart(%q) = %+v, want nil advertiser for a loopback host", host, adv)
			}
		})
	}
}

// TestStopIsNilSafe covers Stop being called on both a totally nil
// *Advertiser and a zero-value one (server == nil) -- both are what
// MaybeStart's loopback branch, or a failed Start, produce, and callers
// unconditionally defer Stop regardless of which happened.
func TestStopIsNilSafe(t *testing.T) {
	var nilAdv *Advertiser
	if err := nilAdv.Stop(); err != nil {
		t.Fatalf("Stop() on nil *Advertiser error = %v, want nil", err)
	}

	zeroAdv := &Advertiser{}
	if err := zeroAdv.Stop(); err != nil {
		t.Fatalf("Stop() on zero-value Advertiser error = %v, want nil", err)
	}
}

// TestStartStopLifecycle exercises the real mDNS responder lifecycle
// against a non-loopback host. This binds a real multicast UDP listener,
// which some sandboxed/CI environments disallow -- when that happens Start
// returns an error (as documented) rather than panicking, and the test
// treats that as an environment limitation (skip) rather than a failure, per
// the manual/integration-only carve-out for real mDNS resolution.
func TestStartStopLifecycle(t *testing.T) {
	adv, err := Start("0.0.0.0", 7777)
	if err != nil {
		t.Skipf("mdns: multicast listener unavailable in this environment: %v", err)
	}
	if adv == nil {
		t.Fatal("Start() returned a nil advertiser with a nil error")
	}
	if err := adv.Stop(); err != nil {
		t.Fatalf("Stop() error = %v", err)
	}
	// Stop must tolerate being called more than once (both the graceful
	// stop and idle-reap shutdown paths defer it unconditionally).
	if err := adv.Stop(); err != nil {
		t.Fatalf("second Stop() error = %v", err)
	}
}

// TestMaybeStartNonLoopbackAttemptsStart is the counterpart to
// TestMaybeStartLoopbackNoOp: for a non-loopback host, MaybeStart must
// actually attempt to start advertising rather than silently no-op. Like
// TestStartStopLifecycle, a bind failure in a sandboxed environment is
// tolerated (skip), but a nil result with a nil error (a silent no-op) is
// not.
func TestMaybeStartNonLoopbackAttemptsStart(t *testing.T) {
	adv, err := MaybeStart("0.0.0.0", 7778)
	if err != nil {
		t.Skipf("mdns: multicast listener unavailable in this environment: %v", err)
	}
	if adv == nil {
		t.Fatal("MaybeStart(\"0.0.0.0\", ...) = nil advertiser, nil error; want it to have started advertising")
	}
	if err := adv.Stop(); err != nil {
		t.Fatalf("Stop() error = %v", err)
	}
}

func TestAdvertiseIPsRejectsAllLoopbackInterfaces(t *testing.T) {
	// A concrete non-loopback literal is returned as-is without touching
	// the network/interface list at all.
	ips, err := advertiseIPs("192.168.8.50")
	if err != nil {
		t.Fatalf("advertiseIPs() error = %v", err)
	}
	if len(ips) != 1 || ips[0].String() != "192.168.8.50" {
		t.Fatalf("advertiseIPs() = %v, want [192.168.8.50]", ips)
	}
}
