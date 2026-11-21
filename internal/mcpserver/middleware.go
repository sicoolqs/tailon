package mcpserver

import (
	"log"
	"net"
	"net/http"
	"net/url"
	"runtime/debug"
	"strings"
)

// wrap applies request safety checks and panic isolation around the SDK
// transport handler. It never enables permissive CORS.
func (s *Server) wrap(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		defer func() {
			if rec := recover(); rec != nil {
				s.stats.Panics.Add(1)
				log.Printf("error: MCP HTTP handler panicked: %v\n%s", rec, debug.Stack())
				http.Error(w, "internal server error", http.StatusInternalServerError)
			}
		}()

		if !s.requestAllowed(r) {
			s.stats.HTTPRejected.Add(1)
			log.Printf("warn: rejected MCP request: host=%q origin=%q remote=%q", r.Host, r.Header.Get("Origin"), r.RemoteAddr)
			http.Error(w, "forbidden", http.StatusForbidden)
			return
		}

		limit := int64(s.cfg.Limits.MaxRequestBytes)
		if limit > 0 {
			if r.ContentLength > limit {
				s.stats.HTTPRejected.Add(1)
				http.Error(w, "request body too large", http.StatusRequestEntityTooLarge)
				return
			}
			if r.Body != nil {
				r.Body = http.MaxBytesReader(w, r.Body, limit)
			}
		}

		next.ServeHTTP(w, r)
	})
}

func (s *Server) requestAllowed(r *http.Request) bool {
	if strings.EqualFold(r.Header.Get("Sec-Fetch-Site"), "cross-site") && r.Header.Get("Origin") == "" {
		return false
	}

	host := requestHostname(r.Host)
	if len(s.cfg.AllowedHosts) > 0 {
		allowed := false
		for _, candidate := range s.cfg.AllowedHosts {
			candidate = strings.TrimSpace(candidate)
			if strings.EqualFold(candidate, r.Host) || strings.EqualFold(requestHostname(candidate), host) {
				allowed = true
				break
			}
		}
		if !allowed {
			return false
		}
	}

	origin := strings.TrimSpace(r.Header.Get("Origin"))
	if origin == "" {
		return true
	}
	if strings.EqualFold(origin, "null") {
		return false
	}
	u, err := url.Parse(origin)
	if err != nil || u.Scheme == "" || u.Host == "" {
		return false
	}
	if u.Scheme != "http" && u.Scheme != "https" {
		return false
	}
	if len(s.cfg.AllowedOrigins) > 0 {
		for _, allowed := range s.cfg.AllowedOrigins {
			if strings.EqualFold(strings.TrimSuffix(allowed, "/"), u.Scheme+"://"+u.Host) {
				return true
			}
		}
		return false
	}
	// No explicit origin list: only same-origin browser requests are allowed.
	return strings.EqualFold(requestHostname(u.Host), host)
}

func requestHostname(hostport string) string {
	hostport = strings.TrimSpace(hostport)
	if host, _, err := net.SplitHostPort(hostport); err == nil {
		return strings.Trim(host, "[]")
	}
	return strings.Trim(hostport, "[]")
}
