package testutil

import (
	"bufio"
	"encoding/hex"
	"net"
	"os"
	"os/exec"
	"strings"
)

// HostGatewayIP returns the IP address that a buildkitd container can use to
// reach HTTP servers on the test host.
//
// On Linux this is the docker bridge gateway (typically 172.17.0.1), resolved
// by inspecting the docker0 interface, then falling back to parsing
// "ip route" output, then /proc/net/route, then the static default 172.17.0.1.
//
// On other platforms (macOS / Windows) none of the Linux-specific probes work
// and the function returns "host.docker.internal", which Docker Desktop injects
// into every container's /etc/hosts.
func HostGatewayIP() string {
	// Try the docker0 bridge interface directly (most reliable on Linux).
	if iface, err := net.InterfaceByName("docker0"); err == nil {
		if addrs, err := iface.Addrs(); err == nil {
			for _, addr := range addrs {
				if ip, _, err := net.ParseCIDR(addr.String()); err == nil && ip.To4() != nil {
					return ip.String()
				}
			}
		}
	}

	// Try "ip route" (Linux): look for the docker0 / 172.17 route.
	if ip := gatewayFromIPRoute(); ip != "" {
		return ip
	}

	// Try /proc/net/route (Linux kernel routing table).
	if ip := gatewayFromProcRoute(); ip != "" {
		return ip
	}

	// Fallback: Docker Desktop (macOS/Windows) injects this name inside
	// containers.  Also used as the final fallback on Linux where the bridge
	// is configured differently.
	return "host.docker.internal"
}

// gatewayFromIPRoute parses the output of "ip route" looking for a line whose
// output interface is docker0 or whose destination starts with 172.17, and
// returns the "src" IP from that line.  Returns "" on failure.
func gatewayFromIPRoute() string {
	out, err := exec.Command("ip", "route").Output()
	if err != nil {
		return ""
	}
	scanner := bufio.NewScanner(strings.NewReader(string(out)))
	for scanner.Scan() {
		line := scanner.Text()
		// e.g. "172.17.0.0/16 dev docker0 proto kernel scope link src 172.17.0.1"
		if strings.Contains(line, "docker0") || strings.HasPrefix(line, "172.17") {
			fields := strings.Fields(line)
			for i, f := range fields {
				if f == "src" && i+1 < len(fields) {
					if ip := net.ParseIP(fields[i+1]); ip != nil && ip.To4() != nil {
						return ip.String()
					}
				}
			}
			// No "src" field; the first field might be the network; skip.
		}
	}
	return ""
}

// gatewayFromProcRoute reads /proc/net/route and returns the gateway for the
// docker0 interface.  /proc/net/route lines have the format:
//
//	Iface  Destination  Gateway  Flags  RefCnt  Use  Metric  Mask  MTU  Window  IRTT
//
// All addresses are in hexadecimal little-endian (host byte order on LE arch).
func gatewayFromProcRoute() string {
	f, err := os.Open("/proc/net/route")
	if err != nil {
		return ""
	}
	defer f.Close()

	scanner := bufio.NewScanner(f)
	scanner.Scan() // skip header
	for scanner.Scan() {
		fields := strings.Fields(scanner.Text())
		if len(fields) < 3 {
			continue
		}
		iface := fields[0]
		if iface != "docker0" {
			continue
		}
		// Gateway is the third column, hex little-endian IPv4.
		gwHex := fields[2]
		b, err := hex.DecodeString(gwHex)
		if err != nil || len(b) != 4 {
			continue
		}
		ip := net.IP([]byte{b[3], b[2], b[1], b[0]}) // little-endian → big-endian
		// Skip the default route (0.0.0.0 gateway means directly connected).
		if ip.IsUnspecified() {
			// The destination 0.0.0.0 row has Gateway 0.0.0.0 too; look at Destination
			// instead.  For a directly connected route the Gateway is 0 — use the
			// destination network's host address by reading the Destination column.
			dstHex := fields[1]
			db, err := hex.DecodeString(dstHex)
			if err != nil || len(db) != 4 {
				continue
			}
			dst := net.IP([]byte{db[3], db[2], db[1], db[0]})
			if !dst.IsUnspecified() {
				// Return the first host in the network (e.g. 172.17.0.1).
				dst[3] = 1
				return dst.String()
			}
			continue
		}
		return ip.String()
	}
	return ""
}
