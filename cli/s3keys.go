package cli

import (
	"crypto/rand"
	"encoding/base32"
	"encoding/json"
	"fmt"
	"log"
	"sort"
	"strings"
	"time"

	s3adapter "github.com/k2b-dev/filegate/v3/adapter/s3"
	apiv1 "github.com/k2b-dev/filegate/v3/api/v1"
	"github.com/k2b-dev/filegate/v3/infra/runtimecfg"
)

// resourceS3Key is the runtime-store kind holding access keys.
const resourceS3Key = "s3key"

// storedS3Key is one access key as persisted.
type storedS3Key struct {
	AccessKey         string   `json:"accessKey"`
	SecretKey         string   `json:"secretKey"`
	Buckets           []string `json:"buckets"`
	RequestsPerSecond int      `json:"requestsPerSecond,omitempty"`
	Burst             int      `json:"burst,omitempty"`
	Disabled          bool     `json:"disabled,omitempty"`
	CreatedAt         int64    `json:"createdAt"`
	UpdatedAt         int64    `json:"updatedAt"`
}

// S3KeyService owns access keys as runtime resources rather than config.
//
// An access key with a secret, bucket grants and a rate limit is a principal,
// not a setting: it wants create, rotate, disable and delete while the service
// runs, with an audit trail. Managing it through the config file meant every
// change needed a restart.
type S3KeyService struct {
	store   *runtimecfg.Store
	handler *s3adapter.Handler
}

func newS3KeyService(store *runtimecfg.Store) *S3KeyService {
	return &S3KeyService{store: store}
}

// AttachHandler connects the live S3 adapter once it exists.
//
// The router is built before the S3 listener, so the service is created first
// with no handler and publishes to nothing until this is called.
func (s *S3KeyService) AttachHandler(handler *s3adapter.Handler) error {
	s.handler = handler
	return s.publish()
}

// SeedOnce imports keys from the static configuration, but only into a store
// that has never been seeded.
//
// This is what keeps a revoked credential revoked. Reconciling on every start
// would resurrect a key an operator deleted, because the variable that seeded
// it is still sitting in a deployment file.
func (s *S3KeyService) SeedOnce(legacyAccess, legacySecret string, configured []s3adapter.KeyEntry) error {
	if _, done, err := s.store.Bootstrapped(resourceS3Key); err != nil {
		return err
	} else if done {
		if len(configured) > 0 || legacyAccess != "" {
			log.Printf("[filegate] s3: ignoring %d configured access key(s); the runtime store is already initialized and owns them now", len(configured)+boolToInt(legacyAccess != ""))
		}
		return nil
	}

	now := time.Now().UnixMilli()
	seeded := 0
	if legacyAccess != "" && legacySecret != "" {
		if err := s.store.PutResource(resourceS3Key, legacyAccess, storedS3Key{
			AccessKey: legacyAccess, SecretKey: legacySecret,
			Buckets: []string{"*"}, CreatedAt: now, UpdatedAt: now,
		}); err != nil {
			return err
		}
		seeded++
	}
	for _, entry := range configured {
		if err := s.store.PutResource(resourceS3Key, entry.AccessKey, storedS3Key{
			AccessKey: entry.AccessKey, SecretKey: entry.SecretKey, Buckets: entry.Buckets,
			RequestsPerSecond: entry.RequestsPerSecond, Burst: entry.Burst,
			CreatedAt: now, UpdatedAt: now,
		}); err != nil {
			return err
		}
		seeded++
	}

	if err := s.store.MarkBootstrapped(resourceS3Key, time.Now()); err != nil {
		return err
	}
	if seeded > 0 {
		log.Printf("[filegate] s3: seeded %d access key(s) into the runtime store; they are managed through the API from now on", seeded)
	}
	return s.publish()
}

func boolToInt(b bool) int {
	if b {
		return 1
	}
	return 0
}

// publish rebuilds the adapter's key set from the store.
func (s *S3KeyService) publish() error {
	if s.handler == nil {
		return nil
	}
	keys, err := s.load()
	if err != nil {
		return err
	}

	entries := make([]s3adapter.KeyEntry, 0, len(keys))
	for _, key := range keys {
		// A disabled key stays stored so it can be re-enabled, but must not
		// reach the adapter, where its presence would mean it still works.
		if key.Disabled {
			continue
		}
		entries = append(entries, s3adapter.KeyEntry{
			AccessKey: key.AccessKey, SecretKey: key.SecretKey, Buckets: key.Buckets,
			RequestsPerSecond: key.RequestsPerSecond, Burst: key.Burst,
		})
	}
	if len(entries) == 0 {
		// buildKeyStore rejects an empty set; leaving the previous one in place
		// would keep deleted keys working, so publish an explicitly empty store.
		return s.handler.SetKeys(nil)
	}
	return s.handler.SetKeys(entries)
}

func (s *S3KeyService) load() ([]storedS3Key, error) {
	raw, err := s.store.ListResources(resourceS3Key)
	if err != nil {
		return nil, err
	}
	out := make([]storedS3Key, 0, len(raw))
	for _, value := range raw {
		var key storedS3Key
		if err := json.Unmarshal(value, &key); err != nil {
			return nil, err
		}
		out = append(out, key)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].AccessKey < out[j].AccessKey })
	return out, nil
}

// List returns every key without its secret.
func (s *S3KeyService) List() ([]apiv1.S3Key, error) {
	keys, err := s.load()
	if err != nil {
		return nil, err
	}
	out := make([]apiv1.S3Key, 0, len(keys))
	for _, key := range keys {
		out = append(out, toAPIKey(key))
	}
	return out, nil
}

func toAPIKey(key storedS3Key) apiv1.S3Key {
	return apiv1.S3Key{
		AccessKey: key.AccessKey, Buckets: key.Buckets,
		RequestsPerSecond: key.RequestsPerSecond, Burst: key.Burst,
		Disabled: key.Disabled, CreatedAt: key.CreatedAt, UpdatedAt: key.UpdatedAt,
	}
}

// Create stores a new key. The secret is returned exactly once, here, because
// it is not recoverable afterwards.
func (s *S3KeyService) Create(req apiv1.S3KeyCreateRequest) (apiv1.S3KeyCreated, error) {
	access := strings.TrimSpace(req.AccessKey)
	if access == "" {
		access = generateCredential(20)
	}
	if _, err := s.get(access); err == nil {
		return apiv1.S3KeyCreated{}, fmt.Errorf("access key %q already exists", access)
	}

	buckets := req.Buckets
	if len(buckets) == 0 {
		return apiv1.S3KeyCreated{}, fmt.Errorf("buckets is required; use [\"*\"] to grant every mount")
	}

	now := time.Now().UnixMilli()
	key := storedS3Key{
		AccessKey: access, SecretKey: generateCredential(40), Buckets: buckets,
		RequestsPerSecond: req.RequestsPerSecond, Burst: req.Burst,
		CreatedAt: now, UpdatedAt: now,
	}
	if err := s.save(key); err != nil {
		return apiv1.S3KeyCreated{}, err
	}
	return apiv1.S3KeyCreated{S3Key: toAPIKey(key), SecretKey: key.SecretKey}, nil
}

// Rotate issues a new secret for an existing key, keeping its grants.
func (s *S3KeyService) Rotate(accessKey string) (apiv1.S3KeyCreated, error) {
	key, err := s.get(accessKey)
	if err != nil {
		return apiv1.S3KeyCreated{}, err
	}
	key.SecretKey = generateCredential(40)
	key.UpdatedAt = time.Now().UnixMilli()
	if err := s.save(key); err != nil {
		return apiv1.S3KeyCreated{}, err
	}
	return apiv1.S3KeyCreated{S3Key: toAPIKey(key), SecretKey: key.SecretKey}, nil
}

// Update changes grants, limits or the disabled flag.
func (s *S3KeyService) Update(accessKey string, req apiv1.S3KeyUpdateRequest) (apiv1.S3Key, error) {
	key, err := s.get(accessKey)
	if err != nil {
		return apiv1.S3Key{}, err
	}
	if req.Buckets != nil {
		if len(*req.Buckets) == 0 {
			return apiv1.S3Key{}, fmt.Errorf("buckets cannot be empty; disable the key instead")
		}
		key.Buckets = *req.Buckets
	}
	if req.RequestsPerSecond != nil {
		key.RequestsPerSecond = *req.RequestsPerSecond
	}
	if req.Burst != nil {
		key.Burst = *req.Burst
	}
	if req.Disabled != nil {
		key.Disabled = *req.Disabled
	}
	key.UpdatedAt = time.Now().UnixMilli()

	if err := s.save(key); err != nil {
		return apiv1.S3Key{}, err
	}
	return toAPIKey(key), nil
}

// Delete removes a key permanently. It stays deleted across restarts even when
// the configuration that originally seeded it is still present.
func (s *S3KeyService) Delete(accessKey string) error {
	if _, err := s.get(accessKey); err != nil {
		return err
	}
	if err := s.store.DeleteResource(resourceS3Key, accessKey); err != nil {
		return err
	}
	return s.publish()
}

func (s *S3KeyService) get(accessKey string) (storedS3Key, error) {
	var key storedS3Key
	if err := s.store.GetResource(resourceS3Key, accessKey, &key); err != nil {
		if err == runtimecfg.ErrNotFound {
			return storedS3Key{}, fmt.Errorf("access key %q not found", accessKey)
		}
		return storedS3Key{}, err
	}
	return key, nil
}

// save persists then republishes. Publishing validates against the live mounts,
// so an invalid bucket grant is caught before it can be used.
func (s *S3KeyService) save(key storedS3Key) error {
	if err := s.store.PutResource(resourceS3Key, key.AccessKey, key); err != nil {
		return err
	}
	if err := s.publish(); err != nil {
		// Roll back so the store never holds a key the adapter rejected.
		_ = s.store.DeleteResource(resourceS3Key, key.AccessKey)
		_ = s.publish()
		return err
	}
	return nil
}

// generateCredential produces an unambiguous, shell-safe credential.
//
// Base32 without padding avoids the +/= characters that make base64 awkward in
// URLs and config files, and keeps credentials copyable by hand.
func generateCredential(length int) string {
	raw := make([]byte, length)
	if _, err := rand.Read(raw); err != nil {
		panic("filegate: no entropy available for credential generation: " + err.Error())
	}
	encoded := base32.StdEncoding.WithPadding(base32.NoPadding).EncodeToString(raw)
	return strings.ToUpper(encoded[:length])
}
