package cli

import (
	"bytes"
	"fmt"
	"github.com/spf13/cobra"
	"io"
	"net/http"
	"net/url"
	"time"
)

var Version = "development"

func Execute() error { return NewRootCmd().Execute() }
func NewRootCmd() *cobra.Command {
	var config string
	cmd := &cobra.Command{Use: "filegate", Short: "Root-scoped Linux file gateway", SilenceUsage: true, SilenceErrors: true}
	cmd.PersistentFlags().StringVar(&config, "config", "/etc/filegate/conf.yaml", "Static YAML configuration")
	cmd.AddCommand(&cobra.Command{Use: "serve", Args: cobra.NoArgs, RunE: func(cmd *cobra.Command, _ []string) error {
		c, e := LoadConfig(config)
		if e != nil {
			return e
		}
		return serve(cmd.Context(), c)
	}})
	cmd.AddCommand(&cobra.Command{Use: "validate", Args: cobra.NoArgs, RunE: func(cmd *cobra.Command, _ []string) error {
		c, e := LoadConfig(config)
		if e == nil {
			_, e = c.token()
		}
		if e == nil {
			fmt.Fprintln(cmd.OutOrStdout(), "Configuration valid")
		}
		return e
	}})
	for _, item := range []struct {
		name, method, endpoint string
		root                   bool
	}{{"status", "GET", "/v1/system", false}, {"roots", "GET", "/v1/roots", false}, {"rebuild ROOT", "POST", "/index/rebuild", true}, {"prune ROOT", "POST", "/versions/prune", true}, {"stats ROOT", "POST", "/stats/refresh", true}} {
		item := item
		args := cobra.NoArgs
		if item.root {
			args = cobra.ExactArgs(1)
		}
		cmd.AddCommand(&cobra.Command{Use: item.name, Args: args, RunE: func(cmd *cobra.Command, args []string) error {
			c, e := LoadConfig(config)
			if e != nil {
				return e
			}
			token, e := c.token()
			if e != nil {
				return e
			}
			endpoint := item.endpoint
			if item.root {
				endpoint = "/v1/roots/" + url.PathEscape(args[0]) + endpoint
			}
			u := c.Server.PublicURL + endpoint
			req, e := http.NewRequestWithContext(cmd.Context(), item.method, u, bytes.NewReader(nil))
			if e != nil {
				return e
			}
			req.Header.Set("Authorization", "Bearer "+token)
			client := &http.Client{Timeout: 30 * time.Minute}
			resp, e := client.Do(req)
			if e != nil {
				return e
			}
			defer resp.Body.Close()
			_, e = io.Copy(cmd.OutOrStdout(), resp.Body)
			if e == nil && resp.StatusCode >= 300 {
				e = fmt.Errorf("daemon returned %s", resp.Status)
			}
			return e
		}})
	}
	cmd.AddCommand(&cobra.Command{Use: "version", Args: cobra.NoArgs, Run: func(cmd *cobra.Command, _ []string) { fmt.Fprintln(cmd.OutOrStdout(), Version) }})
	return cmd
}
