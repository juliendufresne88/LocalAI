package openai

import (
	"fmt"
	"net"
	"strings"

	"github.com/mudler/LocalAI/core/config"
	"github.com/mudler/xlog"
	"github.com/pion/ice/v4"
	"github.com/pion/webrtc/v4"
)

// webRTCSettingEngine builds the pion SettingEngine for /v1/realtime WebRTC.
//
// With a default (empty) SettingEngine, pion gathers a host ICE candidate for
// every local interface except loopback. Under Docker host networking that
// includes bridge addresses (docker0/veth, 172.x) that a remote browser cannot
// route to; the connection often establishes on a good pair and then drops
// once ICE consent checks fail on the unreachable ones. The opt-in knobs below
// let an operator advertise only the reachable address.
//
// Loopback is a separate switch from the interface allow-list. Pion discards
// FlagLoopback addresses before the allow-list runs, so naming lo0 or lo and
// nothing else gathers zero candidates, the SDP answer still returns 201, and
// the browser then closes the transport. That is the same-machine path with
// the radio off: the page is on 127.0.0.1 and the LAN address is gone. Include
// loopback only when the operator named a loopback interface. An empty
// allow-list and a LAN-only list stay on pion's default.
func webRTCSettingEngine(cfg *config.ApplicationConfig) (webrtc.SettingEngine, error) {
	s := webrtc.SettingEngine{}
	if cfg == nil {
		return s, nil
	}
	includeLoopback := iceAllowListIncludesLoopback(cfg.WebRTCICEInterfaces)
	if includeLoopback {
		s.SetIncludeLoopbackCandidate(true)
		xlog.Info("realtime webrtc: including loopback ICE candidates", "interfaces", cfg.WebRTCICEInterfaces)
	}
	if len(cfg.WebRTCNAT1To1IPs) > 0 {
		s.SetNAT1To1IPs(cfg.WebRTCNAT1To1IPs, webrtc.ICECandidateTypeHost)
		xlog.Debug("realtime webrtc: advertising NAT 1:1 host IPs", "ips", cfg.WebRTCNAT1To1IPs)
	}
	if filter := iceInterfaceFilter(cfg.WebRTCICEInterfaces); filter != nil {
		s.SetInterfaceFilter(filter)
		xlog.Debug("realtime webrtc: restricting ICE interfaces", "interfaces", cfg.WebRTCICEInterfaces)
	}
	if cfg.WebRTCUDPPort > 0 {
		mux, err := udpMuxOnPort(cfg.WebRTCUDPPort, cfg.WebRTCICEInterfaces, includeLoopback)
		if err != nil {
			return s, err
		}
		s.SetICEUDPMux(mux)
		xlog.Debug("realtime webrtc: sharing a fixed UDP port", "port", cfg.WebRTCUDPPort)
	}
	return s, nil
}

// udpMuxOnPort binds port on each interface the allow-list admits and returns a
// mux every session shares.
//
// One socket per interface address rather than one wildcard socket: for a mux
// bound to 0.0.0.0 pion derives the host candidates by enumerating interfaces
// itself, with no filter and loopback included, and its muxed gathering path
// never consults SetInterfaceFilter. A wildcard socket therefore silently
// discards WebRTCICEInterfaces and re-advertises exactly the docker0/veth
// addresses that setting exists to suppress.
func udpMuxOnPort(port int, interfaces []string, includeLoopback bool) (ice.UDPMux, error) {
	// UDP4 only, matching the socket family this replaces.
	opts := []ice.UDPMuxFromPortOption{ice.UDPMuxFromPortWithNetworks(ice.NetworkTypeUDP4)}
	if filter := iceInterfaceFilter(interfaces); filter != nil {
		opts = append(opts, ice.UDPMuxFromPortWithInterfaceFilter(filter))
	}
	// The mux enumerates interfaces on its own, with loopback off unless asked.
	// An allow-list of only lo0 would otherwise bind nothing and fail the call.
	if includeLoopback {
		opts = append(opts, ice.UDPMuxFromPortWithLoopback())
	}

	mux, err := ice.NewMultiUDPMuxFromPort(port, opts...)
	if err != nil {
		return nil, fmt.Errorf("bind WebRTC UDP port %d: %w", port, err)
	}
	// An empty mux binds nothing and would leave signaling working while no
	// candidate is ever advertised, so report the misconfiguration instead.
	if len(mux.GetListenAddresses()) == 0 {
		_ = mux.Close()
		if len(interfaces) > 0 {
			return nil, fmt.Errorf("bind WebRTC UDP port %d: no usable IPv4 address on interfaces %v", port, interfaces)
		}
		return nil, fmt.Errorf("bind WebRTC UDP port %d: no usable IPv4 address on this host", port)
	}
	return mux, nil
}

// iceInterfaceFilter returns an interface allow-list predicate for pion, or nil
// when no interfaces are configured (pion's default: gather from all).
func iceInterfaceFilter(allowed []string) func(string) bool {
	set := iceInterfaceSet(allowed)
	if len(set) == 0 {
		return nil
	}
	return func(iface string) bool {
		_, ok := set[iface]
		return ok
	}
}

// iceAllowListIncludesLoopback reports whether the operator named an interface
// that carries the loopback flag. The name is whatever the host uses (lo0 on
// macOS, lo on Linux); the flag, not the spelling, is what pion rejects.
func iceAllowListIncludesLoopback(allowed []string) bool {
	for name := range iceInterfaceSet(allowed) {
		iface, err := net.InterfaceByName(name)
		if err != nil {
			continue
		}
		if iface.Flags&net.FlagLoopback != 0 {
			return true
		}
	}
	return false
}

func iceInterfaceSet(allowed []string) map[string]struct{} {
	set := make(map[string]struct{}, len(allowed))
	for _, name := range allowed {
		name = strings.TrimSpace(name)
		if name == "" {
			continue
		}
		set[name] = struct{}{}
	}
	return set
}
