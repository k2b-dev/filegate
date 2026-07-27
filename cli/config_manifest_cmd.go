package cli

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"sort"
	"strings"
	"time"

	"github.com/spf13/cobra"
	"go.yaml.in/yaml/v3"

	apiv1 "github.com/valentinkolb/filegate/api/v1"
)

const configManifestVersion = 1

type configManifestDocument struct {
	Version int            `yaml:"version"`
	Config  map[string]any `yaml:"config"`
}

type configManifestClientOptions struct {
	file      string
	host      string
	token     string
	tokenFile string
	actor     string
	format    string
	timeout   time.Duration
}

func newConfigPlanCmd(configFile *string) *cobra.Command {
	var opts configManifestClientOptions
	cmd := &cobra.Command{
		Use:   "plan",
		Short: "Plan a complete manifest against a running Filegate",
		RunE: func(cmd *cobra.Command, _ []string) error {
			if err := validateManifestFormat(opts.format); err != nil {
				return err
			}
			values, err := readConfigManifest(opts.file)
			if err != nil {
				return err
			}
			endpoint, token, err := resolveManifestEndpoint(*configFile, opts)
			if err != nil {
				return err
			}
			var plan apiv1.ConfigManifestPlanResponse
			if err := configManifestRequest(endpoint, token, opts.actor, opts.timeout, "/v1/config/plan",
				apiv1.ConfigManifestPlanRequest{Values: values}, &plan); err != nil {
				return err
			}
			return printManifestPlan(cmd, plan, opts.format)
		},
	}
	addConfigManifestFlags(cmd, &opts)
	return cmd
}

func newConfigApplyCmd(configFile *string) *cobra.Command {
	var opts configManifestClientOptions
	cmd := &cobra.Command{
		Use:   "apply",
		Short: "Apply a complete manifest to a running Filegate",
		RunE: func(cmd *cobra.Command, _ []string) error {
			if err := validateManifestFormat(opts.format); err != nil {
				return err
			}
			values, err := readConfigManifest(opts.file)
			if err != nil {
				return err
			}
			endpoint, token, err := resolveManifestEndpoint(*configFile, opts)
			if err != nil {
				return err
			}

			var plan apiv1.ConfigManifestPlanResponse
			if err := configManifestRequest(endpoint, token, opts.actor, opts.timeout, "/v1/config/plan",
				apiv1.ConfigManifestPlanRequest{Values: values}, &plan); err != nil {
				return err
			}
			if plan.CurrentRevision == plan.ProposedRevision {
				if strings.EqualFold(opts.format, "json") {
					return json.NewEncoder(cmd.OutOrStdout()).Encode(plan)
				}
				_, err = fmt.Fprintln(cmd.OutOrStdout(), "No changes. Manifest is already applied.")
				return err
			}

			var applied apiv1.ConfigManifestApplyResponse
			if err := configManifestRequest(endpoint, token, opts.actor, opts.timeout, "/v1/config/apply",
				apiv1.ConfigManifestApplyRequest{
					Values:           values,
					ExpectedRevision: plan.CurrentRevision,
				}, &applied); err != nil {
				return err
			}
			if strings.EqualFold(opts.format, "json") {
				encoder := json.NewEncoder(cmd.OutOrStdout())
				encoder.SetIndent("", "  ")
				return encoder.Encode(applied)
			}
			if err := printManifestChanges(cmd.OutOrStdout(), applied.Changes); err != nil {
				return err
			}
			fmt.Fprintf(cmd.OutOrStdout(), "Applied manifest %s\n", shortRevision(applied.Manifest.Revision))
			if len(applied.RestartRequired) > 0 {
				fmt.Fprintf(cmd.OutOrStdout(), "%d static setting(s) require a restart.\n", len(applied.RestartRequired))
			}
			return nil
		},
	}
	addConfigManifestFlags(cmd, &opts)
	return cmd
}

func validateManifestFormat(format string) error {
	switch strings.ToLower(strings.TrimSpace(format)) {
	case "", "text", "json":
		return nil
	default:
		return fmt.Errorf("--format must be text or json")
	}
}

func addConfigManifestFlags(cmd *cobra.Command, opts *configManifestClientOptions) {
	cmd.Flags().StringVarP(&opts.file, "file", "f", "", "manifest YAML file (required)")
	cmd.Flags().StringVar(&opts.host, "host", "", "Filegate API base URL; FILEGATE_HOST is also accepted")
	cmd.Flags().StringVar(&opts.token, "token", "", "bearer token; FILEGATE_TOKEN is also accepted")
	cmd.Flags().StringVar(&opts.tokenFile, "token-file", "", "file containing the bearer token")
	cmd.Flags().StringVar(&opts.actor, "actor", "filegate-cli", "operator label recorded with the apply")
	cmd.Flags().StringVar(&opts.format, "format", "text", "output format: text or json")
	cmd.Flags().DurationVar(&opts.timeout, "timeout", 15*time.Second, "HTTP request timeout")
	_ = cmd.MarkFlagRequired("file")
}

func readConfigManifest(path string) (map[string]any, error) {
	file, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer file.Close()

	decoder := yaml.NewDecoder(io.LimitReader(file, 2<<20))
	decoder.KnownFields(true)
	var doc configManifestDocument
	if err := decoder.Decode(&doc); err != nil {
		return nil, fmt.Errorf("decode manifest: %w", err)
	}
	var extra any
	if err := decoder.Decode(&extra); !errors.Is(err, io.EOF) {
		if err == nil {
			return nil, fmt.Errorf("manifest must contain exactly one YAML document")
		}
		return nil, fmt.Errorf("decode manifest: %w", err)
	}
	if doc.Version != configManifestVersion {
		return nil, fmt.Errorf("manifest version must be %d", configManifestVersion)
	}
	if doc.Config == nil {
		return nil, fmt.Errorf("manifest config must be a mapping")
	}

	out := make(map[string]any)
	if err := flattenManifest("", doc.Config, out); err != nil {
		return nil, err
	}
	return out, nil
}

func flattenManifest(prefix string, values map[string]any, out map[string]any) error {
	for key, value := range values {
		key = strings.TrimSpace(key)
		if key == "" || strings.Contains(key, ".") {
			return fmt.Errorf("manifest key %q is invalid", key)
		}
		path := key
		if prefix != "" {
			path = prefix + "." + key
		}
		if nested, ok := value.(map[string]any); ok {
			if err := flattenManifest(path, nested, out); err != nil {
				return err
			}
			continue
		}
		out[path] = value
	}
	return nil
}

func resolveManifestEndpoint(configFile string, opts configManifestClientOptions) (string, string, error) {
	token := strings.TrimSpace(opts.token)
	if opts.tokenFile != "" {
		if token != "" {
			return "", "", fmt.Errorf("--token and --token-file are mutually exclusive")
		}
		raw, err := os.ReadFile(opts.tokenFile)
		if err != nil {
			return "", "", err
		}
		token = strings.TrimSpace(string(raw))
		if token == "" {
			return "", "", fmt.Errorf("--token-file %q is empty", opts.tokenFile)
		}
	}
	return resolveLocalEndpoint(configFile, opts.host, token, true)
}

func configManifestRequest(baseURL, token, actor string, timeout time.Duration, path string, body, out any) error {
	payload, err := json.Marshal(body)
	if err != nil {
		return err
	}
	req, err := http.NewRequest(http.MethodPost, baseURL+path, bytes.NewReader(payload))
	if err != nil {
		return err
	}
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("Content-Type", "application/json")
	if actor = strings.TrimSpace(actor); actor != "" {
		req.Header.Set("X-Filegate-Actor", actor)
	}
	resp, err := (&http.Client{Timeout: timeout}).Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	responseBody, err := io.ReadAll(io.LimitReader(resp.Body, 2<<20))
	if err != nil {
		return err
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		var apiErr apiv1.ErrorResponse
		if json.Unmarshal(responseBody, &apiErr) == nil && apiErr.Error != "" {
			return fmt.Errorf("config request failed (%d): %s", resp.StatusCode, apiErr.Error)
		}
		return fmt.Errorf("config request failed (%d): %s", resp.StatusCode, strings.TrimSpace(string(responseBody)))
	}
	if err := json.Unmarshal(responseBody, out); err != nil {
		return fmt.Errorf("decode config response: %w", err)
	}
	return nil
}

func printManifestPlan(cmd *cobra.Command, plan apiv1.ConfigManifestPlanResponse, format string) error {
	switch strings.ToLower(strings.TrimSpace(format)) {
	case "json":
		encoder := json.NewEncoder(cmd.OutOrStdout())
		encoder.SetIndent("", "  ")
		return encoder.Encode(plan)
	case "text", "":
		if plan.CurrentRevision == plan.ProposedRevision {
			_, err := fmt.Fprintln(cmd.OutOrStdout(), "No changes. Manifest is already applied.")
			return err
		}
		if err := printManifestChanges(cmd.OutOrStdout(), plan.Changes); err != nil {
			return err
		}
		fmt.Fprintf(cmd.OutOrStdout(), "Current:  %s\nProposed: %s\n", shortRevision(plan.CurrentRevision), shortRevision(plan.ProposedRevision))
		if len(plan.RestartRequired) > 0 {
			fmt.Fprintf(cmd.OutOrStdout(), "%d static setting(s) would require a restart.\n", len(plan.RestartRequired))
		}
		return nil
	default:
		return fmt.Errorf("--format must be text or json")
	}
}

func printManifestChanges(out io.Writer, changes []apiv1.ConfigManifestChange) error {
	if len(changes) == 0 {
		_, err := fmt.Fprintln(out, "No value changes.")
		return err
	}
	sorted := append([]apiv1.ConfigManifestChange(nil), changes...)
	sort.Slice(sorted, func(i, j int) bool { return sorted[i].Path < sorted[j].Path })
	for _, change := range sorted {
		prefix := map[string]string{"add": "+", "change": "~", "remove": "-"}[change.Operation]
		if prefix == "" {
			prefix = "?"
		}
		switch change.Operation {
		case "remove":
			fmt.Fprintf(out, "%s %s (%s)\n", prefix, change.Path, change.Activation)
		default:
			fmt.Fprintf(out, "%s %s = %s (%s)\n", prefix, change.Path, compactValue(change.To), change.Activation)
		}
	}
	return nil
}

func compactValue(value any) string {
	encoded, err := json.Marshal(value)
	if err != nil {
		return fmt.Sprint(value)
	}
	return string(encoded)
}

func shortRevision(revision string) string {
	if revision == "" {
		return "(none)"
	}
	if len(revision) <= 12 {
		return revision
	}
	return revision[:12]
}
