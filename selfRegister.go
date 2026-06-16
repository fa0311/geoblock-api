package geoblock

import (
	"net"
	"net/http"
	"net/url"
	"strings"
	"time"
)

// selfRegister matches incoming requests against a configured URL prefix
// (selfRegisterURL, e.g. "http://example.com/api/geoblock/") and, on a match,
// whitelists the requesting client's IP under the country code taken from the
// trailing path segment. It is evaluated inside ServeHTTP before the geo check,
// so a client currently blocked by country can still register itself.
//
// The endpoint performs no authentication of its own — it is a state-changing
// API and MUST be protected by an upstream Traefik middleware (basicAuth,
// ipAllowList, ...). It is disabled when selfRegisterURL is empty.
type selfRegister struct {
	host       string // as configured; compared case-insensitively, port-stripped
	pathPrefix string
}

// newSelfRegister parses the configured URL prefix into a matcher. A blank
// rawURL disables the feature (returns nil, nil).
func newSelfRegister(rawURL string) (*selfRegister, error) {
	if rawURL == "" {
		return nil, nil
	}

	u, err := url.Parse(rawURL)
	if err != nil {
		return nil, err
	}

	return &selfRegister{host: u.Hostname(), pathPrefix: u.Path}, nil
}

// match reports whether the request targets the self-register endpoint and, if
// so, returns the country code (the path remainder after the configured prefix).
// Host comparison ignores case and port; the path must start with the prefix.
func (s *selfRegister) match(req *http.Request) (string, bool) {
	host := req.Host
	if h, _, err := net.SplitHostPort(host); err == nil {
		host = h
	}
	if !strings.EqualFold(host, s.host) {
		return "", false
	}

	country, ok := strings.CutPrefix(req.URL.Path, s.pathPrefix)
	if !ok || country == "" {
		return "", false
	}

	return country, true
}

// handleSelfRegister adds the requesting client's IP address to the in-memory IP
// database under the given country code, so subsequent requests from that IP
// pass the geo check immediately (and are persisted when ipDatabaseCachePath is
// configured). The client IP is taken from the request via the same logic as the
// geo check (collectRemoteIP), so it stays consistent with what is enforced.
func (a *GeoBlock) handleSelfRegister(rw http.ResponseWriter, req *http.Request, country string) {
	ipAddresses, err := a.collectRemoteIP(req)
	if err != nil || len(ipAddresses) == 0 {
		a.infoLogger.Printf("%s: self-register could not determine client IP: %s", a.name, err)
		rw.WriteHeader(http.StatusBadRequest)
		return
	}

	ipAddress := ipAddresses[0]
	a.database.Add(ipAddress.String(), ipEntry{Country: country, Timestamp: time.Now()})
	a.ipDatabasePersistence.MarkDirty()
	a.infoLogger.Printf("%s: IP address [%s] self-registered with country [%s]", a.name, ipAddress, country)

	rw.WriteHeader(http.StatusOK)
}
