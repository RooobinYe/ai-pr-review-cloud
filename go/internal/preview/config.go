// Package preview prepares bounded, namespaced preview deployments.
package preview

import (
	"crypto/sha256"
	"fmt"
	"path"
	"regexp"
	"strings"
	"time"

	"gopkg.in/yaml.v3"
)

type Config struct {
	Enabled bool `yaml:"enabled" json:"enabled"`
	Helm    struct {
		Chart      string `yaml:"chart" json:"chart"`
		ValuesFile string `yaml:"valuesFile" json:"valuesFile"`
	} `yaml:"helm" json:"helm"`
	ReadyTimeout string `yaml:"readyTimeout" json:"readyTimeout"`
}

func Parse(data []byte) (Config, error) {
	var root struct {
		Preview Config `yaml:"preview"`
	}
	if err := yaml.Unmarshal(data, &root); err != nil {
		return Config{}, err
	}
	c := root.Preview
	if !c.Enabled {
		return c, nil
	}
	if !SafePath(c.Helm.Chart) || (c.Helm.ValuesFile != "" && !SafePath(c.Helm.ValuesFile)) {
		return c, fmt.Errorf("preview paths must be relative repository paths")
	}
	if c.ReadyTimeout == "" {
		c.ReadyTimeout = "5m"
	}
	d, err := time.ParseDuration(c.ReadyTimeout)
	if err != nil || d < time.Second || d > 10*time.Minute {
		return c, fmt.Errorf("preview readyTimeout must be between 1s and 10m")
	}
	return c, nil
}

func SafePath(p string) bool {
	return p != "" && p != "." && !strings.HasPrefix(p, "/") && !strings.Contains(p, "\\") && path.Clean(p) == p && p != ".." && !strings.HasPrefix(p, "../")
}

var invalidName = regexp.MustCompile(`[^a-z0-9-]`)

// Namespace retains a hash when truncating, preventing long repository collisions.
func Namespace(repo string, number int) string {
	name := "pr-" + invalidName.ReplaceAllString(strings.ToLower(repo), "-") + fmt.Sprintf("-%d", number)
	if len(name) <= 63 {
		return name
	}
	return strings.TrimRight(name[:50], "-") + fmt.Sprintf("-%x", sha256.Sum256([]byte(name)))[:13]
}
