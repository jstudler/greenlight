package celfuncs

import (
	"net/netip"
	"strings"
	"testing"
)

func TestHashIsStableAndNonNegative(t *testing.T) {
	first := Hash("ns/name")
	if first < 0 {
		t.Fatalf("Hash returned a negative value: %d", first)
	}
	if second := Hash("ns/name"); first != second {
		t.Fatalf("Hash is not deterministic: %d != %d", first, second)
	}
	if Hash("ns/name") == Hash("ns/other") {
		t.Fatal("different keys produced the same hash")
	}
}

func TestCIDRSubnet(t *testing.T) {
	tests := []struct {
		name    string
		parent  string
		length  int
		index   int64
		want    string
		wantErr string
	}{
		{name: "first ipv4 subnet", parent: "10.0.0.0/8", length: 25, index: 0, want: "10.0.0.0/25"},
		{name: "second ipv4 subnet", parent: "10.0.0.0/8", length: 25, index: 1, want: "10.0.0.128/25"},
		{name: "same length", parent: "10.1.2.0/24", length: 24, index: 0, want: "10.1.2.0/24"},
		{name: "whole ipv4 space", parent: "0.0.0.0/0", length: 1, index: 1, want: "128.0.0.0/1"},
		{name: "ipv4 /32", parent: "10.0.0.0/24", length: 32, index: 5, want: "10.0.0.5/32"},
		{name: "ipv4 /31", parent: "10.0.0.0/24", length: 31, index: 3, want: "10.0.0.6/31"},
		{name: "unmasked parent is masked first", parent: "10.0.0.37/24", length: 26, index: 2, want: "10.0.0.128/26"},
		{name: "ipv6", parent: "fd00::/48", length: 64, index: 2, want: "fd00:0:0:2::/64"},
		{name: "ipv6 /128", parent: "fd00::/120", length: 128, index: 9, want: "fd00::9/128"},
		{name: "ipv6 /127", parent: "fd00::/120", length: 127, index: 1, want: "fd00::2/127"},
		{name: "ipv6 huge offset", parent: "::/0", length: 16, index: 65535, want: "ffff::/16"},

		{name: "length shorter than parent", parent: "10.0.0.0/8", length: 4, wantErr: "shorter than the parent"},
		{name: "length out of range", parent: "10.0.0.0/8", length: 33, wantErr: "out of range"},
		{name: "index out of range", parent: "10.0.0.0/8", length: 9, index: 2, wantErr: "out of range"},
		{name: "negative index", parent: "10.0.0.0/8", length: 9, index: -1, wantErr: "must not be negative"},
		{name: "invalid parent", parent: "not-a-cidr", length: 24, wantErr: "not a valid CIDR prefix"},
		{name: "bare address", parent: "10.0.0.1", length: 24, wantErr: "not a valid CIDR prefix"},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got, err := CIDRSubnet(tc.parent, tc.length, tc.index)
			checkResult(t, got, err, tc.want, tc.wantErr)
		})
	}
}

func TestCIDRHost(t *testing.T) {
	tests := []struct {
		name    string
		prefix  string
		index   int64
		want    string
		wantErr string
	}{
		{name: "network address", prefix: "10.0.0.0/24", index: 0, want: "10.0.0.0"},
		{name: "first host", prefix: "10.0.0.0/24", index: 1, want: "10.0.0.1"},
		{name: "broadcast", prefix: "10.0.0.0/24", index: 255, want: "10.0.0.255"},
		{name: "single address", prefix: "10.0.0.7/32", index: 0, want: "10.0.0.7"},
		{name: "ipv6", prefix: "fd00::/64", index: 1, want: "fd00::1"},
		{name: "out of range", prefix: "10.0.0.0/24", index: 256, wantErr: "out of range"},
		{name: "negative", prefix: "10.0.0.0/24", index: -2, wantErr: "must not be negative"},
		{name: "invalid prefix", prefix: "10.0.0.0/33", wantErr: "not a valid CIDR prefix"},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got, err := CIDRHost(tc.prefix, tc.index)
			checkResult(t, got, err, tc.want, tc.wantErr)
		})
	}
}

func TestPickSubnetIsDeterministicAndInsideTheParent(t *testing.T) {
	const parent = "10.0.0.0/8"
	keys := []string{"ns/a", "ns/b", "other/a", "", strings.Repeat("x", 500)}

	parsed := netip.MustParsePrefix(parent)
	for _, key := range keys {
		got, err := PickSubnet(parent, 25, key)
		if err != nil {
			t.Fatalf("PickSubnet(%q): %v", key, err)
		}
		again, err := PickSubnet(parent, 25, key)
		if err != nil || again != got {
			t.Fatalf("PickSubnet(%q) is not deterministic: %q then %q (%v)", key, got, again, err)
		}
		subnet, err := netip.ParsePrefix(got)
		if err != nil {
			t.Fatalf("PickSubnet returned %q, which is not a CIDR: %v", got, err)
		}
		if subnet.Bits() != 25 {
			t.Fatalf("PickSubnet returned %q, want a /25", got)
		}
		if subnet.Masked() != subnet {
			t.Fatalf("PickSubnet returned the unmasked prefix %q", got)
		}
		if !parsed.Contains(subnet.Addr()) {
			t.Fatalf("PickSubnet returned %q, which is outside %q", got, parent)
		}
	}
}

func TestPickSubnetErrors(t *testing.T) {
	if _, err := PickSubnet("10.0.0.0/24", 8, "key"); err == nil {
		t.Fatal("expected an error for a prefix length shorter than the parent")
	}
	if _, err := PickSubnet("nope", 24, "key"); err == nil {
		t.Fatal("expected an error for an invalid parent")
	}
}

func TestPickHostNeverReturnsNetworkOrBroadcast(t *testing.T) {
	prefixes := []string{"10.0.0.0/24", "10.0.0.0/30", "192.0.2.0/25", "10.0.0.0/8"}
	keys := []string{"a", "b", "c", "d", "e", "f", "g", "h", "ns/name", "ns/other"}

	for _, prefix := range prefixes {
		parsed := netip.MustParsePrefix(prefix)
		network, err := CIDRHost(prefix, 0)
		if err != nil {
			t.Fatal(err)
		}
		size := int64(1) << (32 - parsed.Bits())
		broadcast, err := CIDRHost(prefix, size-1)
		if err != nil {
			t.Fatal(err)
		}
		for _, key := range keys {
			got, err := PickHost(prefix, key)
			if err != nil {
				t.Fatalf("PickHost(%q, %q): %v", prefix, key, err)
			}
			if got == network || got == broadcast {
				t.Fatalf("PickHost(%q, %q) returned the reserved address %q", prefix, key, got)
			}
			addr, err := netip.ParseAddr(got)
			if err != nil {
				t.Fatalf("PickHost returned %q, which is not an IP: %v", got, err)
			}
			if !parsed.Contains(addr) {
				t.Fatalf("PickHost(%q, %q) returned %q, which is outside the prefix", prefix, key, got)
			}
		}
	}
}

func TestPickHostSmallPrefixes(t *testing.T) {
	tests := []struct {
		prefix string
		want   []string
	}{
		{prefix: "10.0.0.0/31", want: []string{"10.0.0.0", "10.0.0.1"}},
		{prefix: "10.0.0.9/32", want: []string{"10.0.0.9"}},
		{prefix: "fd00::/127", want: []string{"fd00::", "fd00::1"}},
		{prefix: "fd00::5/128", want: []string{"fd00::5"}},
	}
	for _, tc := range tests {
		t.Run(tc.prefix, func(t *testing.T) {
			for _, key := range []string{"a", "b", "c", "d"} {
				got, err := PickHost(tc.prefix, key)
				if err != nil {
					t.Fatalf("PickHost(%q, %q): %v", tc.prefix, key, err)
				}
				if !contains(tc.want, got) {
					t.Fatalf("PickHost(%q, %q) = %q, want one of %v", tc.prefix, key, got, tc.want)
				}
			}
		})
	}
}

func TestPickHostIPv6SkipsTheAnycastAddress(t *testing.T) {
	for _, key := range []string{"a", "b", "c", "d", "e", "f"} {
		got, err := PickHost("fd00::/120", key)
		if err != nil {
			t.Fatal(err)
		}
		if got == "fd00::" {
			t.Fatalf("PickHost returned the subnet-router anycast address for key %q", key)
		}
	}
}

func TestHostCIDR(t *testing.T) {
	tests := []struct {
		name    string
		in      string
		want    string
		wantErr string
	}{
		{name: "ipv4", in: "10.0.0.1", want: "10.0.0.1/32"},
		{name: "ipv6", in: "fd00::1", want: "fd00::1/128"},
		{name: "already a host route", in: "10.0.0.1/32", want: "10.0.0.1/32"},
		{name: "ipv4 in ipv6", in: "::ffff:10.0.0.1", want: "10.0.0.1/32"},
		{name: "invalid", in: "nope", wantErr: "not a valid IP address"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got, err := HostCIDR(tc.in)
			checkResult(t, got, err, tc.want, tc.wantErr)
		})
	}
}

func checkResult(t *testing.T, got string, err error, want, wantErr string) {
	t.Helper()
	if wantErr != "" {
		if err == nil {
			t.Fatalf("expected an error containing %q, got the result %q", wantErr, got)
		}
		if !strings.Contains(err.Error(), wantErr) {
			t.Fatalf("expected an error containing %q, got %q", wantErr, err)
		}
		return
	}
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if got != want {
		t.Fatalf("got %q, want %q", got, want)
	}
}

func contains(list []string, value string) bool {
	for _, item := range list {
		if item == value {
			return true
		}
	}
	return false
}
