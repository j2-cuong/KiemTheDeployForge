package network

import (
	"context"
	"encoding/json"
	"fmt"
	"net"
	"sort"
	"strings"
	"time"

	"kiemthedeployforge/internal/winprocess"
)

type Candidate struct {
	Address           string `json:"address"`
	Interface         string `json:"interface"`
	Description       string `json:"description"`
	Index             int    `json:"index"`
	RouteMetric       int    `json:"routeMetric"`
	InterfaceMetric   int    `json:"interfaceMetric"`
	HardwareInterface bool   `json:"hardwareInterface"`
	MediaType         string `json:"mediaType"`
	Score             int    `json:"score"`
	// Private reports whether the address is RFC1918. A public address is a
	// normal result on a hosted server and a surprising one on a home LAN, so
	// the caller shows it differently.
	Private bool `json:"private"`
	// Policy names the detection pass that accepted this address.
	Policy string `json:"policy"`
}

// policy describes how permissive one detection pass is.
//
// The passes run from strictest to loosest. A machine on a real LAN matches the
// first one, and nothing about its behaviour changes. A hosted server, where
// the only NIC is a hypervisor device carrying a public address, is caught by
// the later passes instead of failing outright.
type policy struct {
	Name            string
	Source          sourceKind
	RequirePrivate  bool
	RequireHardware bool
	// AllowGuestNIC accepts adapters named after a hypervisor. Inside a guest
	// those are the real network card, not an overlay to skip.
	AllowGuestNIC bool
	RouteAware    bool
}

type sourceKind int

const (
	sourceRoute sourceKind = iota
	sourceAdapter
	// sourceSystem reads the addresses straight from the Go runtime, which
	// asks Windows the same question ipconfig does. It needs no PowerShell, so
	// it still answers on builds where Get-NetAdapter and Get-NetRoute are
	// missing, and it cannot be broken by a localised console.
	sourceSystem
)

var detectionPolicies = []policy{
	{Name: "physical LAN adapter with a default route", Source: sourceRoute, RequirePrivate: true, RequireHardware: true, RouteAware: true},
	{Name: "active physical LAN adapter", Source: sourceAdapter, RequirePrivate: true, RequireHardware: true},
	{Name: "any adapter with a default route", Source: sourceRoute, AllowGuestNIC: true, RouteAware: true},
	{Name: "any active adapter", Source: sourceAdapter, AllowGuestNIC: true},
	{Name: "any address Windows reports as up", Source: sourceSystem, AllowGuestNIC: true},
}

// Manual turns an operator supplied address into a candidate.
//
// Detection cannot cover every deployment: on a NAT'd hosted server the address
// clients must use is not bound to any adapter, so it appears nowhere on the
// machine. The value is still validated to the same standard as a detected one,
// because a typo here silently produces a server nobody can reach.
func Manual(address string) (Candidate, error) {
	trimmed := strings.TrimSpace(address)
	if trimmed == "" {
		return Candidate{}, fmt.Errorf("IPv4 address is required")
	}
	ip := net.ParseIP(trimmed)
	if ip == nil || ip.To4() == nil {
		return Candidate{}, fmt.Errorf("%q is not an IPv4 address", trimmed)
	}
	if !usableIPv4(ip) {
		return Candidate{}, fmt.Errorf("%s cannot be reached by a client; loopback, 169.254.x, multicast and reserved ranges are not usable", trimmed)
	}
	normalized := ip.To4().String()
	return Candidate{
		Address: normalized, Interface: "manual", Description: "entered by the operator",
		Private: isPrivateIPv4(ip), Policy: "manual override",
	}, nil
}

func Detect() (Candidate, error) {
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	return DetectContext(ctx)
}

func DetectContext(ctx context.Context) (Candidate, error) {
	return detectContext(ctx, routeCandidates, adapterCandidates,
		func(context.Context) ([]Candidate, error) { return systemCandidates() })
}

func detectContext(ctx context.Context, routeSource, adapterSource, systemSource func(context.Context) ([]Candidate, error)) (Candidate, error) {
	// The PowerShell sources are queried once each and reused across policies.
	routeInput, routeErr := routeSource(ctx)
	adapterInput, adapterErr := adapterSource(ctx)
	systemInput, systemErr := systemSource(ctx)

	for _, current := range detectionPolicies {
		var input []Candidate
		var sourceErr error
		switch current.Source {
		case sourceRoute:
			input, sourceErr = routeInput, routeErr
		case sourceAdapter:
			input, sourceErr = adapterInput, adapterErr
		case sourceSystem:
			input, sourceErr = systemInput, systemErr
		}
		if sourceErr != nil {
			continue
		}
		selected, err := selectBest(input, current.RequireHardware, current.RouteAware, current)
		if err != nil {
			continue
		}
		selected.Policy = current.Name
		return selected, nil
	}
	return Candidate{}, detectionFailure(routeInput, routeErr, append(adapterInput, systemInput...), adapterErr)
}

// systemCandidates lists the IPv4 addresses Windows currently has bound, using
// the Go runtime instead of a console tool.
//
// This is the same information ipconfig prints, taken from the same OS calls.
// Scraping ipconfig text would have to survive translation: a Vietnamese
// Windows prints "Địa chỉ IPv4", not "IPv4 Address". Reading the API avoids
// that entirely, and works even where the Get-Net* cmdlets do not exist.
func systemCandidates() ([]Candidate, error) {
	interfaces, err := net.Interfaces()
	if err != nil {
		return nil, err
	}
	var candidates []Candidate
	for _, adapter := range interfaces {
		if adapter.Flags&net.FlagUp == 0 || adapter.Flags&net.FlagLoopback != 0 {
			continue
		}
		addresses, err := adapter.Addrs()
		if err != nil {
			continue
		}
		for _, address := range addresses {
			network, ok := address.(*net.IPNet)
			if !ok || network.IP.To4() == nil {
				continue
			}
			candidates = append(candidates, Candidate{
				Address: network.IP.To4().String(), Interface: adapter.Name, Description: adapter.Name,
				Index: adapter.Index,
				// No routing information is available here, so every entry is
				// ranked the same and the deterministic tie break decides.
				RouteMetric: 0, InterfaceMetric: 0,
				HardwareInterface: len(adapter.HardwareAddr) > 0,
			})
		}
	}
	return candidates, nil
}

// detectionFailure explains what the machine actually reported, because "no
// adapter was found" is useless to somebody looking at a working ipconfig.
func detectionFailure(routeInput []Candidate, routeErr error, adapterInput []Candidate, adapterErr error) error {
	if routeErr != nil && adapterErr != nil {
		return fmt.Errorf("LAN detection failed: could not query network adapters (default route: %v; adapters: %v)", routeErr, adapterErr)
	}
	seen := map[string]Candidate{}
	for _, candidate := range append(append([]Candidate{}, routeInput...), adapterInput...) {
		if _, exists := seen[candidate.Address]; !exists {
			seen[candidate.Address] = candidate
		}
	}
	if len(seen) == 0 {
		return fmt.Errorf("LAN detection failed: Windows reported no preferred IPv4 address on any adapter that is Up")
	}
	addresses := make([]string, 0, len(seen))
	for _, candidate := range seen {
		addresses = append(addresses, fmt.Sprintf("%s on %q", candidate.Address, candidate.Interface))
	}
	sort.Strings(addresses)
	return fmt.Errorf("LAN detection failed: every IPv4 address found belongs to a tunnel, loopback or link-local adapter (%s)",
		strings.Join(addresses, "; "))
}

func routeCandidates(ctx context.Context) ([]Candidate, error) {
	command, err := winprocess.PowerShellCommandContext(ctx, "-NoProfile", "-NonInteractive", "-Command", powerShellRouteQuery)
	if err != nil {
		return nil, err
	}
	winprocess.Hide(command)
	output, err := command.Output()
	if err != nil {
		return nil, err
	}
	var candidates []Candidate
	if err := json.Unmarshal(output, &candidates); err != nil {
		return nil, err
	}
	return candidates, nil
}

func adapterCandidates(ctx context.Context) ([]Candidate, error) {
	command, err := winprocess.PowerShellCommandContext(ctx, "-NoProfile", "-NonInteractive", "-Command", powerShellAdapterQuery)
	if err != nil {
		return nil, err
	}
	winprocess.Hide(command)
	output, err := command.Output()
	if err != nil {
		return nil, err
	}
	var candidates []Candidate
	if err := json.Unmarshal(output, &candidates); err != nil {
		return nil, err
	}
	return candidates, nil
}

func selectBest(input []Candidate, requireHardware, routeAware bool, current policy) (Candidate, error) {
	deduplicated := map[string]Candidate{}
	for _, candidate := range input {
		ip := net.ParseIP(candidate.Address)
		if !usableIPv4(ip) {
			continue
		}
		private := isPrivateIPv4(ip)
		if current.RequirePrivate && !private {
			continue
		}
		name := candidate.Interface + " " + candidate.Description
		if isOverlayName(name) || (!current.AllowGuestNIC && isGuestNICName(name)) {
			continue
		}
		if requireHardware && !candidate.HardwareInterface {
			continue
		}
		candidate.Private = private
		candidate.Score = candidate.InterfaceMetric
		if routeAware {
			candidate.Score += candidate.RouteMetric
		}
		if !candidate.HardwareInterface {
			candidate.Score += 1000
		}
		current, exists := deduplicated[candidate.Address]
		if !exists || candidate.Score < current.Score {
			deduplicated[candidate.Address] = candidate
		}
	}
	var physical []Candidate
	var fallback []Candidate
	for _, candidate := range deduplicated {
		fallback = append(fallback, candidate)
		if candidate.HardwareInterface {
			physical = append(physical, candidate)
		}
	}
	candidates := fallback
	if len(physical) > 0 {
		candidates = physical
	}
	if len(candidates) == 0 {
		return Candidate{}, fmt.Errorf("no %s was found", current.Name)
	}
	sort.Slice(candidates, func(i, j int) bool {
		if candidates[i].Score == candidates[j].Score {
			leftMedia := mediaPreference(candidates[i].MediaType, candidates[i].Interface)
			rightMedia := mediaPreference(candidates[j].MediaType, candidates[j].Interface)
			if leftMedia != rightMedia {
				return leftMedia < rightMedia
			}
			if candidates[i].Index == candidates[j].Index {
				if candidates[i].Interface == candidates[j].Interface {
					return candidates[i].Address < candidates[j].Address
				}
				return candidates[i].Interface < candidates[j].Interface
			}
			return candidates[i].Index < candidates[j].Index
		}
		return candidates[i].Score < candidates[j].Score
	})
	return candidates[0], nil
}

// usableIPv4 accepts any address a client could actually be told to connect
// to. Loopback, link-local and multicast are never that address.
func usableIPv4(ip net.IP) bool {
	ip = ip.To4()
	if ip == nil || ip.IsLoopback() || ip.IsUnspecified() || ip.IsMulticast() {
		return false
	}
	if ip[0] == 169 && ip[1] == 254 { // APIPA: the adapter has no real address
		return false
	}
	if ip[0] == 0 || ip[0] >= 240 { // "this network" and reserved space
		return false
	}
	return true
}

func isPrivateIPv4(ip net.IP) bool {
	ip = ip.To4()
	if ip == nil {
		return false
	}
	return ip[0] == 10 || (ip[0] == 172 && ip[1] >= 16 && ip[1] <= 31) || (ip[0] == 192 && ip[1] == 168)
}

// overlayMarkers name adapters that are never the machine's own network
// connection: tunnels, container bridges, host side virtual switches. These are
// rejected in every pass, on a LAN machine and on a hosted server alike.
var overlayMarkers = []string{
	"vethernet", "vpn", "tap", "tunnel", "loopback", "wsl", "wireguard",
	"openvpn", "tailscale", "zerotier", "hamachi", "docker", "npcap",
	"bluetooth", "wan miniport",
}

// guestNICMarkers name the paravirtualised network cards a hypervisor gives to
// a guest. On a workstation such an adapter belongs to a VM running on the
// machine and must be skipped. Inside a guest it is the only real network card
// there is, which is why the later passes accept it.
var guestNICMarkers = []string{"vmware", "virtualbox", "virtual", "hyper-v", "vmxnet", "virtio", "xen", "qemu"}

func isOverlayName(value string) bool {
	return containsAny(strings.ToLower(value), overlayMarkers)
}

func isGuestNICName(value string) bool {
	return containsAny(strings.ToLower(value), guestNICMarkers)
}

func containsAny(lower string, markers []string) bool {
	for _, marker := range markers {
		if strings.Contains(lower, marker) {
			return true
		}
	}
	return false
}

func mediaPreference(mediaType, name string) int {
	value := strings.ToLower(mediaType + " " + name)
	if strings.Contains(value, "802.3") || strings.Contains(value, "ethernet") {
		return 0
	}
	if strings.Contains(value, "802.11") || strings.Contains(value, "wi-fi") || strings.Contains(value, "wifi") || strings.Contains(value, "wireless") {
		return 10
	}
	return 20
}

const powerShellRouteQuery = `$routes=Get-NetRoute -AddressFamily IPv4 -DestinationPrefix '0.0.0.0/0' -ErrorAction Stop | Where-Object {$_.State -ne 'Invalid' -and $_.NextHop -ne '0.0.0.0'}
$items=@()
foreach($route in $routes){
 $ipif=Get-NetIPInterface -AddressFamily IPv4 -InterfaceIndex $route.InterfaceIndex -ErrorAction SilentlyContinue
 $adapter=Get-NetAdapter -InterfaceIndex $route.InterfaceIndex -ErrorAction SilentlyContinue
 if(-not $ipif -or -not $adapter -or $adapter.Status -ne 'Up'){continue}
 $addresses=Get-NetIPAddress -AddressFamily IPv4 -InterfaceIndex $route.InterfaceIndex -ErrorAction SilentlyContinue | Where-Object {$_.AddressState -eq 'Preferred'}
 foreach($address in $addresses){
  $items += [pscustomobject]@{address=$address.IPAddress;interface=$adapter.Name;description=$adapter.InterfaceDescription;index=[int]$route.InterfaceIndex;routeMetric=[int]$route.RouteMetric;interfaceMetric=[int]$ipif.InterfaceMetric;hardwareInterface=[bool]$adapter.HardwareInterface;mediaType=[string]$adapter.MediaType;score=0}
 }
}
ConvertTo-Json -Compress -InputObject @($items)`

// The adapter query no longer filters on HardwareInterface. Hyper-V synthetic
// NICs report it as false, and inside a guest that adapter is the machine's
// only connection. Go decides per policy instead, and still ranks real hardware
// first through the score.
const powerShellAdapterQuery = `$items=@()
$adapters=Get-NetAdapter -ErrorAction Stop | Where-Object {$_.Status -eq 'Up'}
foreach($adapter in $adapters){
 $ipif=Get-NetIPInterface -AddressFamily IPv4 -InterfaceIndex $adapter.InterfaceIndex -ErrorAction SilentlyContinue
 if(-not $ipif){continue}
 $addresses=Get-NetIPAddress -AddressFamily IPv4 -InterfaceIndex $adapter.InterfaceIndex -ErrorAction SilentlyContinue | Where-Object {$_.AddressState -eq 'Preferred'}
 foreach($address in $addresses){
  $items += [pscustomobject]@{address=$address.IPAddress;interface=$adapter.Name;description=$adapter.InterfaceDescription;index=[int]$adapter.InterfaceIndex;routeMetric=1000000;interfaceMetric=[int]$ipif.InterfaceMetric;hardwareInterface=[bool]$adapter.HardwareInterface;mediaType=[string]$adapter.MediaType;score=0}
 }
}
ConvertTo-Json -Compress -InputObject @($items)`
