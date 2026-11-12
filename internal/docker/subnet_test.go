package docker

import (
	"testing"
)

func TestPlanSubnet(t *testing.T) {
	cases := []struct {
		cidr  string
		want  SubnetPlan
		label string
	}{
		{
			cidr: "172.30.0.0/24",
			want: SubnetPlan{
				Subnet:  "172.30.0.0/24",
				Gateway: "172.30.0.1",
				IPRange: "172.30.0.128/25",
				First:   "172.30.0.2",
				Last:    "172.30.0.127",
			},
			label: "24-bit prefix",
		},
		{
			cidr: "172.30.0.0/28",
			want: SubnetPlan{
				Subnet:  "172.30.0.0/28",
				Gateway: "172.30.0.1",
				IPRange: "172.30.0.8/29",
				First:   "172.30.0.2",
				Last:    "172.30.0.7",
			},
			label: "28-bit prefix, the smallest allowed",
		},
		{
			cidr: "172.30.0.0/27",
			want: SubnetPlan{
				Subnet:  "172.30.0.0/27",
				Gateway: "172.30.0.1",
				IPRange: "172.30.0.16/28",
				First:   "172.30.0.2",
				Last:    "172.30.0.15",
			},
			label: "27-bit prefix",
		},
		{
			cidr: "172.30.0.0/23",
			want: SubnetPlan{
				Subnet:  "172.30.0.0/23",
				Gateway: "172.30.0.1",
				IPRange: "172.30.1.0/24",
				First:   "172.30.0.2",
				Last:    "172.30.0.255",
			},
			label: "23-bit prefix crosses an octet boundary",
		},
		{
			cidr: "172.30.0.0/16",
			want: SubnetPlan{
				Subnet:  "172.30.0.0/16",
				Gateway: "172.30.0.1",
				IPRange: "172.30.128.0/17",
				First:   "172.30.0.2",
				Last:    "172.30.127.255",
			},
			label: "16-bit prefix",
		},
	}
	for _, c := range cases {
		t.Run(c.label, func(t *testing.T) {
			got, err := PlanSubnet(c.cidr)
			if err != nil {
				t.Fatalf("PlanSubnet(%q) returned %v", c.cidr, err)
			}
			if got != c.want {
				t.Errorf("PlanSubnet(%q) = %+v, want %+v", c.cidr, got, c.want)
			}
		})
	}
}

func TestPlanSubnetRejects(t *testing.T) {
	cases := []struct {
		cidr  string
		want  string
		label string
	}{
		{
			cidr:  "fd00::/64",
			want:  "IPv4",
			label: "IPv6 network is rejected",
		},
		{
			cidr:  "172.30.0.0/29",
			want:  "/28",
			label: "a /29 is below the minimum",
		},
		{
			cidr:  "172.30.0.5/24",
			want:  "host bits",
			label: "host bits are rejected",
		},
		{
			cidr:  "172.30.0.0",
			want:  "",
			label: "a bare address is rejected",
		},
		{
			cidr:  "172.30.0.0/33",
			want:  "",
			label: "a /33 prefix is rejected",
		},
	}
	for _, c := range cases {
		t.Run(c.label, func(t *testing.T) {
			_, err := PlanSubnet(c.cidr)
			if err == nil {
				t.Errorf("PlanSubnet(%q) returned nil, want an error", c.cidr)
				return
			}
			if c.want != "" && !contains(err.Error(), c.want) {
				t.Errorf("PlanSubnet(%q) error = %q, want it to contain %q", c.cidr, err.Error(), c.want)
			}
		})
	}
}

func TestSubnetPlanFixed(t *testing.T) {
	plan, err := PlanSubnet("172.30.0.0/24")
	if err != nil {
		t.Fatalf("PlanSubnet: %v", err)
	}
	cases := []struct {
		ip    string
		want  bool
		label string
	}{
		{ip: "172.30.0.2", want: true, label: "the first fixed address"},
		{ip: "172.30.0.127", want: true, label: "the last fixed address"},
		{ip: "172.30.0.0", want: false, label: "the network address"},
		{ip: "172.30.0.1", want: false, label: "the gateway"},
		{ip: "172.30.0.128", want: false, label: "the first dynamic address"},
		{ip: "172.30.0.255", want: false, label: "the broadcast address"},
		{ip: "", want: false, label: "an empty address"},
		{ip: "fd00::2", want: false, label: "an IPv6 address"},
		{ip: "not-an-ip", want: false, label: "a malformed address"},
		{ip: "0.0.0.0", want: false, label: "the network address is not fixed"},
	}
	for _, c := range cases {
		t.Run(c.label, func(t *testing.T) {
			if got := plan.Fixed(c.ip); got != c.want {
				t.Errorf("SubnetPlan.Fixed(%q) = %v, want %v", c.ip, got, c.want)
			}
		})
	}
}

func TestPlanSubnetHostBitsError(t *testing.T) {
	cidr := "172.30.0.5/24"
	_, err := PlanSubnet(cidr)
	if err == nil {
		t.Fatalf("PlanSubnet(%q) returned nil, want an error", cidr)
	}
	msg := err.Error()
	if !contains(msg, "host bits") {
		t.Errorf("error = %q, want it to contain %q", msg, "host bits")
	}
	if !contains(msg, "172.30.0.0/24") {
		t.Errorf("error = %q, want it to contain %q", msg, "172.30.0.0/24")
	}
}

func TestPlanSubnetCanonicalizesCIDR(t *testing.T) {
	_, err := PlanSubnet("172.30.0.0/024")
	if err != nil {
		t.Fatalf("PlanSubnet(%q) returned %v", "172.30.0.0/024", err)
	}
	subnet, err := PlanSubnet("172.30.0.0/024")
	if err != nil {
		t.Fatalf("PlanSubnet(%q) returned %v", "172.30.0.0/024", err)
	}
	if subnet.Subnet != "172.30.0.0/24" {
		t.Errorf("PlanSubnet(%q).Subnet = %q, want %q", "172.30.0.0/024", subnet.Subnet, "172.30.0.0/24")
	}
}

func TestPlanSubnetIPVersionError(t *testing.T) {
	_, err := PlanSubnet("fd00::/64")
	if err == nil {
		t.Fatalf("PlanSubnet(%q) returned nil, want an error", "fd00::/64")
	}
	msg := err.Error()
	if !contains(msg, "IPv4") {
		t.Errorf("error = %q, want it to contain %q", msg, "IPv4")
	}
	if !contains(msg, "IPv6") {
		t.Errorf("error = %q, want it to contain %q", msg, "IPv6")
	}
}

func TestPlanSubnetPrefixError(t *testing.T) {
	_, err := PlanSubnet("172.30.0.0/29")
	if err == nil {
		t.Fatalf("PlanSubnet(%q) returned nil, want an error", "172.30.0.0/29")
	}
	msg := err.Error()
	if !contains(msg, "/28") {
		t.Errorf("error = %q, want it to contain %q", msg, "/28")
	}
	if !contains(msg, "28") {
		t.Errorf("error = %q, want it to contain %q", msg, "28")
	}
}

func contains(s, substr string) bool {
	return len(s) >= len(substr) && (s == substr || len(substr) == 0 || indexOf(s, substr) >= 0)
}

func indexOf(s, substr string) int {
	for i := 0; i+len(substr) <= len(s); i++ {
		if s[i:i+len(substr)] == substr {
			return i
		}
	}
	return -1
}
