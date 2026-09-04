// Command s3tui is a terminal UI to browse S3 buckets, objects and object versions.
package main

import (
	"context"
	"flag"
	"fmt"
	"os"
	"os/signal"
	"syscall"

	tea "github.com/charmbracelet/bubbletea"

	"github.com/sj14/s3tui/internal/awsclient"
	"github.com/sj14/s3tui/internal/config"
	"github.com/sj14/s3tui/internal/ui"
)

// version is set at build time.
var version = "dev"

func main() {
	var (
		configPath  = flag.String("config", "", "path of the config file (default ~/.config/s3tui/config.toml or ~/.config/sss/config.toml)")
		showVersion = flag.Bool("version", false, "print the version and exit")
	)

	flag.Parse()

	if *showVersion {
		fmt.Println("s3tui", version)
		return
	}

	if err := run(*configPath); err != nil {
		fmt.Fprintln(os.Stderr, "s3tui:", err)
		os.Exit(1)
	}
}

func run(configPath string) error {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	cfg, err := config.Load(configPath)
	if err != nil {
		return err
	}

	var (
		client  *awsclient.Client
		profile config.Profile
	)

	// Which profile to use is asked in the first screen. Only a config without
	// any profile connects right away, on the environment and the AWS defaults.
	if len(cfg.Profiles) == 0 {
		profile = profile.ApplyEnv()

		client, err = awsclient.New(ctx, profile, version)
		if err != nil {
			return err
		}
	}

	program := tea.NewProgram(
		ui.New(ctx, client, "", profile, cfg, version),
		tea.WithAltScreen(),
		tea.WithContext(ctx),
	)

	if _, err := program.Run(); err != nil {
		return err
	}

	return nil
}
