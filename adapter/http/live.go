package httpadapter

import (
	"net/http"
	"net/netip"
	"strings"

	"github.com/k2b-dev/filegate/v3/domain"
)

// liveConfig reads runtime-scoped settings from the published snapshot.
//
// Without this manifest apply would be dishonest: it reports a key as
// runtime-activated, while handlers keep using whatever they captured when the
// router was built. Each accessor falls back to the
// boot-time options when no holder is supplied, which is how the existing
// callers and every current test keep working.
type liveConfig struct {
	holder   *domain.ConfigHolder
	fallback RouterOptions
}

func newLiveConfig(opts RouterOptions) liveConfig {
	return liveConfig{holder: opts.Config, fallback: opts}
}

// snapshot reports the live configuration and whether one is available.
func (l liveConfig) snapshot() (domain.Config, bool) {
	if l.holder == nil {
		return domain.Config{}, false
	}
	return l.holder.Get(), true
}

func (l liveConfig) maxUploadBytes() int64 {
	value := l.fallback.MaxUploadBytes
	if cfg, ok := l.snapshot(); ok {
		value = cfg.Upload.MaxUploadBytes
	}
	if value <= 0 {
		return 500 << 20
	}
	return value
}

func (l liveConfig) maxChunkBytes() int64 {
	value := l.fallback.MaxChunkBytes
	if cfg, ok := l.snapshot(); ok {
		value = cfg.Upload.MaxChunkBytes
	}
	if value <= 0 {
		return 50 << 20
	}
	return value
}

func (l liveConfig) maxSessionUploadBytes() int64 {
	value := l.fallback.MaxSessionUploadBytes
	if cfg, ok := l.snapshot(); ok {
		value = cfg.Upload.MaxSessionUploadBytes
	}
	if value <= 0 {
		return 50 << 30
	}
	return value
}

func (l liveConfig) uploadMinFreeBytes() int64 {
	value := l.fallback.UploadMinFreeBytes
	if cfg, ok := l.snapshot(); ok {
		value = cfg.Upload.MinFreeBytes
	}
	if value < 0 {
		return 0
	}
	return value
}

func (l liveConfig) publicURL() string {
	value := l.fallback.PublicURL
	if cfg, ok := l.snapshot(); ok {
		value = cfg.Server.PublicURL
	}
	return strings.TrimRight(strings.TrimSpace(value), "/")
}

// trustedProxies re-parses on every read.
//
// The parsed form is not stored in the config, and the list is short, so
// parsing per request is cheaper than the bookkeeping needed to cache it. A
// malformed entry cannot appear here: validation rejects it before the snapshot
// is published.
func (l liveConfig) trustedProxies() []netip.Prefix {
	cfg, ok := l.snapshot()
	if !ok {
		return l.fallback.TrustedProxies
	}
	parsed, err := ParseTrustedProxies(cfg.Server.TrustedProxies)
	if err != nil {
		return l.fallback.TrustedProxies
	}
	return parsed
}

func (l liveConfig) cors() domain.CORSConfig {
	if cfg, ok := l.snapshot(); ok {
		return cfg.Server.CORS
	}
	return l.fallback.CORS
}

func (l liveConfig) accessLogEnabled() bool {
	if cfg, ok := l.snapshot(); ok {
		return cfg.Server.AccessLogEnabled
	}
	return l.fallback.AccessLogEnabled
}

// The middlewares below re-read their settings on every request. Deciding once
// when the chain is built would mean a change to CORS, trusted proxies or
// access logging only applied after a restart, while the schema reported those
// keys as runtime-activated.

func liveRealIPMiddleware(live liveConfig) middlewareFunc {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			inner := realIPMiddleware(live.trustedProxies())
			if inner == nil {
				next.ServeHTTP(w, r)
				return
			}
			inner(next).ServeHTTP(w, r)
		})
	}
}

func liveCORSMiddleware(live liveConfig) middlewareFunc {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			inner := corsMiddleware(live.cors())
			if inner == nil {
				next.ServeHTTP(w, r)
				return
			}
			inner(next).ServeHTTP(w, r)
		})
	}
}

func liveAccessLogMiddleware(live liveConfig) middlewareFunc {
	return func(next http.Handler) http.Handler {
		logged := accessLogMiddleware(next)
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if !live.accessLogEnabled() {
				next.ServeHTTP(w, r)
				return
			}
			logged.ServeHTTP(w, r)
		})
	}
}
