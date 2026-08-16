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
}

func Detect() (Candidate, error) {
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	return DetectContext(ctx)
}

func DetectContext(ctx context.Context) (Candidate, error) {
	return detectContext(ctx, routeCandidates, adapterCandidates)
}

func detect(routeSource, adapterSource func() ([]Candidate, error)) (Candidate, error) {
	return detectContext(context.Background(),
		func(context.Context) ([]Candidate, error) { return routeSource() },
		func(context.Context) ([]Candidate, error) { return adapterSource() },
	)
}

func detectContext(ctx context.Context, routeSource, adapterSource func(context.Context) ([]Candidate, error)) (Candidate, error) {
	routeInput, routeErr := routeSource(ctx)
	if routeErr == nil {
		if selected, selectErr := selectBest(routeInput, true, true); selectErr == nil {
			return selected, nil
		} else {
			routeErr = selectErr
		}
	}
	adapterInput, adapterErr := adapterSource(ctx)
	if adapterErr == nil {
		if selected, selectErr := selectBest(adapterInput, true, false); selectErr == nil {
			return selected, nil
		} else {
			adapterErr = selectErr
		}
	}
	return Candidate{}, fmt.Errorf("LAN detection failed (default route: %v; active physical adapters: %v)", routeErr, adapterErr)
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

func selectBest(input []Candidate, requireHardware, routeAware bool) (Candidate, error) {
	deduplicated := map[string]Candidate{}
	for _, candidate := range input {
		ip := net.ParseIP(candidate.Address)
		if !validPrivateIPv4(ip) || isVirtualName(candidate.Interface+" "+candidate.Description) || (requireHardware && !candidate.HardwareInterface) {
			continue
		}
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
		return Candidate{}, fmt.Errorf("no active physical RFC1918 adapter was found")
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

func validPrivateIPv4(ip net.IP) bool {
	ip = ip.To4()
	if ip == nil || ip.IsLoopback() || ip.IsUnspecified() || (ip[0] == 169 && ip[1] == 254) {
		return false
	}
	return ip[0] == 10 || (ip[0] == 172 && ip[1] >= 16 && ip[1] <= 31) || (ip[0] == 192 && ip[1] == 168)
}

func isVirtualName(value string) bool {
	lower := strings.ToLower(value)
	for _, marker := range []string{"vmware", "virtual", "virtualbox", "hyper-v", "vethernet", "vpn", "tap", "tunnel", "loopback", "wsl", "wireguard", "openvpn", "tailscale", "zerotier", "hamachi", "docker", "npcap", "bluetooth", "wan miniport"} {
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

const powerShellAdapterQuery = `$items=@()
$adapters=Get-NetAdapter -ErrorAction Stop | Where-Object {$_.Status -eq 'Up' -and $_.HardwareInterface}
foreach($adapter in $adapters){
 $ipif=Get-NetIPInterface -AddressFamily IPv4 -InterfaceIndex $adapter.InterfaceIndex -ErrorAction SilentlyContinue
 if(-not $ipif){continue}
 $addresses=Get-NetIPAddress -AddressFamily IPv4 -InterfaceIndex $adapter.InterfaceIndex -ErrorAction SilentlyContinue | Where-Object {$_.AddressState -eq 'Preferred'}
 foreach($address in $addresses){
  $items += [pscustomobject]@{address=$address.IPAddress;interface=$adapter.Name;description=$adapter.InterfaceDescription;index=[int]$adapter.InterfaceIndex;routeMetric=1000000;interfaceMetric=[int]$ipif.InterfaceMetric;hardwareInterface=[bool]$adapter.HardwareInterface;mediaType=[string]$adapter.MediaType;score=0}
 }
}
ConvertTo-Json -Compress -InputObject @($items)`
