package cli

import (
	"crypto/rand"
	"encoding/base32"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"sort"
	"strings"

	"github.com/spf13/cobra"
	"github.com/valentinkolb/filegate/domain"
	"github.com/valentinkolb/filegate/infra/filesystem"
	"go.yaml.in/yaml/v3"
)

func newDaemonConfigCmd() *cobra.Command {
	var configFile string
	cmd := &cobra.Command{
		Use:   "config",
		Short: "Inspect bootstrap config and manage declarative manifests",
		Long:  "Inspect or edit bootstrap YAML offline, and plan or apply a versioned manifest through the authenticated daemon API.",
	}
	cmd.PersistentFlags().StringVar(&configFile, "config", "", "path to config file")
	cmd.AddCommand(newConfigShowCmd(&configFile))
	cmd.AddCommand(newConfigSchemaCmd())
	cmd.AddCommand(newConfigValidateCmd(&configFile))
	cmd.AddCommand(newConfigSetCmd(&configFile))
	cmd.AddCommand(newConfigPlanCmd(&configFile))
	cmd.AddCommand(newConfigApplyCmd(&configFile))
	cmd.AddCommand(newConfigS3Cmd(&configFile))
	cmd.AddCommand(newConfigMountCmd(&configFile))
	return cmd
}

func newConfigShowCmd(configFile *string) *cobra.Command {
	var format string
	var showSecrets bool
	cmd := &cobra.Command{
		Use:   "show",
		Short: "Print the effective config",
		RunE: func(cmd *cobra.Command, _ []string) error {
			cfg, err := loadConfig(*configFile)
			if err != nil {
				return err
			}
			view := configDisplayMap(cfg, showSecrets)
			switch strings.ToLower(strings.TrimSpace(format)) {
			case "yaml", "":
				data, err := yaml.Marshal(view)
				if err != nil {
					return err
				}
				_, err = cmd.OutOrStdout().Write(data)
				return err
			case "json":
				enc := json.NewEncoder(cmd.OutOrStdout())
				enc.SetEscapeHTML(false)
				enc.SetIndent("", "  ")
				return enc.Encode(view)
			default:
				return fmt.Errorf("--format must be yaml or json")
			}
		},
	}
	cmd.Flags().StringVar(&format, "format", "yaml", "output format: yaml or json")
	cmd.Flags().BoolVar(&showSecrets, "show-secrets", false, "print secret values instead of redacting them")
	return cmd
}

func newConfigValidateCmd(configFile *string) *cobra.Command {
	return &cobra.Command{
		Use:   "validate",
		Short: "Validate the resolved config",
		RunE: func(cmd *cobra.Command, _ []string) error {
			cfg, err := loadConfig(*configFile)
			if err != nil {
				return err
			}
			if err := validateResolvedConfig(cfg); err != nil {
				return err
			}
			_, err = fmt.Fprintln(cmd.OutOrStdout(), "config ok")
			return err
		},
	}
}

func newConfigSetCmd(configFile *string) *cobra.Command {
	var noBackup bool
	cmd := &cobra.Command{
		Use:   "set",
		Short: "Set one or more config values in YAML",
		RunE: func(cmd *cobra.Command, _ []string) error {
			if err := requireExplicitConfig(cmd, *configFile); err != nil {
				return err
			}
			sets, err := changedConfigFlagValues(cmd.Flags())
			if err != nil {
				return err
			}
			if len(sets) == 0 {
				return fmt.Errorf("at least one config flag is required")
			}
			res, err := writeConfigSets(*configFile, !noBackup, sets)
			if err != nil {
				return err
			}
			return printConfigWriteResult(cmd, res)
		},
	}
	registerConfigFlags(cmd.Flags())
	cmd.Flags().BoolVar(&noBackup, "no-backup", false, "skip timestamped backup before replacing config")
	return cmd
}

func newConfigS3Cmd(configFile *string) *cobra.Command {
	cmd := &cobra.Command{
		Use:   "s3",
		Short: "Manage S3 config",
	}
	cmd.AddCommand(newConfigS3KeyCmd(configFile))
	return cmd
}

func newConfigS3KeyCmd(configFile *string) *cobra.Command {
	cmd := &cobra.Command{
		Use:   "key",
		Short: "Manage S3 key entries",
	}
	cmd.AddCommand(newConfigS3KeyGenerateCmd())
	cmd.AddCommand(newConfigS3KeyListCmd(configFile))
	cmd.AddCommand(newConfigS3KeyAddCmd(configFile))
	cmd.AddCommand(newConfigS3KeyDisableCmd(configFile))
	cmd.AddCommand(newConfigS3KeyRemoveCmd(configFile))
	return cmd
}

func newConfigS3KeyGenerateCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "generate",
		Short: "Generate an S3 access key and secret",
		RunE: func(cmd *cobra.Command, _ []string) error {
			accessKey, secretKey, err := generateS3Credentials()
			if err != nil {
				return err
			}
			_, err = fmt.Fprintf(cmd.OutOrStdout(), "access_key: %s\nsecret_key: %s\n", accessKey, secretKey)
			return err
		},
	}
}

func newConfigS3KeyListCmd(configFile *string) *cobra.Command {
	var showSecrets bool
	cmd := &cobra.Command{
		Use:   "list",
		Short: "List configured S3 keys",
		RunE: func(cmd *cobra.Command, _ []string) error {
			cfg, err := loadConfig(*configFile)
			if err != nil {
				return err
			}
			out := cmd.OutOrStdout()
			if cfg.S3.AccessKey != "" || cfg.S3.SecretKey != "" {
				fmt.Fprintf(out, "legacy access_key=%s secret_key=%s buckets=*\n",
					cfg.S3.AccessKey, maskSecret(cfg.S3.SecretKey, showSecrets))
			}
			for _, key := range cfg.S3.Keys {
				fmt.Fprintf(out, "access_key=%s secret_key=%s buckets=%s requests_per_second=%d burst=%d\n",
					key.AccessKey, maskSecret(key.SecretKey, showSecrets), strings.Join(key.Buckets, ","), key.RequestsPerSecond, key.Burst)
			}
			return nil
		},
	}
	cmd.Flags().BoolVar(&showSecrets, "show-secrets", false, "print secret values instead of redacting them")
	return cmd
}

func newConfigS3KeyAddCmd(configFile *string) *cobra.Command {
	var accessKey, secretKey string
	var buckets []string
	var allBuckets bool
	var rps, burst int
	var noBackup bool
	cmd := &cobra.Command{
		Use:   "add",
		Short: "Add an S3 key entry",
		RunE: func(cmd *cobra.Command, _ []string) error {
			if err := requireExplicitConfig(cmd, *configFile); err != nil {
				return err
			}
			cfg, err := loadConfig(*configFile)
			if err != nil {
				return err
			}
			if accessKey == "" || secretKey == "" {
				genAccess, genSecret, err := generateS3Credentials()
				if err != nil {
					return err
				}
				if accessKey == "" {
					accessKey = genAccess
				}
				if secretKey == "" {
					secretKey = genSecret
				}
			}
			keyBuckets, err := resolveS3KeyBuckets(cfg, buckets, allBuckets)
			if err != nil {
				return err
			}
			key := domain.S3KeyConfig{
				AccessKey:         accessKey,
				SecretKey:         secretKey,
				Buckets:           keyBuckets,
				RequestsPerSecond: rps,
				Burst:             burst,
			}
			cfg.S3.Keys = append(cfg.S3.Keys, key)
			if err := validateResolvedConfig(cfg); err != nil {
				return err
			}
			res, err := writeConfigSets(*configFile, !noBackup, []configYAMLSet{{Path: "s3.keys", Value: cfg.S3.Keys}})
			if err != nil {
				return err
			}
			if err := printConfigWriteResult(cmd, res); err != nil {
				return err
			}
			_, err = fmt.Fprintf(cmd.OutOrStdout(), "created access_key=%s secret_key=%s\n", accessKey, secretKey)
			return err
		},
	}
	cmd.Flags().StringVar(&accessKey, "access-key", "", "access key to add; generated when omitted")
	cmd.Flags().StringVar(&secretKey, "secret-key", "", "secret key to add; generated when omitted")
	cmd.Flags().StringArrayVar(&buckets, "bucket", nil, "allowed bucket name; repeat for multiple buckets")
	cmd.Flags().BoolVar(&allBuckets, "all-buckets", false, "grant access to every configured mount")
	cmd.Flags().IntVar(&rps, "requests-per-second", 0, "per-key sustained request rate; 0 disables throttling")
	cmd.Flags().IntVar(&burst, "burst", 0, "per-key request burst; defaults to requests-per-second when unset")
	cmd.Flags().BoolVar(&noBackup, "no-backup", false, "skip timestamped backup before replacing config")
	return cmd
}

func newConfigS3KeyDisableCmd(configFile *string) *cobra.Command {
	var noBackup bool
	cmd := &cobra.Command{
		Use:   "disable <access-key>",
		Short: "Disable an S3 key by clearing its bucket whitelist",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			if err := requireExplicitConfig(cmd, *configFile); err != nil {
				return err
			}
			cfg, err := loadConfig(*configFile)
			if err != nil {
				return err
			}
			accessKey := args[0]
			found := false
			for i := range cfg.S3.Keys {
				if cfg.S3.Keys[i].AccessKey == accessKey {
					cfg.S3.Keys[i].Buckets = nil
					found = true
					break
				}
			}
			if !found {
				return fmt.Errorf("s3 key %q not found in s3.keys", accessKey)
			}
			if err := validateResolvedConfig(cfg); err != nil {
				return err
			}
			res, err := writeConfigSets(*configFile, !noBackup, []configYAMLSet{{Path: "s3.keys", Value: cfg.S3.Keys}})
			if err != nil {
				return err
			}
			return printConfigWriteResult(cmd, res)
		},
	}
	cmd.Flags().BoolVar(&noBackup, "no-backup", false, "skip timestamped backup before replacing config")
	return cmd
}

func newConfigS3KeyRemoveCmd(configFile *string) *cobra.Command {
	var noBackup bool
	cmd := &cobra.Command{
		Use:   "remove <access-key>",
		Short: "Remove an S3 key entry",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			if err := requireExplicitConfig(cmd, *configFile); err != nil {
				return err
			}
			cfg, err := loadConfig(*configFile)
			if err != nil {
				return err
			}
			accessKey := args[0]
			var sets []configYAMLSet
			found := false
			if cfg.S3.AccessKey == accessKey {
				cfg.S3.AccessKey = ""
				cfg.S3.SecretKey = ""
				sets = append(sets,
					configYAMLSet{Path: "s3.access_key", Value: ""},
					configYAMLSet{Path: "s3.secret_key", Value: ""},
				)
				found = true
			}
			filtered := cfg.S3.Keys[:0]
			for _, key := range cfg.S3.Keys {
				if key.AccessKey == accessKey {
					found = true
					continue
				}
				filtered = append(filtered, key)
			}
			cfg.S3.Keys = filtered
			if !found {
				return fmt.Errorf("s3 key %q not found", accessKey)
			}
			if err := validateResolvedConfig(cfg); err != nil {
				return err
			}
			sets = append(sets, configYAMLSet{Path: "s3.keys", Value: cfg.S3.Keys})
			res, err := writeConfigSets(*configFile, !noBackup, sets)
			if err != nil {
				return err
			}
			return printConfigWriteResult(cmd, res)
		},
	}
	cmd.Flags().BoolVar(&noBackup, "no-backup", false, "skip timestamped backup before replacing config")
	return cmd
}

func newConfigMountCmd(configFile *string) *cobra.Command {
	cmd := &cobra.Command{
		Use:   "mount",
		Short: "Manage storage mounts",
	}
	cmd.AddCommand(newConfigMountListCmd(configFile))
	cmd.AddCommand(newConfigMountAddCmd(configFile))
	cmd.AddCommand(newConfigMountRemoveCmd(configFile))
	return cmd
}

func newConfigMountListCmd(configFile *string) *cobra.Command {
	return &cobra.Command{
		Use:   "list",
		Short: "List storage mounts",
		RunE: func(cmd *cobra.Command, _ []string) error {
			cfg, err := loadConfig(*configFile)
			if err != nil {
				return err
			}
			paths := append([]string(nil), cfg.Storage.BasePaths...)
			sort.Strings(paths)
			for _, path := range paths {
				fmt.Fprintf(cmd.OutOrStdout(), "bucket=%s path=%s\n", filepath.Base(filepath.Clean(path)), path)
			}
			return nil
		},
	}
}

func newConfigMountAddCmd(configFile *string) *cobra.Command {
	var noBackup bool
	cmd := &cobra.Command{
		Use:   "add <path>",
		Short: "Add a storage mount path",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			if err := requireExplicitConfig(cmd, *configFile); err != nil {
				return err
			}
			path, err := filepath.Abs(args[0])
			if err != nil {
				return err
			}
			if err := validateMountPathForConfig(path); err != nil {
				return err
			}
			cfg, err := loadConfig(*configFile)
			if err != nil {
				return err
			}
			for _, existing := range cfg.Storage.BasePaths {
				if filepath.Clean(existing) == filepath.Clean(path) {
					return fmt.Errorf("mount path %s is already configured", path)
				}
			}
			if cfg.S3.Enabled {
				if err := domain.ValidateBucketName(filepath.Base(filepath.Clean(path))); err != nil {
					return err
				}
			}
			cfg.Storage.BasePaths = append(cfg.Storage.BasePaths, path)
			if err := validateResolvedConfig(cfg); err != nil {
				return err
			}
			res, err := writeConfigSets(*configFile, !noBackup, []configYAMLSet{{Path: "storage.base_paths", Value: cfg.Storage.BasePaths}})
			if err != nil {
				return err
			}
			return printConfigWriteResult(cmd, res)
		},
	}
	cmd.Flags().BoolVar(&noBackup, "no-backup", false, "skip timestamped backup before replacing config")
	return cmd
}

func newConfigMountRemoveCmd(configFile *string) *cobra.Command {
	var noBackup bool
	cmd := &cobra.Command{
		Use:   "remove <path-or-bucket>",
		Short: "Remove a storage mount path from config",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			if err := requireExplicitConfig(cmd, *configFile); err != nil {
				return err
			}
			cfg, err := loadConfig(*configFile)
			if err != nil {
				return err
			}
			needle := filepath.Clean(args[0])
			filtered := cfg.Storage.BasePaths[:0]
			found := false
			for _, existing := range cfg.Storage.BasePaths {
				if filepath.Clean(existing) == needle || filepath.Base(filepath.Clean(existing)) == args[0] {
					found = true
					continue
				}
				filtered = append(filtered, existing)
			}
			if !found {
				return fmt.Errorf("mount %q not found", args[0])
			}
			cfg.Storage.BasePaths = filtered
			if err := validateResolvedConfig(cfg); err != nil {
				return err
			}
			res, err := writeConfigSets(*configFile, !noBackup, []configYAMLSet{{Path: "storage.base_paths", Value: cfg.Storage.BasePaths}})
			if err != nil {
				return err
			}
			return printConfigWriteResult(cmd, res)
		},
	}
	cmd.Flags().BoolVar(&noBackup, "no-backup", false, "skip timestamped backup before replacing config")
	return cmd
}

func requireExplicitConfig(cmd *cobra.Command, configFile string) error {
	if strings.TrimSpace(configFile) == "" || !isFlagChangedInCommandTree(cmd, "config") {
		return fmt.Errorf("mutating config commands require explicit --config")
	}
	return nil
}

func isFlagChangedInCommandTree(cmd *cobra.Command, name string) bool {
	for cur := cmd; cur != nil; cur = cur.Parent() {
		if f := cur.Flags().Lookup(name); f != nil && f.Changed {
			return true
		}
		if f := cur.PersistentFlags().Lookup(name); f != nil && f.Changed {
			return true
		}
	}
	if f := cmd.InheritedFlags().Lookup(name); f != nil && f.Changed {
		return true
	}
	return false
}

func printConfigWriteResult(cmd *cobra.Command, res configWriteResult) error {
	if res.BackupPath != "" {
		if _, err := fmt.Fprintf(cmd.OutOrStdout(), "backup: %s\n", res.BackupPath); err != nil {
			return err
		}
	}
	_, err := fmt.Fprintf(cmd.OutOrStdout(), "updated: %s\nrestart filegate to apply offline config changes\n", res.Path)
	return err
}

func generateS3Credentials() (string, string, error) {
	accessBytes := make([]byte, 15)
	secretBytes := make([]byte, 32)
	if _, err := rand.Read(accessBytes); err != nil {
		return "", "", err
	}
	if _, err := rand.Read(secretBytes); err != nil {
		return "", "", err
	}
	accessKey := "FG" + base32.StdEncoding.WithPadding(base32.NoPadding).EncodeToString(accessBytes)
	secretKey := base64.RawURLEncoding.EncodeToString(secretBytes)
	return accessKey, secretKey, nil
}

func resolveS3KeyBuckets(cfg domain.Config, buckets []string, allBuckets bool) ([]string, error) {
	if allBuckets {
		if len(buckets) > 0 {
			return nil, fmt.Errorf("--bucket and --all-buckets are mutually exclusive")
		}
		return []string{"*"}, nil
	}
	if len(buckets) == 0 {
		return nil, fmt.Errorf("at least one --bucket or --all-buckets is required")
	}
	mounts := configuredMountNames(cfg.Storage.BasePaths)
	out := make([]string, 0, len(buckets))
	seen := map[string]struct{}{}
	for _, bucket := range buckets {
		bucket = strings.TrimSpace(bucket)
		if bucket == "" {
			continue
		}
		if _, ok := mounts[bucket]; !ok {
			return nil, fmt.Errorf("bucket %q is not a configured mount", bucket)
		}
		if _, ok := seen[bucket]; ok {
			continue
		}
		seen[bucket] = struct{}{}
		out = append(out, bucket)
	}
	if len(out) == 0 {
		return nil, fmt.Errorf("at least one non-empty --bucket is required")
	}
	return out, nil
}

func validateMountPathForConfig(path string) error {
	info, err := os.Stat(path)
	if err != nil {
		return err
	}
	if !info.IsDir() {
		return fmt.Errorf("mount path is not a directory: %s", path)
	}
	if runtime.GOOS != "linux" {
		return nil
	}
	health := filesystem.CheckMountHealth(path)
	if len(health.Errors) > 0 {
		return fmt.Errorf("mount health failed for %s: %s", path, joinErrs(health.Errors))
	}
	return nil
}
