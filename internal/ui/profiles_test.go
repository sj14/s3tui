package ui

import (
	"strings"
	"testing"

	"github.com/sj14/s3tui/internal/config"
)

func TestProfileRowsPreserveNamesOnNarrowScreens(t *testing.T) {
	cfg := config.Config{Profiles: map[string]config.Profile{
		"backblaze": {
			Endpoint: "https://s3.eu-central-003.backblazeb2.com",
			Region:   "eu-central-003",
		},
		"default": {
			Endpoint: "https://hel1.your-objectstorage.com",
			Region:   "hel1",
		},
	}}

	for _, item := range profileItems(cfg, "") {
		profile := item.(profileItem)
		row := profile.row(58, false)
		if !strings.Contains(row, profile.name) {
			t.Errorf("row truncates profile name %q:\n%s", profile.name, row)
		}
		if strings.Contains(row, profile.profile.Endpoint) {
			t.Errorf("row contains endpoint:\n%s", row)
		}
	}
}
