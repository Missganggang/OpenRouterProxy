package nodeclient

import (
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"strings"

	"gopkg.in/yaml.v3"
)

// Config reads the same config.yml that the panel's installer creates.
type Config struct {
	BaseURL        string `yaml:"base-url"`
	Token          string `yaml:"token"`
	MachineID      string `yaml:"machine-id"`
	DataDir        string `yaml:"-"`
	BindInbound    string `yaml:"-"`
	CountInterface string `yaml:"-"`
}

// LoadConfig allows CLI credentials to override the file for old systemd units.
func LoadConfig(path, baseURL, token string) (Config, error) {
	var cfg Config
	data, err := os.ReadFile(path)
	if err != nil && !(os.IsNotExist(err) && baseURL != "" && token != "") {
		return cfg, fmt.Errorf("read node configuration: %w", err)
	}
	if len(data) != 0 {
		if err := yaml.Unmarshal(data, &cfg); err != nil {
			return cfg, errors.New("invalid YAML in node configuration")
		}
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
		return cfg, errors.New("base-url must be an http(s) URL without credentials, query or fragment")
	}
	if cfg.Token == "" || strings.ContainsAny(cfg.Token, "\r\n") {
		return cfg, errors.New("a valid node token is required")
	}
	cfg.DataDir, err = filepath.Abs(filepath.Dir(path))
	if err != nil {
		return cfg, err
	}
	cfg.BindInbound = os.Getenv("BIND_INBOUND")
	cfg.CountInterface = os.Getenv("COUNT_INTERFACE")
	if id := strings.TrimSpace(os.Getenv("UUID")); id != "" {
		cfg.MachineID = id
	}
	return cfg, nil
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
	return os.Rename(name, path)
}
