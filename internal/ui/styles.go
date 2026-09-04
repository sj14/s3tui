package ui

import "github.com/charmbracelet/lipgloss"

var (
	colorAccent = lipgloss.AdaptiveColor{Light: "#7D56F4", Dark: "#B197FC"}
	colorDim    = lipgloss.AdaptiveColor{Light: "#6C6C6C", Dark: "#8A8A8A"}
	colorErr    = lipgloss.AdaptiveColor{Light: "#B00020", Dark: "#FF6B6B"}
	colorOK     = lipgloss.AdaptiveColor{Light: "#207D3A", Dark: "#77DD77"}

	styleApp       = lipgloss.NewStyle().Bold(true).Foreground(colorAccent)
	styleDim       = lipgloss.NewStyle().Foreground(colorDim)
	styleErr       = lipgloss.NewStyle().Foreground(colorErr)
	styleOK        = lipgloss.NewStyle().Foreground(colorOK)
	styleCrumb     = lipgloss.NewStyle().Bold(true)
	styleSelected  = lipgloss.NewStyle().Foreground(colorAccent).Bold(true)
	styleAccent    = lipgloss.NewStyle().Foreground(colorAccent).Bold(true)
	styleWarn      = lipgloss.NewStyle().Foreground(lipgloss.AdaptiveColor{Light: "#A15C00", Dark: "#FFC46B"})
	stylePrefixRow = lipgloss.NewStyle().Foreground(lipgloss.AdaptiveColor{Light: "#1C6FD0", Dark: "#79B8FF"})
	styleHeader    = lipgloss.NewStyle().Padding(0, 1)
	styleFooter    = lipgloss.NewStyle().Padding(0, 1)
	stylePopup     = lipgloss.NewStyle().Border(lipgloss.RoundedBorder()).BorderForeground(colorErr).Padding(0, 1)
)
