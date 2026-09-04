package ui

import (
	"errors"
	"strings"
	"testing"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"
	"github.com/charmbracelet/x/ansi"
)

// styled builds a background line the way lipgloss would: a colored left half
// and a colored right half. Colors are off in tests, so the escapes are
// written by hand here.
func styled(left, right string) string {
	return "\x1b[31m" + left + "\x1b[0m" + "\x1b[34m" + right + "\x1b[0m"
}

func TestOverlayKeepsTheBackgroundAroundTheBox(t *testing.T) {
	background := make([]string, 9)
	for i := range background {
		background[i] = styled(strings.Repeat("L", 30), strings.Repeat("R", 30))
	}

	box := "╭────────────╮\n│ error      │\n╰────────────╯"
	out := overlay(strings.Join(background, "\n"), box, 60)

	lines := strings.Split(out, "\n")
	if len(lines) != len(background) {
		t.Fatalf("the overlay changed the number of lines: %d, want %d", len(lines), len(background))
	}

	// the box sits in the middle, the background keeps the rest of the line
	if got := ansi.Strip(lines[4]); got != strings.Repeat("L", 23)+"│ error      │"+strings.Repeat("R", 23) {
		t.Errorf("line 4 = %q", got)
	}
	if got := ansi.Strip(lines[0]); got != strings.Repeat("L", 30)+strings.Repeat("R", 30) {
		t.Errorf("an uncovered line was touched: %q", got)
	}

	for i, line := range lines {
		if got := lipgloss.Width(line); got != 60 {
			t.Errorf("line %d is %d columns wide, want 60", i, got)
		}
	}

	// the part right of the box keeps its color instead of bleeding
	if !strings.Contains(lines[4], "\x1b[34m"+strings.Repeat("R", 23)) {
		t.Errorf("the right side lost its style: %q", lines[4])
	}
}

func TestOverlayPadsShortBackgroundLines(t *testing.T) {
	// nine lines the box could land on, all of them shorter than it
	background := strings.Repeat("short\n", 8) + "short"
	box := "╭──╮\n│ok│\n╰──╯"

	lines := strings.Split(overlay(background, box, 40), "\n")

	// the box is centered, so it covers the three lines in the middle
	for i, want := range map[int]string{3: "╭──╮", 4: "│ok│", 5: "╰──╯"} {
		if got := ansi.Strip(lines[i]); !strings.HasSuffix(got, want) {
			t.Errorf("line %d = %q, want it to end in %q", i, got, want)
		}
		if got := lipgloss.Width(lines[i]); got != 22 {
			t.Errorf("line %d is %d columns wide, want the box to reach column 22", i, got)
		}
	}
}

func TestErrorBoxFitsTheScreen(t *testing.T) {
	server := fakeS3(t)

	for _, size := range []tea.WindowSizeMsg{{Width: 100, Height: 24}, {Width: 40, Height: 12}, {Width: 200, Height: 60}} {
		model := newTestModel(t, server.URL)
		model = step(t, model, size)
		model = step(t, model, errMsg{
			what: "listing objects",
			err:  errors.New(strings.Repeat("a long message that has to be wrapped somewhere. ", 40)),
		})

		inner := model.(Model)
		box := inner.errorPopup()

		if got := lipgloss.Width(box); got > size.Width {
			t.Errorf("%dx%d: the box is %d columns wide", size.Width, size.Height, got)
		}
		if got := lipgloss.Height(box); got > size.Height {
			t.Errorf("%dx%d: the box is %d lines tall", size.Width, size.Height, got)
		}

		view := model.View()

		quiet := inner
		quiet.err = nil

		if got, want := lipgloss.Height(view), lipgloss.Height(quiet.View()); got != want {
			t.Errorf("%dx%d: the box changed the height of the screen: %d instead of %d",
				size.Width, size.Height, got, want)
		}
		if !strings.Contains(view, "esc closes") {
			t.Errorf("%dx%d: no error box:\n%s", size.Width, size.Height, view)
		}
	}
}
