// Copyright 2009 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

// +build darwin dragonfly freebsd linux nacl netbsd openbsd solaris windows

package net

import (
	"context"
	"runtime"
	"syscall"
)

func probeIPv4Stack() bool {
	s, err := socketFunc(syscall.AF_INET, syscall.SOCK_STREAM, syscall.IPPROTO_TCP)
	switch err {
	case syscall.EAFNOSUPPORT, syscall.EPROTONOSUPPORT:
		return false
	case nil:
		closeFunc(s)
	}
	return true
}

// Should we try to use the IPv4 socket interface if we're
// only dealing with IPv4 sockets?  As long as the host system
// understands IPv6, it's okay to pass IPv4 addresses to the IPv6
// interface. That simplifies our code and is most general.
// Unfortunately, we need to run on kernels built without IPv6
// support too. So probe the kernel to figure it out.
//
// probeIPv6Stack probes both basic IPv6 capability and IPv6 IPv4-
// mapping capability which is controlled by IPV6_V6ONLY socket
// option and/or kernel state "net.inet6.ip6.v6only".
// It returns two boolean values. If the first boolean value is
// true, kernel supports basic IPv6 functionality. If the second
// boolean value is true, kernel supports IPv6 IPv4-mapping.
func probeIPv6Stack() (supportsIPv6, supportsIPv4map bool) {
	var probes = []struct {
		laddr TCPAddr
		value int
	}{
		// IPv6 communication capability
		{laddr: TCPAddr{IP: ParseIP("::1")}, value: 1},
		// IPv4-mapped IPv6 address communication capability
		{laddr: TCPAddr{IP: IPv4(127, 0, 0, 1)}, value: 0},
	}
	var supps [2]bool
	switch runtime.GOOS {
	case "dragonfly", "openbsd":
		// Some released versions of DragonFly BSD pretend to
		// accept IPV6_V6ONLY=0 successfully, but the state
		// still stays IPV6_V6ONLY=1. Eventually DragonFly BSD
		// stops pretending, but the transition period would
		// cause unpredictable behavior and we need to avoid
		// it.
		//
		// OpenBSD also doesn't support IPV6_V6ONLY=0 but it
		// never pretends to accept IPV6_V6OLY=0. It always
		// returns an error and we don't need to probe the
		// capability.
		probes = probes[:1]
	}

	for i := range probes {
		s, err := socketFunc(syscall.AF_INET6, syscall.SOCK_STREAM, syscall.IPPROTO_TCP)
		if err != nil {
			continue
		}
		defer closeFunc(s)
		syscall.SetsockoptInt(s, syscall.IPPROTO_IPV6, syscall.IPV6_V6ONLY, probes[i].value)
		sa, err := probes[i].laddr.sockaddr(syscall.AF_INET6)
		if err != nil {
			continue
		}
		if err := syscall.Bind(s, sa); err != nil {
			continue
		}
		supps[i] = true
	}

	return supps[0], supps[1]
}

// favoriteAddrFamily returns the appropriate address family to
// the given net, laddr, raddr and mode. At first it figures
// address family out from the net. If mode indicates "listen"
// and laddr is a wildcard, it assumes that the user wants to
// make a passive connection with a wildcard address family, both
// AF_INET and AF_INET6, and a wildcard address like following:
//
//	1. A wild-wild listen, "tcp" + ""
//	If the platform supports both IPv6 and IPv6 IPv4-mapping
//	capabilities, or does not support IPv4, we assume that
//	the user wants to listen on both IPv4 and IPv6 wildcard
//	addresses over an AF_INET6 socket with IPV6_V6ONLY=0.
//	Otherwise we prefer an IPv4 wildcard address listen over
//	an AF_INET socket.
//
//	2. A wild-ipv4wild listen, "tcp" + "0.0.0.0"
//	Same as 1.
//
//	3. A wild-ipv6wild listen, "tcp" + "[::]"
//	Almost same as 1 but we prefer an IPv6 wildcard address
//	listen over an AF_INET6 socket with IPV6_V6ONLY=0 when
//	the platform supports IPv6 capability but not IPv6 IPv4-
//	mapping capability.
//
//	4. A ipv4-ipv4wild listen, "tcp4" + "" or "0.0.0.0"
//	We use an IPv4 (AF_INET) wildcard address listen.
//
//	5. A ipv6-ipv6wild listen, "tcp6" + "" or "[::]"
//	We use an IPv6 (AF_INET6, IPV6_V6ONLY=1) wildcard address
//	listen.
//
// Otherwise guess: if the addresses are IPv4 then returns AF_INET,
// or else returns AF_INET6.  It also returns a boolean value what
// designates IPV6_V6ONLY option.
//
// Note that OpenBSD allows neither "net.inet6.ip6.v6only=1" change
// nor IPPROTO_IPV6 level IPV6_V6ONLY socket option setting.
func favoriteAddrFamily(net string, laddr, raddr sockaddr, mode string) (family int, ipv6only bool) {
	switch net[len(net)-1] {
	case '4':
		return syscall.AF_INET, false
	case '6':
		return syscall.AF_INET6, true
	}

	if mode == "listen" && (laddr == nil || laddr.isWildcard()) {
		if supportsIPv4map || !supportsIPv4 {
			return syscall.AF_INET6, false
		}
		if laddr == nil {
			return syscall.AF_INET, false
		}
		return laddr.family(), false
	}

	if (laddr == nil || laddr.family() == syscall.AF_INET) &&
		(raddr == nil || raddr.family() == syscall.AF_INET) {
		return syscall.AF_INET, false
	}
	return syscall.AF_INET6, false
}

// Internet sockets (TCP, UDP, IP)
func internetSocket(ctx context.Context, net string, laddr, raddr sockaddr, sotype, proto int, mode string) (fd *netFD, err error) {
	if (runtime.GOOS == "windows" || runtime.GOOS == "openbsd" || runtime.GOOS == "nacl") && mode == "dial" && raddr.isWildcard() {
		raddr = raddr.toLocal(net)
	}
	family, ipv6only := favoriteAddrFamily(net, laddr, raddr, mode)
	return socket(ctx, net, family, sotype, proto, ipv6only, laddr, raddr)
}

func ipToSockaddr(family int, ip IP, port int, zone string) (syscall.Sockaddr, error) {
	switch family {
	case syscall.AF_INET:
		if len(ip) == 0 {
			ip = IPv4zero
		}
		ip4 := ip.To4()
		if ip4 == nil {
			return nil, &AddrError{Err: "non-IPv4 address", Addr: ip.String()}
		}
		sa := &syscall.SockaddrInet4{Port: port}
		copy(sa.Addr[:], ip4)
		return sa, nil
	case syscall.AF_INET6:
		// In general, an IP wildcard address, which is either
		// "0.0.0.0" or "::", means the entire IP addressing
		// space. For some historical reason, it is used to
		// specify "any available address" on some operations
		// of IP node.
		//
		// When the IP node supports IPv4-mapped IPv6 address,
		// we allow an listener to listen to the wildcard
		// address of both IP addressing spaces by specifying
		// IPv6 wildcard address.
		if len(ip) == 0 || ip.Equal(IPv4zero) {
			ip = IPv6zero
		}
		// We accept any IPv6 address including IPv4-mapped
		// IPv6 address.
		ip6 := ip.To16()
		if ip6 == nil {
			return nil, &AddrError{Err: "non-IPv6 address", Addr: ip.String()}
		}
		sa := &syscall.SockaddrInet6{Port: port, ZoneId: uint32(zoneToInt(zone))}
		copy(sa.Addr[:], ip6)
		return sa, nil
	}
	return nil, &AddrError{Err: "invalid address family", Addr: ip.String()}
}
