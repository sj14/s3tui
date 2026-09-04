package config

import (
	"os"
	"path/filepath"
	"testing"
)

// sample uses the config format of https://github.com/sj14/sss
const sample = `
[profiles.default]
endpoint = "https://earth.example.com"
region = "earth"
access_key = "earth-key"
secret_key = "earth-secret"

[profiles.mars]
endpoint = "https://mars.example.com"
region = "mars"
access_key = "mars-key"
secret_key = "mars-secret"
path_style = true
insecure = true
read_only = true
network = "tcp6"
bandwidth = "128 MiB"
`

func writeConfig(t *testing.T, content string) string {
	t.Helper()

	path := filepath.Join(t.TempDir(), "config.toml")
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}

	return path
}

func TestLoadProfiles(t *testing.T) {
	cfg, err := Load(writeConfig(t, sample))
	if err != nil {
		t.Fatalf("loading config: %v", err)
	}

	if len(cfg.Profiles) != 2 {
		t.Fatalf("got %d profiles, want 2", len(cfg.Profiles))
	}

	earth, ok := cfg.Profiles["default"]
	if !ok {
		t.Fatal("the first profile is missing")
	}
	if earth.Endpoint != "https://earth.example.com" || earth.Region != "earth" {
		t.Errorf("unexpected profile: %+v", earth)
	}

	mars, ok := cfg.Profiles["mars"]
	if !ok {
		t.Fatal("the second profile is missing")
	}
	if !mars.PathStyle || !mars.Insecure || !mars.ReadOnly {
		t.Errorf("unexpected flags: %+v", mars)
	}
	if mars.Network != "tcp6" || mars.Bandwidth != "128 MiB" {
		t.Errorf("unexpected network/bandwidth: %+v", mars)
	}

	if _, ok := cfg.Profiles["venus"]; ok {
		t.Error("a profile appeared out of nowhere")
	}
}

func TestLoadRejectsUnknownFields(t *testing.T) {
	if _, err := Load(writeConfig(t, "[profiles.default]\nunknown = true\n")); err == nil {
		t.Error("expected an error for an unknown field")
	}
}

func TestApplyEnv(t *testing.T) {
	t.Setenv("S3TUI_ENDPOINT", "https://override.example.com")
	t.Setenv("SSS_REGION", "sss-region")
	t.Setenv("S3TUI_PATH_STYLE", "true")

	profile := Profile{Endpoint: "https://earth.example.com", Region: "earth"}.ApplyEnv()

	if profile.Endpoint != "https://override.example.com" {
		t.Errorf("endpoint = %q", profile.Endpoint)
	}
	if profile.Region != "sss-region" {
		t.Errorf("region = %q, want the SSS_ fallback", profile.Region)
	}
	if !profile.PathStyle {
		t.Error("path_style was not overridden")
	}
}

func TestProfileWithoutConfig(t *testing.T) {
	cfg, err := Load(writeConfig(t, ""))
	if err != nil {
		t.Fatalf("loading an empty config: %v", err)
	}

	// no profiles at all is what makes s3tui connect on the AWS defaults
	if len(cfg.Profiles) != 0 {
		t.Errorf("profiles = %+v, want none", cfg.Profiles)
	}
}
