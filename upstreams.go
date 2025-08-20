package mulery

import (
	"context"
	"fmt"
	"net"
	"net/http"
	"strings"
	"time"
)

const dnsRefreshInterval = 3 * time.Minute

func (c *Config) HandleAll(resp http.ResponseWriter, _ *http.Request) {
	if c.RedirectURL == "" {
		resp.WriteHeader(http.StatusUnauthorized)
		return
	}

	resp.Header().Add("Location", c.RedirectURL)
	resp.WriteHeader(http.StatusFound)
}

func (c *Config) HandleOK(resp http.ResponseWriter, _ *http.Request) {
	http.Error(resp, "OK", http.StatusOK)
}

func (c *Config) ValidateUpstream(next http.Handler) http.Handler {
	return http.HandlerFunc(func(resp http.ResponseWriter, req *http.Request) {
		if c.allow.Contains(req.RemoteAddr) {
			next.ServeHTTP(resp, req)
		} else {
			c.HandleAll(resp, req)
		}
	})
}

// AllowedIPs determines who make can requests.
type AllowedIPs struct {
	askIP chan string
	allow chan bool
	input []string
	nets  []*net.IPNet
}

var _ = fmt.Stringer(&AllowedIPs{})

// String turns a list of allowedIPs into a printable masterpiece.
func (n *AllowedIPs) String() string {
	if n == nil || len(n.nets) < 1 {
		return "(none)"
	}

	output := ""

	for idx := range n.nets {
		if output != "" {
			output += ", "
		}

		if n.nets[idx] != nil {
			output += n.nets[idx].String() + " (input: " + n.input[idx] + ")"
		} else {
			output += n.input[idx] + " (ignored)"
		}
	}

	return output
}

// Contains returns true if an IP is allowed.
func (n *AllowedIPs) Contains(ip string) bool {
	n.askIP <- strings.Trim(ip[:strings.LastIndex(ip, ":")], "[]")
	return <-n.allow
}

// MakeIPs turns a list of CIDR strings, IPs or dns hostnames into a list of net.IPNet.
// This "allowed" list is later used to check incoming IPs from web requests.
// Starts a go routine that does dns lookups for hostnames in the upstreams list.
func MakeIPs(upstreams []string) *AllowedIPs {
	return MakeIPsContext(context.Background(), upstreams)
}

// MakeIPsContext is like MakeIPs, but takes a context.
func MakeIPsContext(ctx context.Context, upstreams []string) *AllowedIPs {
	allowed := &AllowedIPs{
		input: make([]string, len(upstreams)),
		nets:  make([]*net.IPNet, len(upstreams)),
	}
	allowed.parseAndLookup(ctx, upstreams)

	go allowed.Start(ctx)

	return allowed
}

// parseAndLookup turns a list of CIDR strings, IPs or dns hostnames into a list of net.IPNet.
// This "allowed" list is later used to check incoming IPs from web requests.
// Starts a go routine that does dns lookups for hostnames in the upstreams list.
func (n *AllowedIPs) parseAndLookup(ctx context.Context, upstreams []string) {
	for idx, ipAddr := range upstreams {
		n.input[idx] = ipAddr

		if !strings.Contains(ipAddr, "/") {
			if strings.Contains(ipAddr, ":") {
				ipAddr += "/128"
			} else {
				ipAddr += "/32"
			}
		}

		if _, ipnet, err := net.ParseCIDR(ipAddr); err == nil {
			n.nets[idx] = ipnet
			continue // it's an ip, no dns lookup needed.
		}

		iplist, err := net.DefaultResolver.LookupHost(ctx, n.input[idx])
		if err != nil || len(iplist) < 1 {
			continue // keep what we had if the lookup is empty.
		}

		// if err != nil, keep what we had, or "nothing" if it never recovers.
		if _, ipnet, err := net.ParseCIDR(iplist[0] + "/32"); err == nil {
			n.nets[idx] = ipnet // update what we had with new lookup.
		}
	}
}

func (n *AllowedIPs) Start(ctx context.Context) {
	if n.askIP != nil {
		panic("AllowedIPs already running!")
	}

	n.askIP = make(chan string)
	n.allow = make(chan bool)
	ticker := time.NewTicker(dnsRefreshInterval)

	defer func() {
		n.askIP = nil
		close(n.allow) // signal finished.
		ticker.Stop()
	}()

	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			n.parseAndLookup(ctx, n.input) // update input w/ input.
		case askIP, ok := <-n.askIP:
			if !ok {
				return
			}

			n.allow <- n.contains(askIP)
		}
	}
}

func (n *AllowedIPs) contains(askIP string) bool {
	for i := range n.nets {
		if n.nets[i] != nil && n.nets[i].Contains(net.ParseIP(askIP)) {
			return true
		}
	}

	return false
}

// Stop the running allow IP routine.
func (n *AllowedIPs) Stop() {
	close(n.askIP)
	<-n.allow
	n.allow = nil
}
