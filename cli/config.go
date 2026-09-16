// Package cli runs Filegate and sends administrative commands to its daemon.
package cli

import (
	"fmt"
	"github.com/k2b-dev/filegate/v4/domain"
	"gopkg.in/yaml.v3"
	"io"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"time"
)

type Config struct {
	Server struct {
		Listen         string   `yaml:"listen"`
		PublicURL      string   `yaml:"public_url"`
		AllowedOrigins []string `yaml:"allowed_origins"`
	} `yaml:"server"`
	Auth struct {
		TokenFile string `yaml:"token_file"`
	} `yaml:"auth"`
	StateDir string `yaml:"state_dir"`
	Uploads  struct {
		MaxFileSize string `yaml:"max_file_size"`
	} `yaml:"uploads"`
	Roots    []rootConfig `yaml:"roots"`
	maxBytes int64
}
type rootConfig struct {
	Name       string `yaml:"name"`
	Path       string `yaml:"path"`
	Index      bool   `yaml:"index"`
	Versioning struct {
		Enabled  bool         `yaml:"enabled"`
		Cooldown string       `yaml:"cooldown"`
		Keep     *domain.Keep `yaml:"keep"`
	} `yaml:"versioning"`
}

func LoadConfig(file string) (Config, error) {
	c := Config{}
	c.Server.Listen = "127.0.0.1:8080"
	c.Uploads.MaxFileSize = "10GiB"
	f, e := os.Open(file)
	if e != nil {
		return c, e
	}
	defer f.Close()
	d := yaml.NewDecoder(io.LimitReader(f, 1<<20))
	d.KnownFields(true)
	if e = d.Decode(&c); e != nil {
		return c, e
	}
	if e = d.Decode(&struct{}{}); e != io.EOF {
		return c, fmt.Errorf("exactly one YAML document required")
	}
	if c.Auth.TokenFile == "" || !filepath.IsAbs(c.Auth.TokenFile) {
		return c, fmt.Errorf("auth.token_file must be absolute")
	}
	if c.StateDir == "" || !filepath.IsAbs(c.StateDir) {
		return c, fmt.Errorf("state_dir must be absolute")
	}
	c.StateDir, e = canonicalPath(c.StateDir)
	if e != nil {
		return c, e
	}
	u, e := url.Parse(c.Server.PublicURL)
	if e != nil || u.Host == "" || (u.Scheme != "http" && u.Scheme != "https") || u.User != nil || u.RawQuery != "" || u.Fragment != "" || u.Path != "" && u.Path != "/" {
		return c, fmt.Errorf("server.public_url must be an HTTP(S) origin")
	}
	c.Server.PublicURL = strings.TrimRight(c.Server.PublicURL, "/")
	for _, origin := range c.Server.AllowedOrigins {
		u, e := url.Parse(origin)
		if e != nil || u.Host == "" || (u.Scheme != "https" && u.Scheme != "http") || u.Path != "" || u.RawQuery != "" || u.Fragment != "" || u.User != nil {
			return c, fmt.Errorf("invalid allowed origin")
		}
	}
	c.maxBytes, e = parseSize(c.Uploads.MaxFileSize)
	if e != nil || c.maxBytes < 1 {
		return c, fmt.Errorf("invalid uploads.max_file_size")
	}
	if len(c.Roots) == 0 {
		return c, fmt.Errorf("at least one root required")
	}
	names := map[string]bool{}
	namePattern := regexp.MustCompile(`^[a-zA-Z0-9][a-zA-Z0-9_-]{0,63}$`)
	for i := range c.Roots {
		r := &c.Roots[i]
		if !namePattern.MatchString(r.Name) || names[r.Name] {
			return c, fmt.Errorf("invalid or duplicate root name %q", r.Name)
		}
		names[r.Name] = true
		if !filepath.IsAbs(r.Path) {
			return c, fmt.Errorf("root path must be absolute")
		}
		r.Path, e = filepath.EvalSymlinks(r.Path)
		if e != nil {
			return c, e
		}
		st, e := os.Stat(r.Path)
		if e != nil || !st.IsDir() {
			return c, fmt.Errorf("root must be an existing directory")
		}
		if overlap(r.Path, c.StateDir) {
			return c, fmt.Errorf("state_dir and roots must not overlap")
		}
		for j := 0; j < i; j++ {
			if overlap(r.Path, c.Roots[j].Path) {
				return c, fmt.Errorf("roots must not overlap")
			}
		}
		if r.Versioning.Enabled && !r.Index {
			return c, fmt.Errorf("root %s: versioning requires index", r.Name)
		}
		if r.Versioning.Cooldown == "" {
			r.Versioning.Cooldown = "1m"
		}
		duration, e := time.ParseDuration(r.Versioning.Cooldown)
		if e != nil || duration < 0 {
			return c, fmt.Errorf("invalid cooldown")
		}
		if r.Versioning.Keep == nil {
			r.Versioning.Keep = &domain.Keep{Last: 10, Daily: 30, Monthly: 12}
		}
		k := r.Versioning.Keep
		for _, n := range []int{k.Last, k.Hourly, k.Daily, k.Weekly, k.Monthly} {
			if n < 0 || n > 100000 {
				return c, fmt.Errorf("retention counts must be between 0 and 100000")
			}
		}
	}
	return c, nil
}
func overlap(a, b string) bool {
	return a == b || strings.HasPrefix(a, b+string(os.PathSeparator)) || strings.HasPrefix(b, a+string(os.PathSeparator)) || a == "/" || b == "/"
}
func parseSize(s string) (int64, error) {
	for _, x := range []struct {
		suffix string
		n      int64
	}{{"GiB", 1 << 30}, {"MiB", 1 << 20}, {"KiB", 1 << 10}, {"B", 1}} {
		if strings.HasSuffix(s, x.suffix) {
			v, e := strconv.ParseInt(strings.TrimSpace(strings.TrimSuffix(s, x.suffix)), 10, 64)
			if e != nil || v < 0 || v > ((1<<63-1)/x.n) {
				return 0, fmt.Errorf("size out of range")
			}
			return v * x.n, nil
		}
	}
	return strconv.ParseInt(s, 10, 64)
}
func (c Config) token() (string, error) {
	b, e := os.ReadFile(c.Auth.TokenFile)
	if e != nil {
		return "", e
	}
	s := strings.TrimSpace(string(b))
	if len(s) < 32 || len(s) > 4096 || strings.ContainsAny(s, "\r\n") {
		return "", fmt.Errorf("token file must contain one token of 32–4096 bytes")
	}
	return s, nil
}

func canonicalPath(p string) (string, error) {
	p = filepath.Clean(p)
	suffix := []string{}
	for {
		resolved, e := filepath.EvalSymlinks(p)
		if e == nil {
			for i := len(suffix) - 1; i >= 0; i-- {
				resolved = filepath.Join(resolved, suffix[i])
			}
			return resolved, nil
		}
		if !os.IsNotExist(e) {
			return "", e
		}
		parent := filepath.Dir(p)
		if parent == p {
			return "", e
		}
		suffix = append(suffix, filepath.Base(p))
		p = parent
	}
}
