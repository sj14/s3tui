// Package config loads the TOML configuration.
//
// The file format is identical to https://github.com/sj14/sss, so an existing
// sss config can be used as is.
package config

import (
	"fmt"
	"os"
	"path/filepath"
	"strconv"

	"github.com/BurntSushi/toml"
)

// Config is the top level of the configuration file.
type Config struct {
	Profiles map[string]Profile `toml:"profiles"`
}

// Profile holds the connection settings of a single S3 endpoint.
type Profile struct {
	Endpoint  string `toml:"endpoint"`
	Region    string `toml:"region"`
	AccessKey string `toml:"access_key"`
	SecretKey string `toml:"secret_key"`
	PathStyle bool   `toml:"path_style"`
	Insecure  bool   `toml:"insecure"`
	ReadOnly  bool   `toml:"read_only"`
	SNI       string `toml:"sni"`
	Network   string `toml:"network"`
	Bandwidth string `toml:"bandwidth"`
}

// DefaultConfigPath returns the first existing default location: the s3tui
// config, falling back to the sss config.
func DefaultConfigPath() (string, error) {
	homeDir, err := os.UserHomeDir()
	if err != nil {
		return "", err
	}

	own := filepath.Join(homeDir, ".config", "s3tui", "config.toml")
	if _, err := os.Stat(own); err == nil {
		return own, nil
	}

	sss := filepath.Join(homeDir, ".config", "sss", "config.toml")
	if _, err := os.Stat(sss); err == nil {
		return sss, nil
	}

	return own, nil
}

// Load reads the config from configPath. When configPath is empty, the default
// locations are used and a missing file is not an error.
func Load(configPath string) (Config, error) {
	var (
		config Config
		err    error
	)

	if configPath == "" {
		configPath = env("CONFIG")
	}

	if configPath == "" {
		configPath, err = DefaultConfigPath()
		if err != nil {
			return config, err
		}

		// prevent failing when the default config does not exist
		_, err := os.Stat(configPath)
		if os.IsNotExist(err) {
			return config, nil
		}
		if err != nil {
			return config, err
		}
	}

	md, err := toml.DecodeFile(configPath, &config)
	if err != nil {
		return config, err
	}

	if undecoded := md.Undecoded(); len(undecoded) > 0 {
		return config, fmt.Errorf("unknown fields in config: %v", undecoded)
	}

	return config, nil
}

// ApplyEnv overrides the profile with the environment variables.
func (p Profile) ApplyEnv() Profile {
	str := func(target *string, key string) {
		if v := env(key); v != "" {
			*target = v
		}
	}
	boolean := func(target *bool, key string) {
		v := env(key)
		if v == "" {
			return
		}
		parsed, err := strconv.ParseBool(v)
		if err != nil {
			return
		}
		*target = parsed
	}

	str(&p.Endpoint, "ENDPOINT")
	str(&p.Region, "REGION")
	str(&p.AccessKey, "ACCESS_KEY")
	str(&p.SecretKey, "SECRET_KEY")
	str(&p.SNI, "SNI")
	str(&p.Network, "NETWORK")
	str(&p.Bandwidth, "BANDWIDTH")
	boolean(&p.PathStyle, "PATH_STYLE")
	boolean(&p.Insecure, "INSECURE")
	boolean(&p.ReadOnly, "READ_ONLY")

	return p
}

// env looks up S3TUI_<key> and falls back to SSS_<key>.
func env(key string) string {
	if v := os.Getenv("S3TUI_" + key); v != "" {
		return v
	}
	return os.Getenv("SSS_" + key)
}
