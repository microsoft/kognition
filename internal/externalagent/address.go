// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package externalagent

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/netip"
)

// DefaultResolver returns the process resolver.
func DefaultResolver() Resolver { return net.DefaultResolver }

// ErrForbiddenAddress marks an endpoint that resolves to an address a workflow
// must never reach (loopback, private, link-local, metadata, cluster ranges).
var ErrForbiddenAddress = errors.New("external agent endpoint resolves to a forbidden address")

// ErrUnresolvable marks an endpoint whose host did not resolve.
var ErrUnresolvable = errors.New("external agent endpoint host did not resolve")

// Resolver is the subset of *net.Resolver the policy needs.
type Resolver interface {
	LookupNetIP(ctx context.Context, network, host string) ([]netip.Addr, error)
}

var forbiddenPrefixes = mustPrefixes(
	"0.0.0.0/8",       // "this" network
	"10.0.0.0/8",      // RFC 1918, also the usual pod and service range
	"100.64.0.0/10",   // CGNAT, includes the Alibaba metadata address
	"127.0.0.0/8",     // loopback
	"169.254.0.0/16",  // link-local, includes cloud metadata 169.254.169.254
	"172.16.0.0/12",   // RFC 1918
	"192.0.0.0/24",    // IETF protocol assignments
	"192.0.2.0/24",    // TEST-NET-1
	"192.168.0.0/16",  // RFC 1918
	"198.18.0.0/15",   // benchmarking
	"198.51.100.0/24", // TEST-NET-2
	"203.0.113.0/24",  // TEST-NET-3
	"224.0.0.0/4",     // multicast
	"240.0.0.0/4",     // reserved and broadcast
	"::/128",          // unspecified
	"::1/128",         // loopback
	"100::/64",        // discard-only
	"2001::/32",       // Teredo
	"2001:db8::/32",   // documentation
	"fc00::/7",        // unique local
	"fe80::/10",       // link-local
	"ff00::/8",        // multicast
)

func mustPrefixes(values ...string) []netip.Prefix {
	out := make([]netip.Prefix, len(values))
	for i, value := range values {
		out[i] = netip.MustParsePrefix(value)
	}
	return out
}

// ClassifyAddress returns ErrForbiddenAddress (wrapped with the reason) when
// addr must not be contacted. IPv4-mapped IPv6 addresses are judged as IPv4, and
// the IPv4 embedded in NAT64 (64:ff9b::/96) and 6to4 (2002::/16) addresses is
// judged too, so a hostile AAAA record cannot smuggle a private IPv4 address.
func ClassifyAddress(addr netip.Addr) error {
	if !addr.IsValid() {
		return fmt.Errorf("%w: invalid address", ErrForbiddenAddress)
	}
	addr = addr.WithZone("").Unmap()
	if addr.Is6() {
		bytes := addr.As16()
		switch {
		case bytes[0] == 0x00 && bytes[1] == 0x64 && bytes[2] == 0xff && bytes[3] == 0x9b && allZero(bytes[4:12]):
			return classifyEmbedded(addr, netip.AddrFrom4([4]byte{bytes[12], bytes[13], bytes[14], bytes[15]}))
		case bytes[0] == 0x20 && bytes[1] == 0x02:
			return classifyEmbedded(addr, netip.AddrFrom4([4]byte{bytes[2], bytes[3], bytes[4], bytes[5]}))
		}
	}
	for _, prefix := range forbiddenPrefixes {
		if prefix.Contains(addr) {
			return fmt.Errorf("%w: %s is in %s", ErrForbiddenAddress, addr, prefix)
		}
	}
	return nil
}

func classifyEmbedded(outer, inner netip.Addr) error {
	if err := ClassifyAddress(inner); err != nil {
		return fmt.Errorf("%w: %s embeds %s", ErrForbiddenAddress, outer, inner)
	}
	return nil
}

func allZero(values []byte) bool {
	for _, value := range values {
		if value != 0 {
			return false
		}
	}
	return true
}

// ResolvePermitted resolves host and returns its addresses only when every one
// is permitted. A single forbidden answer rejects the host, so a mixed DNS
// response cannot be used to reach an internal address.
func ResolvePermitted(ctx context.Context, resolver Resolver, host string) ([]netip.Addr, error) {
	addrs, err := resolver.LookupNetIP(ctx, "ip", host)
	if err != nil {
		return nil, fmt.Errorf("%w: %v", ErrUnresolvable, err)
	}
	if len(addrs) == 0 {
		return nil, ErrUnresolvable
	}
	for _, addr := range addrs {
		if err := ClassifyAddress(addr); err != nil {
			return nil, err
		}
	}
	return addrs, nil
}
