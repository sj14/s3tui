package ui

import (
	"github.com/charmbracelet/bubbles/textinput"
)

type promptKind int

const (
	promptDownload promptKind = iota
	promptCopy
	promptMove
	promptSearch
	promptDeletePrefix
)

func (k promptKind) String() string {
	switch k {
	case promptDownload:
		return "download"
	case promptMove:
		return "move"
	case promptSearch:
		return "search"
	case promptDeletePrefix:
		return "delete prefix"
	default:
		return "copy"
	}
}

// promptState is the path input shown above the help line.
type promptState struct {
	kind  promptKind
	title string
	input textinput.Model

	// source of a download, copy or move
	bucket    string
	key       string
	versionID string
	recursive bool
	size      int64
}

func newPrompt(kind promptKind, title, value string, width int) *promptState {
	input := textinput.New()
	input.Prompt = ""
	input.SetValue(value)
	input.CursorEnd()
	input.Focus()
	input.Width = max(10, width-len(title)-6)

	return &promptState{kind: kind, title: title, input: input}
}

func (p *promptState) view() string {
	return styleAccent.Render(p.title+": ") + p.input.View()
}
