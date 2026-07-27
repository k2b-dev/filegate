package httpadapter

import (
	"net/http"
	"net/netip"

	"github.com/valentinkolb/filegate/domain"
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
	if cfg, ok := l.snapshot(); ok {
		return cfg.Upload.MaxUploadBytes
	}
	return l.fallback.MaxUploadBytes
}

func (l liveConfig) maxChunkBytes() int64 {
	if cfg, ok := l.snapshot(); ok {
		return cfg.Upload.MaxChunkBytes
	}
	return l.fallback.MaxChunkBytes
}

func (l liveConfig) maxSessionUploadBytes() int64 {
	if cfg, ok := l.snapshot(); ok {
		return cfg.Upload.MaxSessionUploadBytes
	}
	return l.fallback.MaxSessionUploadBytes
}

func (l liveConfig) uploadMinFreeBytes() int64 {
	if cfg, ok := l.snapshot(); ok {
		return cfg.Upload.MinFreeBytes
	}
	return l.fallback.UploadMinFreeBytes
}

func (l liveConfig) publicURL() string {
	if cfg, ok := l.snapshot(); ok {
		return cfg.Server.PublicURL
	}
	return l.fallback.PublicURL
}

func (l liveConfig) bearerToken() string {
	if cfg, ok := l.snapshot(); ok {
		return cfg.Auth.BearerToken
	}
	return l.fallback.BearerToken
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
