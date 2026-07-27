package cli

import (
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"os"
	"strings"
	"time"

	"github.com/spf13/cobra"
)

func newHealthCmd() *cobra.Command {
	var host string
	var configFile string
	var timeout time.Duration

	cmd := &cobra.Command{
		Use:   "health",
		Short: "Check Filegate health endpoint",
		RunE: func(_ *cobra.Command, _ []string) error {
			baseURL, _, err := resolveLocalEndpoint(configFile, host, "", false)
			if err != nil {
				return err
			}
			client := &http.Client{Timeout: timeout}
			req, err := http.NewRequest(http.MethodGet, baseURL+"/health", nil)
			if err != nil {
				return err
			}
			resp, err := client.Do(req)
			if err != nil {
				return err
			}
			defer resp.Body.Close()
			body, _ := io.ReadAll(io.LimitReader(resp.Body, 2048))
			fmt.Printf("status=%d body=%s\n", resp.StatusCode, strings.TrimSpace(string(body)))
			if resp.StatusCode < 200 || resp.StatusCode >= 300 {
				return fmt.Errorf("health check failed")
			}
			return nil
		},
	}
	cmd.Flags().StringVar(&host, "host", "", "api base host/url override")
	cmd.Flags().StringVar(&configFile, "config", "", "path to config file")
	cmd.Flags().DurationVar(&timeout, "timeout", 10*time.Second, "request timeout")
	return cmd
}

func newStatusCmd() *cobra.Command {
	var host string
	var token string
	var configFile string
	var timeout time.Duration

	cmd := &cobra.Command{
		Use:   "status",
		Short: "Fetch Filegate runtime status (/v1/stats)",
		RunE: func(_ *cobra.Command, _ []string) error {
			baseURL, bearer, err := resolveLocalEndpoint(configFile, host, token, true)
			if err != nil {
				return err
			}
			client := &http.Client{Timeout: timeout}
			req, err := http.NewRequest(http.MethodGet, baseURL+"/v1/stats", nil)
			if err != nil {
				return err
			}
			req.Header.Set("Authorization", "Bearer "+bearer)
			resp, err := client.Do(req)
			if err != nil {
				return err
			}
			defer resp.Body.Close()
			payload, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
			if resp.StatusCode < 200 || resp.StatusCode >= 300 {
				return fmt.Errorf("status request failed: %d %s", resp.StatusCode, strings.TrimSpace(string(payload)))
			}
			var pretty any
			if err := json.Unmarshal(payload, &pretty); err != nil {
				return fmt.Errorf("invalid status payload: %w", err)
			}
			formatted, err := json.MarshalIndent(pretty, "", "  ")
			if err != nil {
				return err
			}
			fmt.Printf("%s\n", formatted)
			return nil
		},
	}
	cmd.Flags().StringVar(&host, "host", "", "api base host/url override")
	cmd.Flags().StringVar(&token, "token", "", "bearer token override")
	cmd.Flags().StringVar(&configFile, "config", "", "path to config file")
	cmd.Flags().DurationVar(&timeout, "timeout", 10*time.Second, "request timeout")
	return cmd
}

func resolveLocalEndpoint(configFile, host, token string, needToken bool) (string, string, error) {
	baseURL := strings.TrimSpace(host)
	bearer := strings.TrimSpace(token)
	if baseURL == "" {
		baseURL = strings.TrimSpace(os.Getenv("FILEGATE_HOST"))
	}
	if bearer == "" {
		bearer = strings.TrimSpace(os.Getenv("FILEGATE_TOKEN"))
	}

	needsCfg := baseURL == "" || (needToken && bearer == "")
	if needsCfg {
		cfg, err := loadConfig(configFile)
		if err != nil {
			return "", "", err
		}
		if baseURL == "" {
			baseURL, err = baseURLFromListen(cfg.Server.Listen)
			if err != nil {
				return "", "", err
			}
		}
		if needToken && bearer == "" {
			bearer = strings.TrimSpace(cfg.Auth.BearerToken)
		}
	}

	var err error
	baseURL, err = normalizeBaseURL(baseURL)
	if err != nil {
		return "", "", err
	}
	if needToken && bearer == "" {
		return "", "", fmt.Errorf("token required for status endpoint")
	}
	return baseURL, bearer, nil
}

func baseURLFromListen(listen string) (string, error) {
	addr := strings.TrimSpace(listen)
	if addr == "" {
		return "", fmt.Errorf("server.listen is empty")
	}
	if strings.HasPrefix(addr, "http://") || strings.HasPrefix(addr, "https://") {
		return normalizeBaseURL(addr)
	}
	if strings.HasPrefix(addr, ":") {
		addr = "127.0.0.1" + addr
	}
	return normalizeBaseURL(addr)
}

func normalizeBaseURL(raw string) (string, error) {
	val := strings.TrimSpace(raw)
	if val == "" {
		return "", fmt.Errorf("host is empty")
	}
	if !strings.HasPrefix(val, "http://") && !strings.HasPrefix(val, "https://") {
		val = "http://" + val
	}
	u, err := url.Parse(val)
	if err != nil {
		return "", err
	}
	if u.Host == "" {
		return "", fmt.Errorf("invalid host %q", raw)
	}

	host := u.Host
	if h, p, err := net.SplitHostPort(host); err == nil {
		nh := normalizeHost(h)
		host = net.JoinHostPort(nh, p)
	} else {
		host = normalizeHost(host)
	}
	u.Host = host
	u.Path = ""
	u.RawPath = ""
	u.RawQuery = ""
	u.Fragment = ""
	return strings.TrimRight(u.String(), "/"), nil
}

func normalizeHost(host string) string {
	h := strings.TrimSpace(host)
	h = strings.TrimPrefix(h, "[")
	h = strings.TrimSuffix(h, "]")
	switch h {
	case "", "0.0.0.0", "::":
		return "127.0.0.1"
	default:
		return h
	}
}
