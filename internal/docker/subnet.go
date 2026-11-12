package docker

import (
	"encoding/binary"
	"fmt"
	"net"
	"strconv"
)

// SubnetPlan derives the way a host's containers use a fixed IPv4 subnet.
// Gateway is the subnet's first address (the bridge's), IPRange is the upper
// half (the dynamic range), and First and Last are the addresses that may be
// pinned, such as a service's fixed endpoint.
type SubnetPlan struct {
	Subnet  string // canonical CIDR, e.g. "172.30.0.0/24"
	Gateway string // the first address, e.g. "172.30.0.1"
	IPRange string // the upper half, e.g. "172.30.0.128/25"
	First   string // first fixed address, e.g. "172.30.0.2"
	Last    string // last fixed address, e.g. "172.30.0.127"
}

// PlanSubnet validates an IPv4 CIDR and splits it into a gateway, a fixed
// address range in the lower half, and a dynamic range in the upper half.
//
// It rejects a non-IPv4 network, a prefix longer than /28, a network address
// with host bits set, and an invalid CIDR. The errors are suitable for the
// OUTHAUL_NETWORK_SUBNET setting, which must be one of these.
func PlanSubnet(cidr string) (SubnetPlan, error) {
	ip, ipnet, err := net.ParseCIDR(cidr)
	if err != nil {
		return SubnetPlan{}, fmt.Errorf("must be an IPv4 network in CIDR form, got %q", cidr)
	}
	if ip.To4() == nil {
		return SubnetPlan{}, fmt.Errorf("want an IPv4 network such as 172.30.0.0/24, not IPv6")
	}
	ones, _ := ipnet.Mask.Size()
	prefix := ones
	if prefix > 28 {
		return SubnetPlan{}, fmt.Errorf("want a prefix of /28 or larger, not /%d", prefix)
	}
	ip4 := binary.BigEndian.Uint32(ip.To4())
	size := uint64(1) << (32 - prefix)
	half := size / 2
	if !ip.Equal(ipnet.IP) {
		return SubnetPlan{}, fmt.Errorf("host bits are set; did you mean %s?", ipnet.String())
	}
	plan := SubnetPlan{
		Subnet:  ipnet.String(),
		Gateway: makeAddr(ip4 + 1),
		First:   makeAddr(ip4 + 2),
		Last:    makeAddr(ip4 + uint32(half) - 1),
		IPRange: makeCIDR(ip4+uint32(half), prefix+1),
	}
	return plan, nil
}

// NetworkFor returns the spec for the named network. An empty subnet leaves
// Docker to choose the network's addressing; otherwise the subnet's canonical
// form supplies the gateway and the dynamic range.
func NetworkFor(name, subnet string) (NetworkSpec, error) {
	if subnet == "" {
		return NetworkSpec{Name: name}, nil
	}
	plan, err := PlanSubnet(subnet)
	if err != nil {
		return NetworkSpec{}, err
	}
	return NetworkSpec{
		Name:    name,
		Subnet:  plan.Subnet,
		Gateway: plan.Gateway,
		IPRange: plan.IPRange,
	}, nil
}

// Fixed reports whether ip is an IPv4 address in the subnet's fixed range,
// inclusive of the First and Last addresses.
func (p SubnetPlan) Fixed(ip string) bool {
	addr := net.ParseIP(ip)
	if addr == nil || addr.To4() == nil {
		return false
	}
	ip4 := binary.BigEndian.Uint32(addr.To4())
	first := net.ParseIP(p.First)
	last := net.ParseIP(p.Last)
	if first == nil || first.To4() == nil || last == nil || last.To4() == nil {
		return false
	}
	first4 := binary.BigEndian.Uint32(first.To4())
	last4 := binary.BigEndian.Uint32(last.To4())
	return ip4 >= first4 && ip4 <= last4
}

func makeAddr(u uint32) string {
	var b [4]byte
	binary.BigEndian.PutUint32(b[:], u)
	return net.IPv4(b[0], b[1], b[2], b[3]).String()
}

func makeCIDR(u uint32, prefix int) string {
	return net.IPv4(byte(u>>24), byte(u>>16), byte(u>>8), byte(u)).String() + "/" + strconv.Itoa(prefix)
}
