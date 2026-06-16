package geoblock

import (
	"encoding/json"
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

// hostMatches reports whether the request targets the configured host, ignoring
// case and an optional port.
func (s *selfRegister) hostMatches(req *http.Request) bool {
	host := req.Host
	if h, _, err := net.SplitHostPort(host); err == nil {
		host = h
	}
	return strings.EqualFold(host, s.host)
}

// match reports whether the request is a register call (POST {prefix}{country})
// and, if so, returns the country code (the path remainder after the prefix).
func (s *selfRegister) match(req *http.Request) (string, bool) {
	if req.Method != http.MethodPost || !s.hostMatches(req) {
		return "", false
	}

	country, ok := strings.CutPrefix(req.URL.Path, s.pathPrefix)
	if !ok || country == "" {
		return "", false
	}

	return country, true
}

// queryRequested reports whether the request is a self-query call: GET on the
// bare prefix (no country segment).
func (s *selfRegister) queryRequested(req *http.Request) bool {
	return req.Method == http.MethodGet && s.hostMatches(req) && req.URL.Path == s.pathPrefix
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
	entry := ipEntry{Country: country, Timestamp: time.Now()}
	a.database.Add(ipAddress.String(), entry)
	a.ipDatabasePersistence.MarkDirty()
	a.infoLogger.Printf("%s: IP address [%s] self-registered with country [%s]", a.name, ipAddress, country)

	a.writeEntryJSON(rw, ipAddress.String(), entry)
}

// handleSelfQuery writes the caller's own cached entry as JSON: its country and
// ttlSeconds (countdown to the monthly refresh, only enforced when
// forceMonthlyUpdate is set, reported as ttlEnforced). It returns 404 when the
// caller's IP is not cached. The client IP is taken via collectRemoteIP, the same
// logic the geo check enforces, so a caller only ever sees their own entry.
func (a *GeoBlock) handleSelfQuery(rw http.ResponseWriter, req *http.Request) {
	ipAddresses, err := a.collectRemoteIP(req)
	if err != nil || len(ipAddresses) == 0 {
		a.infoLogger.Printf("%s: self-query could not determine client IP: %s", a.name, err)
		rw.WriteHeader(http.StatusBadRequest)
		return
	}

	ipAddress := ipAddresses[0]
	cached, ok := a.database.Get(ipAddress.String())
	entry, typeOK := cached.(ipEntry)
	if !ok || !typeOK {
		rw.WriteHeader(http.StatusNotFound)
		return
	}

	a.writeEntryJSON(rw, ipAddress.String(), entry)
}

// writeEntryJSON writes a single cache entry as JSON: its country and ttlSeconds
// (countdown to the monthly refresh, only enforced when forceMonthlyUpdate is
// set, reported as ttlEnforced). Shared by the register (POST) and query (GET)
// responses.
func (a *GeoBlock) writeEntryJSON(rw http.ResponseWriter, ip string, entry ipEntry) {
	ttl := a.cacheTTL
	rw.Header().Set("Content-Type", "application/json")
	if err := json.NewEncoder(rw).Encode(struct {
		IP          string `json:"ip"`
		Country     string `json:"country"`
		TTLSeconds  int64  `json:"ttlSeconds"`
		TTLEnforced bool   `json:"ttlEnforced"`
	}{
		IP:          ip,
		Country:     entry.Country,
		TTLSeconds:  int64((ttl - time.Since(entry.Timestamp)).Seconds()),
		TTLEnforced: a.forceMonthlyUpdate,
	}); err != nil {
		a.infoLogger.Printf("%s: response encode failed: %s", a.name, err)
	}
}
