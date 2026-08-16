package network

import (
	"context"
	"errors"
	"net"
	"strings"
	"testing"
)

func parse(address string) net.IP { return net.ParseIP(address) }

// detect drives the policy chain from fixtures. The system source is empty so a
// test never sees the real machine's adapters; detectAll covers that tier.
func detect(routeSource, adapterSource func() ([]Candidate, error)) (Candidate, error) {
	return detectAll(routeSource, adapterSource, func() ([]Candidate, error) { return nil, nil })
}

func detectAll(routeSource, adapterSource, systemSource func() ([]Candidate, error)) (Candidate, error) {
	lift := func(source func() ([]Candidate, error)) func(context.Context) ([]Candidate, error) {
		return func(context.Context) ([]Candidate, error) { return source() }
	}
	return detectContext(context.Background(), lift(routeSource), lift(adapterSource), lift(systemSource))
}

var strictPhysical = detectionPolicies[0]

func TestDetectFallsBackToDeterministicPhysicalAdapterWithoutDefaultRoute(t *testing.T) {
	routeSource := func() ([]Candidate, error) {
		return nil, errors.New("no default route")
	}
	adapterSource := func() ([]Candidate, error) {
		return []Candidate{
			{Address: "192.168.50.20", Interface: "Wi-Fi", Index: 12, InterfaceMetric: 25, HardwareInterface: true, MediaType: "802.11"},
			{Address: "192.168.50.10", Interface: "Ethernet", Index: 7, InterfaceMetric: 25, HardwareInterface: true, MediaType: "802.3"},
		}, nil
	}

	selected, err := detect(routeSource, adapterSource)
	if err != nil {
		t.Fatal(err)
	}
	if selected.Address != "192.168.50.10" || selected.Interface != "Ethernet" {
		t.Fatalf("selected %+v, want deterministic Ethernet fallback", selected)
	}
	if !selected.Private {
		t.Fatal("an RFC1918 address was not reported as private")
	}
}

// A LAN machine must keep preferring its private address even when a public one
// is also present, so the stricter passes still run first.
func TestDetectPrefersPrivateAddressOverPublicOnTheSameMachine(t *testing.T) {
	empty := func() ([]Candidate, error) { return nil, nil }
	adapterSource := func() ([]Candidate, error) {
		return []Candidate{
			{Address: "203.0.113.9", Interface: "Ethernet 2", HardwareInterface: true, MediaType: "802.3", Index: 3},
			{Address: "192.168.1.50", Interface: "Ethernet", HardwareInterface: true, MediaType: "802.3", Index: 7},
		}, nil
	}
	selected, err := detect(empty, adapterSource)
	if err != nil {
		t.Fatal(err)
	}
	if selected.Address != "192.168.1.50" {
		t.Fatalf("selected %+v, want the private address", selected)
	}
}

// A hosted server has one hypervisor NIC carrying a public address. Refusing it
// left Setup unable to install anywhere except a home LAN.
func TestDetectAcceptsPublicAddressOnHypervisorNIC(t *testing.T) {
	empty := func() ([]Candidate, error) { return nil, nil }
	adapterSource := func() ([]Candidate, error) {
		return []Candidate{
			{Address: "45.76.10.20", Interface: "Ethernet", Description: "Red Hat VirtIO Ethernet Adapter", HardwareInterface: true, MediaType: "802.3", Index: 4},
		}, nil
	}
	selected, err := detect(empty, adapterSource)
	if err != nil {
		t.Fatalf("a hosted server adapter was rejected: %v", err)
	}
	if selected.Address != "45.76.10.20" {
		t.Fatalf("selected %+v", selected)
	}
	if selected.Private {
		t.Fatal("a public address was reported as private")
	}
	if selected.Policy == "" {
		t.Fatal("the accepting policy was not recorded")
	}
}

// Hyper-V synthetic NICs report HardwareInterface as false, and inside a guest
// that adapter is the only connection there is.
func TestDetectAcceptsSyntheticNICWithoutHardwareFlag(t *testing.T) {
	empty := func() ([]Candidate, error) { return nil, nil }
	adapterSource := func() ([]Candidate, error) {
		return []Candidate{
			{Address: "10.20.30.40", Interface: "Ethernet", Description: "Microsoft Hyper-V Network Adapter", HardwareInterface: false, MediaType: "802.3", Index: 2},
		}, nil
	}
	selected, err := detect(empty, adapterSource)
	if err != nil {
		t.Fatalf("a synthetic guest adapter was rejected: %v", err)
	}
	if selected.Address != "10.20.30.40" {
		t.Fatalf("selected %+v", selected)
	}
}

// Tunnels, container bridges and host side switches are never the answer, on
// any machine, in any pass.
func TestDetectNeverAcceptsOverlayAdapters(t *testing.T) {
	empty := func() ([]Candidate, error) { return nil, nil }
	adapterSource := func() ([]Candidate, error) {
		return []Candidate{
			{Address: "192.168.1.20", Interface: "vEthernet (WSL)", HardwareInterface: true},
			{Address: "10.8.0.2", Interface: "OpenVPN TAP-Windows6", HardwareInterface: true},
			{Address: "172.17.0.1", Interface: "Docker bridge", HardwareInterface: true},
			{Address: "169.254.10.10", Interface: "Ethernet", HardwareInterface: true},
			{Address: "127.0.0.1", Interface: "Loopback Pseudo-Interface 1", HardwareInterface: false},
		}, nil
	}
	_, err := detect(empty, adapterSource)
	if err == nil {
		t.Fatal("an overlay or link-local adapter was accepted")
	}
	// The message has to name what the machine reported, otherwise somebody
	// looking at a working ipconfig has nothing to act on.
	if !strings.Contains(err.Error(), "192.168.1.20") {
		t.Fatalf("failure does not list what was found: %v", err)
	}
}

func TestDetectReportsWhenNoAddressExistsAtAll(t *testing.T) {
	empty := func() ([]Candidate, error) { return nil, nil }
	_, err := detect(empty, empty)
	if err == nil {
		t.Fatal("an empty machine was accepted")
	}
	if !strings.Contains(err.Error(), "no preferred IPv4 address") {
		t.Fatalf("unexpected message: %v", err)
	}
}

// Older Windows Server builds have no Get-NetAdapter or Get-NetRoute, so both
// PowerShell sources fail. The runtime still knows the addresses.
func TestDetectFallsBackToTheRuntimeWhenPowerShellSourcesFail(t *testing.T) {
	broken := func() ([]Candidate, error) { return nil, errors.New("cmdlet not recognised") }
	systemSource := func() ([]Candidate, error) {
		return []Candidate{
			{Address: "103.15.51.7", Interface: "Ethernet", Description: "Ethernet", HardwareInterface: true, Index: 5},
		}, nil
	}
	selected, err := detectAll(broken, broken, systemSource)
	if err != nil {
		t.Fatalf("runtime fallback did not run: %v", err)
	}
	if selected.Address != "103.15.51.7" {
		t.Fatalf("selected %+v", selected)
	}
}

// The real machine must always produce something, which is the whole point of
// the last tier.
func TestSystemCandidatesSeeTheRunningMachine(t *testing.T) {
	candidates, err := systemCandidates()
	if err != nil {
		t.Fatalf("reading the machine's interfaces failed: %v", err)
	}
	for _, candidate := range candidates {
		if net.ParseIP(candidate.Address).To4() == nil {
			t.Fatalf("non IPv4 address leaked in: %+v", candidate)
		}
	}
}

func TestManualOverrideValidatesTheAddress(t *testing.T) {
	for _, bad := range []string{"", "   ", "not-an-ip", "1.2.3", "::1", "127.0.0.1", "169.254.1.5", "224.0.0.1"} {
		if _, err := Manual(bad); err == nil {
			t.Fatalf("manual address %q was accepted", bad)
		}
	}
	selected, err := Manual("  203.0.113.7 ")
	if err != nil {
		t.Fatal(err)
	}
	if selected.Address != "203.0.113.7" {
		t.Fatalf("address was not normalised: %+v", selected)
	}
	if selected.Private {
		t.Fatal("a public address was reported as private")
	}
	if private, err := Manual("192.168.1.4"); err != nil || !private.Private {
		t.Fatalf("private address handling is wrong: %+v %v", private, err)
	}
}

func TestDefaultRouteMetricWinsBeforeMediaPreference(t *testing.T) {
	selected, err := selectBest([]Candidate{
		{Address: "192.168.1.10", Interface: "Ethernet", HardwareInterface: true, MediaType: "802.3", RouteMetric: 20, InterfaceMetric: 50},
		{Address: "192.168.1.20", Interface: "Wi-Fi", HardwareInterface: true, MediaType: "802.11", RouteMetric: 10, InterfaceMetric: 50},
	}, true, true, strictPhysical)
	if err != nil {
		t.Fatal(err)
	}
	if selected.Address != "192.168.1.20" {
		t.Fatalf("selected %+v, want the lower effective route metric", selected)
	}
}

func TestUsableIPv4RejectsUnroutableSpace(t *testing.T) {
	for _, address := range []string{"127.0.0.1", "0.0.0.0", "169.254.1.1", "224.0.0.1", "255.255.255.255", "240.0.0.1"} {
		if usableIPv4(parse(address)) {
			t.Fatalf("%s was accepted as a connectable address", address)
		}
	}
	for _, address := range []string{"10.0.0.5", "172.16.4.4", "192.168.1.1", "45.76.10.20", "8.8.8.8"} {
		if !usableIPv4(parse(address)) {
			t.Fatalf("%s was rejected", address)
		}
	}
}
