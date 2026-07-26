package cli

import (
	"log"
	"os"
	"strings"
	"time"

	"github.com/valentinkolb/filegate/domain"
	"github.com/valentinkolb/filegate/infra/runtimecfg"
)

// defaultBasePath is the mount a fresh install serves when nothing is
// configured. The packages already create /var/lib/filegate.
const defaultBasePath = "/var/lib/filegate/data"

// resourceBearerToken is the runtime-store kind holding the generated token.
const resourceBearerToken = "bearer"

type storedToken struct {
	Token     string `json:"token"`
	CreatedAt int64  `json:"createdAt"`
}

// ensureDefaultBasePath creates the built-in mount so the service starts on a
// clean machine.
//
// Only the exact default is created. A configured path is left alone on
// purpose: a typo there must fail the health check loudly rather than quietly
// serving a freshly made empty directory that looks like data loss.
func ensureDefaultBasePath(cfg domain.Config) error {
	if len(cfg.Storage.BasePaths) != 1 || cfg.Storage.BasePaths[0] != defaultBasePath {
		return nil
	}
	return os.MkdirAll(defaultBasePath, 0o755)
}

// bootstrapBearerToken returns the API token, generating and storing one the
// first time the service runs without a configured value.
//
// The token is printed once, prominently. A credential that scrolls past in a
// log nobody reads is barely better than no credential, so the message says
// plainly that this is the only time it appears.
func bootstrapBearerToken(store *runtimecfg.Store, configured string) (string, error) {
	if strings.TrimSpace(configured) != "" {
		return configured, nil
	}

	var existing storedToken
	err := store.GetResource(resourceBearerToken, "api", &existing)
	if err == nil && existing.Token != "" {
		return existing.Token, nil
	}
	if err != nil && err != runtimecfg.ErrNotFound {
		return "", err
	}

	token := generateCredential(40)
	if err := store.PutResource(resourceBearerToken, "api", storedToken{Token: token, CreatedAt: time.Now().UnixMilli()}); err != nil {
		return "", err
	}
	if err := store.MarkBootstrapped(resourceBearerToken, time.Now()); err != nil {
		return "", err
	}

	log.Printf("\n"+
		"┌─ filegate: generated an API token ─────────────────────────────────\n"+
		"│\n"+
		"│   %s\n"+
		"│\n"+
		"│  This is shown only once. Store it now, or set auth.bearer_token\n"+
		"│  to a value of your own and restart.\n"+
		"└────────────────────────────────────────────────────────────────────\n", token)
	return token, nil
}
