package browserruntime

import (
	"net"
	"net/url"
	"strconv"
	"strings"
)

// lsof process/file fields retain socket addresses. A port-only PID lookup can
// confuse a listener on another interface with the configured loopback service.
func listenerFromFields(base string, data []byte) (int, error) {
	endpoint, err := url.Parse(base)
	if err != nil || !localEndpoint(base) {
		return 0, NewError(Unknown, "invalid local Browser endpoint")
	}
	port := endpoint.Port()
	if port == "" {
		port = "80"
	}
	wantPort, err := strconv.ParseUint(port, 10, 16)
	if err != nil || wantPort == 0 {
		return 0, NewError(Unknown, "invalid local Browser port")
	}
	host, _, _ := strings.Cut(endpoint.Hostname(), "%")
	wantIP := net.ParseIP(host)
	localhost := strings.EqualFold(host, "localhost")
	seen := map[int]bool{}
	pid := 0
	kind, name := "", ""
	invalid := false
	flush := func() {
		if kind == "" && name == "" {
			return
		}
		if pid <= 1 || (kind != "IPv4" && kind != "IPv6") || name == "" {
			invalid = true
			return
		}
		address, actualPort, splitErr := net.SplitHostPort(name)
		actualPortNumber, portErr := strconv.ParseUint(actualPort, 10, 16)
		if splitErr != nil || portErr != nil || actualPortNumber != wantPort {
			invalid = true
			return
		}
		address, _, _ = strings.Cut(address, "%")
		actualIP := net.ParseIP(address)
		wildcard := address == "*" || (actualIP != nil && actualIP.IsUnspecified())
		if !wildcard && actualIP == nil {
			invalid = true
			return
		}
		matches := false
		if localhost {
			matches = wildcard || actualIP.IsLoopback()
		} else if wildcard {
			// An IPv6 wildcard may also accept IPv4 through a dual-stack socket.
			// Without a socket v6-only receipt, treat it as a possible owner.
			matches = kind == "IPv6" || (wantIP != nil && wantIP.To4() != nil)
		} else {
			matches = wantIP != nil && actualIP.Equal(wantIP)
		}
		if matches {
			seen[pid] = true
		}
	}
	for _, line := range strings.Split(strings.TrimSpace(string(data)), "\n") {
		if line == "" {
			continue
		}
		switch line[0] {
		case 'p':
			flush()
			kind, name = "", ""
			pid, err = strconv.Atoi(line[1:])
			if err != nil || pid <= 1 {
				invalid = true
			}
		case 'f':
			flush()
			kind, name = "", ""
		case 't':
			kind = line[1:]
		case 'n':
			name = line[1:]
		default:
			invalid = true
		}
	}
	flush()
	if invalid || len(seen) > 1 {
		return 0, NewError(Unknown, "Browser endpoint has no unique verifiable listener process")
	}
	for pid := range seen {
		return pid, nil
	}
	return 0, NewError(Unavailable, "no listener serves the configured local Browser endpoint")
}
