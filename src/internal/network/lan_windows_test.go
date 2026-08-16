package network

import (
	"errors"
	"testing"
)

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
}

func TestDetectNeverFallsBackToVirtualOrPublicAdapter(t *testing.T) {
	empty := func() ([]Candidate, error) { return nil, nil }
	adapterSource := func() ([]Candidate, error) {
		return []Candidate{
			{Address: "192.168.1.20", Interface: "vEthernet (WSL)", HardwareInterface: true},
			{Address: "8.8.8.8", Interface: "Ethernet", HardwareInterface: true},
		}, nil
	}

	if _, err := detect(empty, adapterSource); err == nil {
		t.Fatal("virtual/public adapters were accepted")
	}
}

func TestDefaultRouteMetricWinsBeforeMediaPreference(t *testing.T) {
	selected, err := selectBest([]Candidate{
		{Address: "192.168.1.10", Interface: "Ethernet", HardwareInterface: true, MediaType: "802.3", RouteMetric: 20, InterfaceMetric: 50},
		{Address: "192.168.1.20", Interface: "Wi-Fi", HardwareInterface: true, MediaType: "802.11", RouteMetric: 10, InterfaceMetric: 50},
	}, true, true)
	if err != nil {
		t.Fatal(err)
	}
	if selected.Address != "192.168.1.20" {
		t.Fatalf("selected %+v, want the lower effective route metric", selected)
	}
}
