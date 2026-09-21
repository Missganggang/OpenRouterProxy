package nodeclient

import (
	"bytes"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"net/url"
	"os"
	"path/filepath"
	"strings"

	"github.com/openroute/openroute/internal/nodeproto"
	"gopkg.in/yaml.v3"
)

// Config reads the same config.yml that the panel's installer creates.
type Config struct {
	BaseURL        string                      `yaml:"base-url"`
	Token          string                      `yaml:"token"`
	MachineID      string                      `yaml:"machine-id"`
	DataDir        string                      `yaml:"-"`
	BindInbound    string                      `yaml:"-"`
	CountInterface string                      `yaml:"-"`
	DisableExecute bool                        `yaml:"disable-execute"`
	ConfigPath     string                      `yaml:"-"`
	Network        nodeproto.NodeNetworkConfig `yaml:",inline"`
}

// LoadConfig allows CLI credentials to override the file for old systemd units.
func LoadConfig(path, baseURL, token string) (Config, error) {
	return LoadConfigWithOverrides(path, baseURL, token, nil)
}

// ConfigOverrides contains only explicitly supplied command-line values. Keeping
// presence separate from a value allows zero ports and an empty host to clear a
// previous override without resetting fields the caller did not specify.
type ConfigOverrides map[string]any

func LoadConfigWithOverrides(path, baseURL, token string, overrides ConfigOverrides) (Config, error) {
	cfg, _, err := prepareConfig(path, baseURL, token, overrides)
	return cfg, err
}

// WriteConfig merges an existing YAML document and writes a private, atomic
// replacement. Unknown settings and comments survive installer upgrades.
func WriteConfig(path, target, baseURL, token string, overrides ConfigOverrides) error {
	cfg, doc, err := prepareConfig(path, baseURL, token, overrides)
	if err != nil {
		return err
	}
	if strings.TrimSpace(target) == "" {
		return errors.New("configuration output path is required")
	}
	root := doc.Content[0]
	for key, value := range map[string]any{"base-url": cfg.BaseURL, "token": cfg.Token} {
		if err := setConfigValue(root, key, value); err != nil {
			return err
		}
	}
	if cfg.MachineID != "" {
		if err := setConfigValue(root, "machine-id", cfg.MachineID); err != nil {
			return err
		}
	}
	if _, ok := overrides["connect-host"]; ok {
		if err := setConfigValue(root, "connect-host", cfg.Network.ConnectHost); err != nil {
			return err
		}
	}
	data, err := yaml.Marshal(doc)
	if err != nil {
		return errors.New("cannot encode node configuration")
	}
	return atomicWrite(target, data)
}

func prepareConfig(path, baseURL, token string, overrides ConfigOverrides) (Config, *yaml.Node, error) {
	var cfg Config
	data, err := os.ReadFile(path)
	if err != nil && !(os.IsNotExist(err) && baseURL != "" && token != "") {
		return cfg, nil, fmt.Errorf("read node configuration: %w", err)
	}
	doc := &yaml.Node{Kind: yaml.DocumentNode, Content: []*yaml.Node{{Kind: yaml.MappingNode, Tag: "!!map"}}}
	if len(bytes.TrimSpace(data)) != 0 {
		decoder := yaml.NewDecoder(bytes.NewReader(data))
		if err := decoder.Decode(doc); err != nil {
			return cfg, nil, errors.New("invalid YAML in node configuration")
		}
		var extra yaml.Node
		if err := decoder.Decode(&extra); err != io.EOF {
			return cfg, nil, errors.New("node configuration must contain one YAML document")
		}
	}
	if len(doc.Content) != 1 || doc.Content[0].Kind != yaml.MappingNode {
		return cfg, nil, errors.New("node configuration must be a YAML mapping")
	}
	// Decode before merging too, so an override cannot hide malformed fields or
	// duplicate keys in the existing file.
	if err := doc.Decode(&cfg); err != nil {
		return cfg, nil, errors.New("invalid YAML fields in node configuration")
	}
	for key, value := range overrides {
		switch key {
		case "connect-host", "direct-port", "ws-port", "tls-port", "udp-port", "rev-port",
			"connect-direct-port", "connect-ws-port", "connect-tls-port", "connect-udp-port", "connect-rev-port", "is-outbound":
		default:
			return cfg, nil, fmt.Errorf("unknown node configuration override %q", key)
		}
		if err := setConfigValue(doc.Content[0], key, value); err != nil {
			return cfg, nil, err
		}
	}
	if err := doc.Decode(&cfg); err != nil {
		return cfg, nil, errors.New("invalid YAML fields in node configuration")
	}
	if baseURL != "" {
		cfg.BaseURL = baseURL
	}
	if token != "" {
		cfg.Token = token
	}
	cfg.BaseURL = strings.TrimRight(strings.TrimSpace(cfg.BaseURL), "/")
	cfg.Token = strings.TrimSpace(cfg.Token)
	u, err := url.Parse(cfg.BaseURL)
	if err != nil || u == nil || u.Host == "" || (u.Scheme != "http" && u.Scheme != "https") || u.User != nil || u.RawQuery != "" || u.Fragment != "" {
		return cfg, nil, errors.New("base-url must be an http(s) URL without credentials, query or fragment")
	}
	if cfg.Token == "" || strings.ContainsAny(cfg.Token, "\r\n") {
		return cfg, nil, errors.New("a valid node token is required")
	}
	if err := cfg.Network.Normalize(); err != nil {
		return cfg, nil, err
	}
	cfg.DataDir, err = filepath.Abs(filepath.Dir(path))
	if err != nil {
		return cfg, nil, err
	}
	cfg.BindInbound = os.Getenv("BIND_INBOUND")
	cfg.CountInterface = os.Getenv("COUNT_INTERFACE")
	cfg.ConfigPath, _ = filepath.Abs(path)
	if v := strings.ToLower(strings.TrimSpace(os.Getenv("DISABLE_EXECUTE"))); v == "1" || v == "true" || v == "yes" {
		cfg.DisableExecute = true
	}
	if id := strings.TrimSpace(os.Getenv("UUID")); id != "" {
		cfg.MachineID = id
	}
	if err := ValidateNetworkEnvironment(cfg.BindInbound); err != nil {
		return cfg, nil, err
	}
	return cfg, doc, nil
}

func setConfigValue(root *yaml.Node, key string, value any) error {
	var node yaml.Node
	if err := node.Encode(value); err != nil {
		return fmt.Errorf("cannot encode node configuration field %q", key)
	}
	for i := 0; i+1 < len(root.Content); i += 2 {
		if root.Content[i].Value == key {
			previous := root.Content[i+1]
			node.HeadComment, node.LineComment, node.FootComment = previous.HeadComment, previous.LineComment, previous.FootComment
			node.Anchor = previous.Anchor
			*previous = node
			return nil
		}
	}
	root.Content = append(root.Content, &yaml.Node{Kind: yaml.ScalarNode, Tag: "!!str", Value: key}, &node)
	return nil
}

func (c *Config) ensureIdentity() error {
	if c.MachineID != "" {
		return nil
	}
	path := filepath.Join(c.DataDir, "machine-id")
	data, err := os.ReadFile(path)
	if err == nil && strings.TrimSpace(string(data)) != "" {
		c.MachineID = strings.TrimSpace(string(data))
		return nil
	}
	if err != nil && !os.IsNotExist(err) {
		return err
	}
	var id [16]byte
	if _, err := rand.Read(id[:]); err != nil {
		return err
	}
	id[6] = (id[6] & 0x0f) | 0x40
	id[8] = (id[8] & 0x3f) | 0x80
	s := hex.EncodeToString(id[:])
	c.MachineID = s[:8] + "-" + s[8:12] + "-" + s[12:16] + "-" + s[16:20] + "-" + s[20:]
	return atomicWrite(path, []byte(c.MachineID+"\n"))
}

func atomicWrite(path string, data []byte) error {
	f, err := os.CreateTemp(filepath.Dir(path), ".openroute-*")
	if err != nil {
		return err
	}
	name := f.Name()
	defer os.Remove(name)
	if err = f.Chmod(0600); err == nil {
		_, err = f.Write(data)
	}
	if err == nil {
		err = f.Sync()
	}
	if closeErr := f.Close(); err == nil {
		err = closeErr
	}
	if err != nil {
		return err
	}
	if err := os.Rename(name, path); err != nil {
		return err
	}
	return syncDirectory(filepath.Dir(path))
}
