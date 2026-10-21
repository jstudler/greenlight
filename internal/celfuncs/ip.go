package celfuncs

import (
	"fmt"
	"hash/fnv"
	"math/big"
	"net/netip"
)

// Hash returns a deterministic, non-negative FNV-1a 64 hash of key.
func Hash(key string) int64 {
	h := fnv.New64a()
	_, _ = h.Write([]byte(key))
	return int64(h.Sum64() & 0x7fffffffffffffff)
}

// CIDRSubnet returns the index-th subnet of length prefixLength inside parent.
// The result is always masked. It behaves like Terraform's cidrsubnet, except
// that prefixLength is absolute rather than a number of additional bits.
func CIDRSubnet(parent string, prefixLength int, index int64) (string, error) {
	p, err := parsePrefix(parent)
	if err != nil {
		return "", err
	}
	if index < 0 {
		return "", fmt.Errorf("cidrSubnet: index %d must not be negative", index)
	}
	return cidrSubnetBig(p, prefixLength, big.NewInt(index))
}

// CIDRHost returns the index-th address of prefix as a bare IP (index 0 is the
// network address).
func CIDRHost(prefix string, index int64) (string, error) {
	p, err := parsePrefix(prefix)
	if err != nil {
		return "", err
	}
	if index < 0 {
		return "", fmt.Errorf("cidrHost: index %d must not be negative", index)
	}
	return cidrHostBig(p, big.NewInt(index))
}

// PickSubnet deterministically selects one of the subnets of length
// prefixLength inside parent, based on the hash of key.
func PickSubnet(parent string, prefixLength int, key string) (string, error) {
	p, err := parsePrefix(parent)
	if err != nil {
		return "", err
	}
	if err := checkSubnetLength(p, prefixLength); err != nil {
		return "", err
	}
	count := pow2(uint(prefixLength - p.Bits()))
	index := new(big.Int).Mod(big.NewInt(Hash(key)), count)
	return cidrSubnetBig(p, prefixLength, index)
}

// PickHost deterministically selects a usable host address inside prefix based
// on the hash of key. For IPv4 prefixes of /30 or shorter, the network and
// broadcast addresses are excluded; /31 and /32 use every address. For IPv6
// prefixes of /126 or shorter, the subnet-router anycast address (index 0) is
// excluded; /127 and /128 use every address.
func PickHost(prefix string, key string) (string, error) {
	p, err := parsePrefix(prefix)
	if err != nil {
		return "", err
	}
	start, count := usableRange(p)
	index := new(big.Int).Mod(big.NewInt(Hash(key)), count)
	index.Add(index, big.NewInt(start))
	return cidrHostBig(p, index)
}

// HostCIDR turns a bare IP into a host route: /32 for IPv4, /128 for IPv6.
func HostCIDR(addr string) (string, error) {
	a, err := netip.ParseAddr(addr)
	if err != nil {
		// Tolerate an address that already carries a mask, as long as it is a
		// host route or can be reduced to one.
		if p, perr := netip.ParsePrefix(addr); perr == nil {
			a = p.Addr()
		} else {
			return "", fmt.Errorf("hostCIDR: %q is not a valid IP address", addr)
		}
	}
	a = a.Unmap()
	return netip.PrefixFrom(a, a.BitLen()).String(), nil
}

func parsePrefix(s string) (netip.Prefix, error) {
	p, err := netip.ParsePrefix(s)
	if err != nil {
		return netip.Prefix{}, fmt.Errorf("%q is not a valid CIDR prefix", s)
	}
	if p.Addr().Is4In6() {
		p = netip.PrefixFrom(p.Addr().Unmap(), p.Bits()-96)
	}
	return p.Masked(), nil
}

func checkSubnetLength(p netip.Prefix, prefixLength int) error {
	bits := p.Addr().BitLen()
	if prefixLength < 0 || prefixLength > bits {
		return fmt.Errorf("prefix length /%d is out of range for %s (max /%d)", prefixLength, p, bits)
	}
	if prefixLength < p.Bits() {
		return fmt.Errorf("prefix length /%d is shorter than the parent prefix %s", prefixLength, p)
	}
	return nil
}

func cidrSubnetBig(p netip.Prefix, prefixLength int, index *big.Int) (string, error) {
	if err := checkSubnetLength(p, prefixLength); err != nil {
		return "", err
	}
	count := pow2(uint(prefixLength - p.Bits()))
	if index.Cmp(count) >= 0 {
		return "", fmt.Errorf("index %s is out of range: %s only contains %s subnets of length /%d",
			index, p, count, prefixLength)
	}
	offset := new(big.Int).Lsh(index, uint(p.Addr().BitLen()-prefixLength))
	addr, err := addrFromInt(new(big.Int).Add(addrToInt(p.Addr()), offset), p.Addr().Is4())
	if err != nil {
		return "", err
	}
	return netip.PrefixFrom(addr, prefixLength).String(), nil
}

func cidrHostBig(p netip.Prefix, index *big.Int) (string, error) {
	size := pow2(uint(p.Addr().BitLen() - p.Bits()))
	if index.Cmp(size) >= 0 {
		return "", fmt.Errorf("index %s is out of range: %s only contains %s addresses", index, p, size)
	}
	addr, err := addrFromInt(new(big.Int).Add(addrToInt(p.Addr()), index), p.Addr().Is4())
	if err != nil {
		return "", err
	}
	return addr.String(), nil
}

// usableRange returns the first usable host index and the number of usable
// host addresses in p.
func usableRange(p netip.Prefix) (start int64, count *big.Int) {
	size := pow2(uint(p.Addr().BitLen() - p.Bits()))
	switch {
	case p.Addr().Is4() && p.Bits() <= 30:
		// Exclude the network and the broadcast address.
		return 1, new(big.Int).Sub(size, big.NewInt(2))
	case !p.Addr().Is4() && p.Bits() <= 126:
		// Exclude the subnet-router anycast address.
		return 1, new(big.Int).Sub(size, big.NewInt(1))
	default:
		return 0, size
	}
}

func pow2(n uint) *big.Int {
	return new(big.Int).Lsh(big.NewInt(1), n)
}

func addrToInt(a netip.Addr) *big.Int {
	if a.Is4() {
		b := a.As4()
		return new(big.Int).SetBytes(b[:])
	}
	b := a.As16()
	return new(big.Int).SetBytes(b[:])
}

func addrFromInt(i *big.Int, is4 bool) (netip.Addr, error) {
	size := 16
	if is4 {
		size = 4
	}
	b := i.Bytes()
	if len(b) > size {
		return netip.Addr{}, fmt.Errorf("address %s overflows the address space", i)
	}
	buf := make([]byte, size)
	copy(buf[size-len(b):], b)
	addr, ok := netip.AddrFromSlice(buf)
	if !ok {
		return netip.Addr{}, fmt.Errorf("cannot build an IP address from %s", i)
	}
	return addr.Unmap(), nil
}
