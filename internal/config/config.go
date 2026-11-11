// Package config loads the service configuration.
package config

import (
	"errors"
	"fmt"
	"os"
	"strings"
	"time"

	"gopkg.in/yaml.v3"

	"github.com/conallob/unifi-power-controller/internal/power"
)

// Config is the on-disk (YAML) configuration.
type Config struct {
	Listen string `yaml:"listen"`
	// MinOffTime is the minimum time PoE stays off before being re-enabled.
	// Defaults to 5s; set to 0s to disable.
	MinOffTime *time.Duration `yaml:"minOffTime"`
	UniFi      UniFi          `yaml:"unifi"`
	RPC        RPC            `yaml:"rpc"`
	// Switches are named so several nodes can share one.
	Switches map[string]Switch `yaml:"switches"`
	Nodes    map[string]Node   `yaml:"nodes"`
}

// UniFi describes how to reach the controller.
type UniFi struct {
	URL  string `yaml:"url"`
	Site string `yaml:"site"`
	// APIKeyFile is read at startup. UNIFI_API_KEY takes precedence if set.
	APIKeyFile string `yaml:"apiKeyFile"`
	// InsecureSkipVerify disables TLS verification; controllers commonly use
	// a self-signed certificate on the LAN.
	InsecureSkipVerify bool          `yaml:"insecureSkipVerify"`
	Timeout            time.Duration `yaml:"timeout"`

	APIKey string `yaml:"-"`
}

// RPC configures the bmclib/Rufio-compatible endpoint (POST /rpc).
type RPC struct {
	// HMACSHA256Files / HMACSHA512Files list files each containing one
	// shared secret. When any are set, requests must carry a matching
	// X-BMCLIB-Signature-256 / -512 header.
	HMACSHA256Files []string `yaml:"hmacSHA256Files"`
	HMACSHA512Files []string `yaml:"hmacSHA512Files"`
	// SignedHeaders are request headers whose values are appended to the body
	// before signing. Must mirror Rufio's signature.includedPayloadHeaders.
	SignedHeaders []string `yaml:"signedHeaders"`

	SHA256Secrets []string `yaml:"-"`
	SHA512Secrets []string `yaml:"-"`
}

// Switch is a UniFi switch identified by MAC address.
type Switch struct {
	MAC string `yaml:"mac"`
}

// Node maps a host to a PoE port on a named switch.
type Node struct {
	Switch string `yaml:"switch"`
	Port   int64  `yaml:"port"`
}

// Load reads and validates the YAML file at path, then resolves secrets.
func Load(path string) (*Config, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	var c Config
	dec := yaml.NewDecoder(strings.NewReader(string(b)))
	dec.KnownFields(true)
	if err := dec.Decode(&c); err != nil {
		return nil, fmt.Errorf("parse %s: %w", path, err)
	}
	if err := c.finalize(); err != nil {
		return nil, err
	}
	return &c, nil
}

func (c *Config) finalize() error {
	if c.Listen == "" {
		c.Listen = ":8080"
	}
	if c.MinOffTime == nil {
		d := 5 * time.Second
		c.MinOffTime = &d
	}
	if c.UniFi.Site == "" {
		c.UniFi.Site = "default"
	}
	if c.UniFi.Timeout == 0 {
		c.UniFi.Timeout = 30 * time.Second
	}
	if c.UniFi.URL == "" {
		return errors.New("unifi.url is required")
	}
	if len(c.Nodes) == 0 {
		return errors.New("at least one node is required")
	}
	for name, sw := range c.Switches {
		if sw.MAC == "" {
			return fmt.Errorf("switch %q: mac is required", name)
		}
	}
	for name, n := range c.Nodes {
		if _, ok := c.Switches[n.Switch]; !ok {
			return fmt.Errorf("node %q: unknown switch %q", name, n.Switch)
		}
		if n.Port < 1 {
			return fmt.Errorf("node %q: port must be >= 1", name)
		}
	}

	c.UniFi.APIKey = strings.TrimSpace(os.Getenv("UNIFI_API_KEY"))
	if c.UniFi.APIKey == "" && c.UniFi.APIKeyFile != "" {
		s, err := readSecret(c.UniFi.APIKeyFile)
		if err != nil {
			return err
		}
		c.UniFi.APIKey = s
	}
	if c.UniFi.APIKey == "" {
		return errors.New("no UniFi API key: set UNIFI_API_KEY or unifi.apiKeyFile")
	}

	var err error
	if c.RPC.SHA256Secrets, err = readSecrets(c.RPC.HMACSHA256Files); err != nil {
		return err
	}
	if c.RPC.SHA512Secrets, err = readSecrets(c.RPC.HMACSHA512Files); err != nil {
		return err
	}
	return nil
}

// PowerTargets resolves node names to switch/port targets.
func (c *Config) PowerTargets() map[string]power.Target {
	out := make(map[string]power.Target, len(c.Nodes))
	for name, n := range c.Nodes {
		out[name] = power.Target{SwitchMAC: power.NormalizeMAC(c.Switches[n.Switch].MAC), Port: n.Port}
	}
	return out
}

func readSecrets(paths []string) ([]string, error) {
	var out []string
	for _, p := range paths {
		s, err := readSecret(p)
		if err != nil {
			return nil, err
		}
		out = append(out, s)
	}
	return out, nil
}

func readSecret(path string) (string, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return "", fmt.Errorf("read secret: %w", err)
	}
	s := strings.TrimSpace(string(b))
	if s == "" {
		return "", fmt.Errorf("secret file %s is empty", path)
	}
	return s, nil
}
