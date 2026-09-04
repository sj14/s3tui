package ui

import (
	"strings"

	"github.com/charmbracelet/bubbles/viewport"
	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"
	"github.com/charmbracelet/x/ansi"
)

// popupWidth is what the error box aims for, narrower on a narrow terminal.
const popupWidth = 72

// preparePopup sizes the box and fills it with the wrapped error. Both the
// rendering and the scrolling go through it, so no code path that sets an
// error has to know about the box; only the scroll offset lives in the model.
func (m *Model) preparePopup() int {
	width := max(24, min(popupWidth, m.width-8))
	inner := width - 4 // the border and one column of padding on each side

	wrapped := lipgloss.NewStyle().Width(inner).Render(m.err.Error())
	lines := lipgloss.Height(wrapped)

	// the chrome around the text is six lines, the rest of the screen stays
	m.popup.Width = inner
	m.popup.Height = max(1, min(lines, m.height-10))
	m.popup.SetContent(wrapped)

	return lines
}

// errorPopup renders the box with the current error.
func (m Model) errorPopup() string {
	lines := m.preparePopup()

	hint := "esc closes"
	if lines > m.popup.Height {
		hint = "↑/↓ scrolls · esc closes"
	}

	body := styleErr.Render("error") + "\n\n" +
		m.popup.View() + "\n\n" +
		styleDim.Render(hint)

	// Width counts the padding, the text itself keeps m.popup.Width columns
	return stylePopup.Width(m.popup.Width + 2).Render(body)
}

// handleErrorPopup scrolls the box, every other key acknowledges the error.
func (m Model) handleErrorPopup(msg tea.KeyMsg) (tea.Model, tea.Cmd) {
	switch msg.String() {
	case "up", "k", "down", "j", "pgup", "pgdown", "home", "end", "g", "G":
		m.preparePopup()

		var cmd tea.Cmd

		m.popup, cmd = m.popup.Update(msg)

		return m, cmd
	}

	m.err = nil
	m.popup = viewport.New(0, 0)

	return m, nil
}

// overlay draws box over the middle of a screen of the given width and
// returns the result. Both are blocks of terminal lines; the background stays
// where the box does not cover it.
func overlay(background, box string, width int) string {
	lines := strings.Split(background, "\n")
	boxLines := strings.Split(box, "\n")

	if len(boxLines) > len(lines) {
		return box
	}

	boxWidth := lipgloss.Width(box)

	top := (len(lines) - len(boxLines)) / 2
	left := max(0, (width-boxWidth)/2)

	for i, boxLine := range boxLines {
		line := lines[top+i]

		// the box needs ground under it, short lines are padded first
		if pad := left + boxWidth - lipgloss.Width(line); pad > 0 {
			line += strings.Repeat(" ", pad)
		}

		before := ansi.Truncate(line, left, "")
		if strings.ContainsRune(before, ansi.ESC) {
			before += "\x1b[0m" // the cut may sit inside a styled run
		}

		lines[top+i] = before + boxLine + ansi.TruncateLeft(line, left+boxWidth, "")
	}

	return strings.Join(lines, "\n")
}
