package cli

import (
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/valentinkolb/filegate/domain"
	"go.yaml.in/yaml/v3"
)

type configYAMLSet struct {
	Path  string
	Value any
}

type configWriteResult struct {
	Path       string
	BackupPath string
}

func readConfigDocument(path string) (*yaml.Node, error) {
	var doc yaml.Node
	data, err := os.ReadFile(path)
	if err != nil {
		if !os.IsNotExist(err) {
			return nil, err
		}
		doc.Kind = yaml.DocumentNode
		doc.Content = []*yaml.Node{{Kind: yaml.MappingNode}}
		return &doc, nil
	}
	if len(data) == 0 {
		doc.Kind = yaml.DocumentNode
		doc.Content = []*yaml.Node{{Kind: yaml.MappingNode}}
		return &doc, nil
	}
	if err := yaml.Unmarshal(data, &doc); err != nil {
		return nil, err
	}
	if len(doc.Content) == 0 {
		doc.Content = []*yaml.Node{{Kind: yaml.MappingNode}}
	}
	if doc.Content[0].Kind != yaml.MappingNode {
		return nil, fmt.Errorf("config root must be a YAML mapping")
	}
	return &doc, nil
}

func writeConfigSets(path string, backup bool, sets []configYAMLSet) (configWriteResult, error) {
	doc, err := readConfigDocument(path)
	if err != nil {
		return configWriteResult{}, err
	}
	for _, set := range sets {
		if err := setYAMLPath(doc.Content[0], set.Path, valueToYAMLNode(set.Value)); err != nil {
			return configWriteResult{}, err
		}
	}
	return writeConfigDocumentAtomic(path, backup, doc)
}

func writeConfigDocumentAtomic(path string, backup bool, doc *yaml.Node) (configWriteResult, error) {
	path = filepath.Clean(path)
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return configWriteResult{}, err
	}

	// New config files default to owner-only: they carry the REST
	// bearer token and S3 secrets. Existing files keep their mode
	// (the package ships /etc/filegate/conf.yaml as root:filegate
	// 0640 so the service user can read it).
	mode := os.FileMode(0o600)
	if info, err := os.Stat(path); err == nil {
		mode = info.Mode().Perm()
	} else if !os.IsNotExist(err) {
		return configWriteResult{}, err
	}

	data, err := yaml.Marshal(doc)
	if err != nil {
		return configWriteResult{}, err
	}
	tmp := filepath.Join(dir, fmt.Sprintf(".%s.tmp-%d.yaml", filepath.Base(path), os.Getpid()))
	if err := os.WriteFile(tmp, data, mode); err != nil {
		return configWriteResult{}, err
	}
	defer func() { _ = os.Remove(tmp) }()

	if _, err := loadConfig(tmp); err != nil {
		return configWriteResult{}, fmt.Errorf("resulting config is invalid: %w", err)
	}

	var backupPath string
	if backup {
		if _, err := os.Stat(path); err == nil {
			backupPath = filepath.Join(dir, fmt.Sprintf("%s.bak.%s", filepath.Base(path), time.Now().UTC().Format("20060102T150405.000000000Z")))
			if err := copyFile(path, backupPath, mode); err != nil {
				return configWriteResult{}, fmt.Errorf("backup config: %w", err)
			}
		} else if !os.IsNotExist(err) {
			return configWriteResult{}, err
		}
	}

	if err := os.Rename(tmp, path); err != nil {
		return configWriteResult{}, err
	}
	return configWriteResult{Path: path, BackupPath: backupPath}, nil
}

func copyFile(src, dst string, mode os.FileMode) error {
	data, err := os.ReadFile(src)
	if err != nil {
		return err
	}
	return os.WriteFile(dst, data, mode)
}

func setYAMLPath(root *yaml.Node, path string, value *yaml.Node) error {
	parts := splitYAMLPath(path)
	if len(parts) == 0 {
		return fmt.Errorf("empty config path")
	}
	cur := root
	for _, part := range parts[:len(parts)-1] {
		next := mappingValue(cur, part)
		if next == nil {
			next = &yaml.Node{Kind: yaml.MappingNode}
			cur.Content = append(cur.Content, &yaml.Node{Kind: yaml.ScalarNode, Tag: "!!str", Value: part}, next)
		}
		if next.Kind != yaml.MappingNode {
			return fmt.Errorf("%s is not a YAML mapping", part)
		}
		cur = next
	}
	last := parts[len(parts)-1]
	for i := 0; i < len(cur.Content); i += 2 {
		if cur.Content[i].Value == last {
			cur.Content[i+1] = value
			return nil
		}
	}
	cur.Content = append(cur.Content, &yaml.Node{Kind: yaml.ScalarNode, Tag: "!!str", Value: last}, value)
	return nil
}

func mappingValue(node *yaml.Node, key string) *yaml.Node {
	if node == nil || node.Kind != yaml.MappingNode {
		return nil
	}
	for i := 0; i < len(node.Content); i += 2 {
		if node.Content[i].Value == key {
			return node.Content[i+1]
		}
	}
	return nil
}

func splitYAMLPath(path string) []string {
	var parts []string
	for _, part := range strings.Split(path, ".") {
		if part != "" {
			parts = append(parts, part)
		}
	}
	return parts
}

func valueToYAMLNode(value any) *yaml.Node {
	switch v := value.(type) {
	case string:
		return &yaml.Node{Kind: yaml.ScalarNode, Tag: "!!str", Value: v}
	case bool:
		return &yaml.Node{Kind: yaml.ScalarNode, Tag: "!!bool", Value: strconv.FormatBool(v)}
	case int:
		return &yaml.Node{Kind: yaml.ScalarNode, Tag: "!!int", Value: strconv.Itoa(v)}
	case int64:
		return &yaml.Node{Kind: yaml.ScalarNode, Tag: "!!int", Value: strconv.FormatInt(v, 10)}
	case []string:
		return stringSliceNode(v)
	case []domain.S3KeyConfig:
		return s3KeysNode(v)
	case []domain.RetentionBucketConfig:
		return retentionBucketsNode(v)
	default:
		return &yaml.Node{Kind: yaml.ScalarNode, Tag: "!!str", Value: fmt.Sprint(v)}
	}
}

func stringSliceNode(values []string) *yaml.Node {
	node := &yaml.Node{Kind: yaml.SequenceNode}
	for _, value := range values {
		node.Content = append(node.Content, &yaml.Node{Kind: yaml.ScalarNode, Tag: "!!str", Value: value})
	}
	return node
}

func s3KeysNode(keys []domain.S3KeyConfig) *yaml.Node {
	node := &yaml.Node{Kind: yaml.SequenceNode}
	for _, key := range keys {
		item := &yaml.Node{Kind: yaml.MappingNode}
		appendMapScalar(item, "access_key", key.AccessKey)
		appendMapScalar(item, "secret_key", key.SecretKey)
		appendMapNode(item, "buckets", stringSliceNode(key.Buckets))
		appendMapInt(item, "requests_per_second", key.RequestsPerSecond)
		appendMapInt(item, "burst", key.Burst)
		node.Content = append(node.Content, item)
	}
	return node
}

func retentionBucketsNode(buckets []domain.RetentionBucketConfig) *yaml.Node {
	node := &yaml.Node{Kind: yaml.SequenceNode}
	for _, bucket := range buckets {
		item := &yaml.Node{Kind: yaml.MappingNode}
		appendMapScalar(item, "keep_for", bucket.KeepFor.String())
		appendMapInt(item, "max_count", bucket.MaxCount)
		node.Content = append(node.Content, item)
	}
	return node
}

func appendMapScalar(node *yaml.Node, key, value string) {
	appendMapNode(node, key, &yaml.Node{Kind: yaml.ScalarNode, Tag: "!!str", Value: value})
}

func appendMapInt(node *yaml.Node, key string, value int) {
	appendMapNode(node, key, &yaml.Node{Kind: yaml.ScalarNode, Tag: "!!int", Value: strconv.Itoa(value)})
}

func appendMapNode(node *yaml.Node, key string, value *yaml.Node) {
	node.Content = append(node.Content, &yaml.Node{Kind: yaml.ScalarNode, Tag: "!!str", Value: key}, value)
}

func configDisplayMap(cfg domain.Config, showSecrets bool) map[string]any {
	return map[string]any{
		"server": map[string]any{
			"listen":          cfg.Server.Listen,
			"public_url":      cfg.Server.PublicURL,
			"trusted_proxies": cfg.Server.TrustedProxies,
			"cors": map[string]any{
				"allowed_origins":   cfg.Server.CORS.AllowedOrigins,
				"allowed_methods":   cfg.Server.CORS.AllowedMethods,
				"allowed_headers":   cfg.Server.CORS.AllowedHeaders,
				"exposed_headers":   cfg.Server.CORS.ExposedHeaders,
				"max_age":           cfg.Server.CORS.MaxAge.String(),
				"allow_credentials": cfg.Server.CORS.AllowCredentials,
			},
			"write_timeout":      cfg.Server.WriteTimeout.String(),
			"access_log_enabled": cfg.Server.AccessLogEnabled,
			"shutdown_timeout":   cfg.Server.ShutdownTimeout.String(),
		},
		"auth": map[string]any{
			"bearer_token": maskSecret(cfg.Auth.BearerToken, showSecrets),
		},
		"storage": map[string]any{
			"base_paths": cfg.Storage.BasePaths,
			"index_path": cfg.Storage.IndexPath,
		},
		"detection": map[string]any{
			"backend":            cfg.Detection.Backend,
			"poll_interval":      cfg.Detection.PollInterval.String(),
			"reconcile_interval": cfg.Detection.ReconcileInterval.String(),
		},
		"cache": map[string]any{
			"path_cache_size": cfg.Cache.PathCacheSize,
		},
		"jobs": map[string]any{
			"workers":              cfg.Jobs.Workers,
			"queue_size":           cfg.Jobs.QueueSize,
			"thumbnail_workers":    cfg.Jobs.ThumbnailWorkers,
			"thumbnail_queue_size": cfg.Jobs.ThumbnailQueueSize,
		},
		"upload": map[string]any{
			"expiry":                        cfg.Upload.Expiry.String(),
			"cleanup_interval":              cfg.Upload.CleanupInterval.String(),
			"max_chunk_bytes":               cfg.Upload.MaxChunkBytes,
			"max_upload_bytes":              cfg.Upload.MaxUploadBytes,
			"max_session_upload_bytes":      cfg.Upload.MaxSessionUploadBytes,
			"max_concurrent_segment_writes": cfg.Upload.MaxConcurrentSegmentWrites,
			"min_free_bytes":                cfg.Upload.MinFreeBytes,
		},
		"thumbnail": map[string]any{
			"lru_cache_size":   cfg.Thumbnail.LRUCacheSize,
			"max_source_bytes": cfg.Thumbnail.MaxSourceBytes,
			"max_pixels":       cfg.Thumbnail.MaxPixels,
		},
		"versioning": map[string]any{
			"enabled":                   cfg.Versioning.Enabled,
			"cooldown":                  cfg.Versioning.Cooldown.String(),
			"min_size_for_auto_v1":      cfg.Versioning.MinSizeForAutoV1,
			"retention_buckets":         displayRetentionBuckets(cfg.Versioning.RetentionBuckets),
			"pruner_interval":           cfg.Versioning.PrunerInterval.String(),
			"max_pinned_per_file":       cfg.Versioning.MaxPinnedPerFile,
			"pinned_grace_after_delete": cfg.Versioning.PinnedGraceAfterDelete.String(),
			"max_label_bytes":           cfg.Versioning.MaxLabelBytes,
		},
		"s3": map[string]any{
			"enabled":               cfg.S3.Enabled,
			"listen":                cfg.S3.Listen,
			"region":                cfg.S3.Region,
			"access_key":            cfg.S3.AccessKey,
			"secret_key":            maskSecret(cfg.S3.SecretKey, showSecrets),
			"max_concurrent_writes": cfg.S3.MaxConcurrentWrites,
			"keys":                  displayS3Keys(cfg.S3.Keys, showSecrets),
			"cleanup": map[string]any{
				"done_retention":       cfg.S3.Cleanup.DoneRetention.String(),
				"aborted_retention":    cfg.S3.Cleanup.AbortedRetention.String(),
				"stuck_upload_max_age": cfg.S3.Cleanup.StuckUploadMaxAge.String(),
				"interval":             cfg.S3.Cleanup.Interval.String(),
			},
		},
		"metrics": map[string]any{
			"enabled": cfg.Metrics.Enabled,
			"path":    cfg.Metrics.Path,
			"token":   maskSecret(cfg.Metrics.Token, showSecrets),
		},
		"activity": map[string]any{
			"ring_buffer_size": cfg.Activity.RingBufferSize,
		},
	}
}

func displayRetentionBuckets(buckets []domain.RetentionBucketConfig) []map[string]any {
	out := make([]map[string]any, 0, len(buckets))
	for _, bucket := range buckets {
		out = append(out, map[string]any{"keep_for": bucket.KeepFor.String(), "max_count": bucket.MaxCount})
	}
	return out
}

func displayS3Keys(keys []domain.S3KeyConfig, showSecrets bool) []map[string]any {
	out := make([]map[string]any, 0, len(keys))
	for _, key := range keys {
		out = append(out, map[string]any{
			"access_key":          key.AccessKey,
			"secret_key":          maskSecret(key.SecretKey, showSecrets),
			"buckets":             key.Buckets,
			"requests_per_second": key.RequestsPerSecond,
			"burst":               key.Burst,
		})
	}
	return out
}

func maskSecret(value string, showSecrets bool) string {
	if showSecrets || value == "" {
		return value
	}
	return "<redacted>"
}
