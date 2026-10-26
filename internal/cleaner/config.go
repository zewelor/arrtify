package cleaner

import (
	"errors"
	"fmt"
	"io"
	"math"
	"net/url"
	"strconv"
	"strings"
	"time"

	"go.yaml.in/yaml/v3"
)

type Duration time.Duration
type Endpoint struct {
	URL string `yaml:"url"`
}
type Rule struct {
	Name        string    `yaml:"name"`
	Categories  []string  `yaml:"categories"`
	Ratio       *float64  `yaml:"ratio"`
	MinSeedTime *Duration `yaml:"min_seed_time"`
	MaxSeedTime *Duration `yaml:"max_seed_time,omitempty"`
}
type Config struct {
	SyncTrackerLabel yamlBool    `yaml:"sync_tracker_label"`
	Transmission     Endpoint    `yaml:"transmission"`
	Sonarr           Endpoint    `yaml:"sonarr"`
	Radarr           Endpoint    `yaml:"radarr"`
	IgnoreTrackers   []string    `yaml:"ignore_trackers"`
	Rules            []Rule      `yaml:"rules"`
	Credentials      Credentials `yaml:"-" json:"-"`
}
type Credentials struct{ TransmissionUser, TransmissionPassword, SonarrKey, RadarrKey string }

// yaml/v3 accepts legacy yes/on strings for plain Go booleans. An option
// enabling writes must use a YAML boolean, including through aliases or merges.
type yamlBool bool

func (b *yamlBool) UnmarshalYAML(n *yaml.Node) error {
	if n.Tag != "!!bool" {
		return errors.New("expected a boolean (true or false)")
	}
	var value bool
	if err := n.Decode(&value); err != nil {
		return errors.New("invalid boolean")
	}
	*b = yamlBool(value)
	return nil
}

func (d *Duration) UnmarshalYAML(n *yaml.Node) error {
	if n.Tag != "!!str" || n.Value == "0" {
		return errors.New("duration requires a string with a unit")
	}
	v, err := time.ParseDuration(n.Value)
	if err != nil {
		return errors.New("invalid duration")
	}
	*d = Duration(v)
	return nil
}

func ParseConfig(r io.Reader, lookup func(string) (string, bool)) (Config, error) {
	var c Config
	// Bound untrusted configuration as well as API responses.
	b, err := io.ReadAll(io.LimitReader(r, 1<<20+1))
	if err != nil {
		return c, errors.New("cannot read configuration")
	}
	if len(b) > 1<<20 {
		return c, errors.New("configuration exceeds 1 MiB")
	}
	// Inspect nulls separately: pointer decoding otherwise treats a null maximum
	// like an omitted one. The typed decoder below enforces known fields.
	var root yaml.Node
	if err := yaml.Unmarshal(b, &root); err != nil || containsNull(&root) {
		return Config{}, errors.New("invalid YAML configuration: null values, malformed syntax or duplicate keys")
	}
	dec := yaml.NewDecoder(strings.NewReader(string(b)))
	dec.KnownFields(true)
	// YAML errors can contain scalar values (including a misplaced secret).
	if err := dec.Decode(&c); err != nil {
		return Config{}, errors.New("invalid YAML configuration: check fields, types and duplicate keys")
	}
	var extra any
	if err := dec.Decode(&extra); err != io.EOF {
		return Config{}, errors.New("configuration must contain exactly one YAML document")
	}
	for _, e := range []struct {
		name     string
		endpoint Endpoint
	}{{"transmission", c.Transmission}, {"sonarr", c.Sonarr}, {"radarr", c.Radarr}} {
		if _, err := parseEndpoint(e.endpoint.URL, e.name == "transmission"); err != nil {
			return Config{}, fmt.Errorf("%s: %w", e.name, err)
		}
	}
	if len(c.Rules) == 0 {
		return Config{}, errors.New("at least one rule is required")
	}
	names := map[string]bool{}
	for i := range c.Rules {
		rule := &c.Rules[i]
		if strings.TrimSpace(rule.Name) == "" || names[rule.Name] {
			return Config{}, fmt.Errorf("rule %d: empty or duplicate name", i+1)
		}
		names[rule.Name] = true
		if len(rule.Categories) == 0 {
			return Config{}, fmt.Errorf("rule %d: categories required", i+1)
		}
		for j, s := range rule.Categories {
			category, err := Category("/" + s)
			if err != nil || strings.Contains(s, "/") || strings.TrimSpace(s) != s {
				return Config{}, fmt.Errorf("rule %d: invalid category", i+1)
			}
			rule.Categories[j] = category
		}
		if rule.Ratio == nil || math.IsNaN(*rule.Ratio) || math.IsInf(*rule.Ratio, 0) || *rule.Ratio < 0 {
			return Config{}, fmt.Errorf("rule %d: finite nonnegative ratio required", i+1)
		}
		if rule.MinSeedTime == nil || *rule.MinSeedTime < 0 {
			return Config{}, fmt.Errorf("rule %d: nonnegative min_seed_time required", i+1)
		}
		if rule.MaxSeedTime != nil && *rule.MaxSeedTime <= 0 {
			return Config{}, fmt.Errorf("rule %d: max_seed_time must be positive", i+1)
		}
	}
	if c.IgnoreTrackers == nil {
		return Config{}, errors.New("ignore_trackers must be an explicit list (empty is allowed)")
	}
	for i, h := range c.IgnoreTrackers {
		normalized, err := Hostname(h)
		if err != nil {
			return Config{}, errors.New("invalid ignored tracker hostname")
		}
		c.IgnoreTrackers[i] = normalized
	}
	for _, entry := range []struct {
		name string
		dest *string
	}{{"TRANSMISSION_USER", &c.Credentials.TransmissionUser}, {"TRANSMISSION_PASSWORD", &c.Credentials.TransmissionPassword}, {"SONARR_API_KEY", &c.Credentials.SonarrKey}, {"RADARR_API_KEY", &c.Credentials.RadarrKey}} {
		value, ok := lookup(entry.name)
		if !ok || strings.TrimSpace(value) == "" || strings.ContainsAny(value, "\r\n") {
			return Config{}, fmt.Errorf("%s must be set and nonempty without line breaks", entry.name)
		}
		*entry.dest = value
	}
	return c, nil
}

func parseEndpoint(s string, transmission bool) (*url.URL, error) {
	u, err := url.Parse(s)
	if err != nil || u.Hostname() == "" || (u.Scheme != "http" && u.Scheme != "https") || u.User != nil || u.RawQuery != "" || u.ForceQuery || u.Fragment != "" || strings.Contains(s, "#") || u.Opaque != "" {
		return nil, errors.New("invalid service URL (http/https, no credentials, query or fragment)")
	}
	if port := u.Port(); port != "" {
		n, err := strconv.Atoi(port)
		if err != nil || n < 1 || n > 65535 {
			return nil, errors.New("invalid service URL port")
		}
	} else if strings.HasSuffix(u.Host, ":") {
		return nil, errors.New("invalid service URL port")
	}
	if _, err := Hostname(u.Hostname()); err != nil {
		return nil, errors.New("invalid service hostname")
	}
	if strings.ContainsAny(u.Path, "\x00\\") {
		return nil, errors.New("invalid service URL path")
	}
	for _, part := range strings.Split(u.Path, "/") {
		if part == "." || part == ".." {
			return nil, errors.New("invalid service URL path")
		}
	}
	if transmission && !strings.HasSuffix(u.Path, "/transmission/rpc") {
		return nil, errors.New("Transmission URL path must end in /transmission/rpc")
	}
	return u, nil
}

func containsNull(n *yaml.Node) bool {
	if n.Tag == "!!null" {
		return true
	}
	for _, child := range n.Content {
		if containsNull(child) {
			return true
		}
	}
	return false
}
