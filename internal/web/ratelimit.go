package web

import (
	"net"
	"net/http"
	"net/netip"
	"strings"
	"sync"
	"time"
)

// Sign-in throttling: at most loginPerUser failures per (username, client)
// and loginPerIP failures per client within loginWindow. A success clears the
// (username, client) count.
//
// ponytail: in-memory and per-process, so counts reset on restart and are not
// shared between instances; fine for one small self-hosted process.
const (
	loginWindow  = 15 * time.Minute
	loginPerUser = 5
	loginPerIP   = 20
	limiterCap   = 10000 // tracked keys; bounds memory under a flood of distinct names
)

type limiter struct {
	mu   sync.Mutex
	hits map[string]*attempts
	now  func() time.Time
}

type attempts struct {
	start time.Time
	count int
}

func newLimiter() *limiter {
	return &limiter{hits: map[string]*attempts{}, now: time.Now}
}

func userKey(user, ip string) string { return "u|" + strings.ToLower(user) + "|" + ip }
func ipKey(ip string) string         { return "i|" + ip }

// retryAfter returns how long a client must wait before trying again, or 0.
func (l *limiter) retryAfter(user, ip string) time.Duration {
	l.mu.Lock()
	defer l.mu.Unlock()
	var wait time.Duration
	for key, limit := range map[string]int{userKey(user, ip): loginPerUser, ipKey(ip): loginPerIP} {
		if a := l.live(key); a != nil && a.count >= limit {
			wait = max(wait, a.start.Add(loginWindow).Sub(l.now()))
		}
	}
	return wait
}

func (l *limiter) fail(user, ip string) {
	l.mu.Lock()
	defer l.mu.Unlock()
	for _, key := range []string{userKey(user, ip), ipKey(ip)} {
		a := l.live(key)
		if a == nil {
			if len(l.hits) >= limiterCap {
				l.sweep()
				if len(l.hits) >= limiterCap {
					continue // full of live entries: the per-IP key still applies
				}
			}
			a = &attempts{start: l.now()}
			l.hits[key] = a
		}
		a.count++
	}
}

func (l *limiter) succeed(user, ip string) {
	l.mu.Lock()
	defer l.mu.Unlock()
	delete(l.hits, userKey(user, ip))
}

// live returns the key's attempts if its window has not expired.
func (l *limiter) live(key string) *attempts {
	a := l.hits[key]
	if a != nil && l.now().Sub(a.start) >= loginWindow {
		delete(l.hits, key)
		return nil
	}
	return a
}

func (l *limiter) sweep() {
	for key, a := range l.hits {
		if l.now().Sub(a.start) >= loginWindow {
			delete(l.hits, key)
		}
	}
}

// remoteAddr is the TCP peer's address.
func remoteAddr(r *http.Request) netip.Addr {
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		host = r.RemoteAddr
	}
	addr, _ := netip.ParseAddr(host)
	return addr.Unmap()
}

func (s *Server) trusted(addr netip.Addr) bool {
	for _, p := range s.cfg.TrustedProxies {
		if p.Contains(addr) {
			return true
		}
	}
	return false
}

// clientIP is the address of the browser. X-Forwarded-For is believed only
// when the request came through a trusted proxy, and then only as far back as
// the chain stays trusted: the right-most address that is not a trusted proxy
// is the client.
func (s *Server) clientIP(r *http.Request) string {
	addr := remoteAddr(r)
	if !s.trusted(addr) {
		return addr.String()
	}
	hops := strings.Split(strings.Join(r.Header.Values("X-Forwarded-For"), ","), ",")
	for i := len(hops) - 1; i >= 0; i-- {
		hop, err := netip.ParseAddr(strings.TrimSpace(hops[i]))
		if err != nil {
			break
		}
		addr = hop.Unmap()
		if !s.trusted(addr) {
			break
		}
	}
	return addr.String()
}

// ParseTrustedProxies reads a comma-separated list of addresses and CIDR
// ranges, e.g. "127.0.0.1,::1,10.0.0.0/8".
func ParseTrustedProxies(list string) ([]netip.Prefix, error) {
	var out []netip.Prefix
	for _, item := range strings.Split(list, ",") {
		item = strings.TrimSpace(item)
		if item == "" {
			continue
		}
		if p, err := netip.ParsePrefix(item); err == nil {
			out = append(out, p.Masked())
			continue
		}
		addr, err := netip.ParseAddr(item)
		if err != nil {
			return nil, err
		}
		out = append(out, netip.PrefixFrom(addr.Unmap(), addr.Unmap().BitLen()))
	}
	return out, nil
}
