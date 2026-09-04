package ui

import (
	"context"
	"sort"

	"github.com/charmbracelet/bubbles/list"
	tea "github.com/charmbracelet/bubbletea"

	"github.com/sj14/s3tui/internal/awsclient"
	"github.com/sj14/s3tui/internal/config"
)

// profileItem is one entry of the profile switcher.
type profileItem struct {
	name    string
	profile config.Profile
	active  bool
}

func (i profileItem) FilterValue() string { return i.name }

func (i profileItem) row(width int, selected bool) string {
	target := i.profile.Endpoint
	if target == "" {
		target = "aws"
	}

	suffix := ""
	if i.active {
		suffix = " [active]"
	}

	return renderRow(width, selected, false, i.name, suffix,
		cell(target, 34, false),
		cell(i.profile.Region, 16, false),
	)
}

// profileItems returns all configured profiles, sorted by name.
func profileItems(cfg config.Config, active string) []list.Item {
	names := make([]string, 0, len(cfg.Profiles))
	for name := range cfg.Profiles {
		names = append(names, name)
	}

	sort.Strings(names)

	items := make([]list.Item, 0, len(names))
	for _, name := range names {
		items = append(items, profileItem{
			name:    name,
			profile: cfg.Profiles[name],
			active:  name == active,
		})
	}

	return items
}

// profileSwitchedMsg carries the client of the newly selected profile.
type profileSwitchedMsg struct {
	name    string
	profile config.Profile
	client  *awsclient.Client
}

// switchProfileCmd builds the client of the given profile.
func switchProfileCmd(ctx context.Context, name string, profile config.Profile, userAgent string) tea.Cmd {
	return func() tea.Msg {
		profile = profile.ApplyEnv()

		client, err := awsclient.New(ctx, profile, userAgent)
		if err != nil {
			return fail("switching to profile "+name, err)
		}

		return profileSwitchedMsg{name: name, profile: profile, client: client}
	}
}
