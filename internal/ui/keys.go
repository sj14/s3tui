package ui

import "github.com/charmbracelet/bubbles/key"

type keyMap struct {
	Up        key.Binding
	Down      key.Binding
	Page      key.Binding
	Open      key.Binding
	Back      key.Binding
	Upload    key.Binding
	Random    key.Binding
	Download  key.Binding
	Copy      key.Binding
	Move      key.Binding
	Delete    key.Binding
	Versions  key.Binding
	Search    key.Binding
	Transfers key.Binding
	Cancel    key.Binding
	Edit      key.Binding
	Filter    key.Binding
	Help      key.Binding
	Quit      key.Binding
}

func defaultKeys() keyMap {
	return keyMap{
		Up:        key.NewBinding(key.WithKeys("up", "k"), key.WithHelp("↑/k", "up")),
		Down:      key.NewBinding(key.WithKeys("down", "j"), key.WithHelp("↓/j", "down")),
		Page:      key.NewBinding(key.WithKeys("pgup", "pgdown"), key.WithHelp("pgup/pgdn", "page")),
		Open:      key.NewBinding(key.WithKeys("enter"), key.WithHelp("enter", "open")),
		Back:      key.NewBinding(key.WithKeys("esc", "backspace"), key.WithHelp("esc", "back")),
		Upload:    key.NewBinding(key.WithKeys("u"), key.WithHelp("u", "upload")),
		Random:    key.NewBinding(key.WithKeys("r"), key.WithHelp("r", "random objects")),
		Download:  key.NewBinding(key.WithKeys("l"), key.WithHelp("l", "download")),
		Copy:      key.NewBinding(key.WithKeys("c"), key.WithHelp("c", "copy")),
		Move:      key.NewBinding(key.WithKeys("m"), key.WithHelp("m", "move")),
		Delete:    key.NewBinding(key.WithKeys("d"), key.WithHelp("d", "delete")),
		Versions:  key.NewBinding(key.WithKeys("D"), key.WithHelp("D", "delete every version")),
		Search:    key.NewBinding(key.WithKeys("/"), key.WithHelp("/", "search")),
		Transfers: key.NewBinding(key.WithKeys("t"), key.WithHelp("t", "transfers")),
		Cancel:    key.NewBinding(key.WithKeys("x"), key.WithHelp("x", "cancel")),
		Edit:      key.NewBinding(key.WithKeys("e"), key.WithHelp("e", "edit")),
		Filter:    key.NewBinding(key.WithKeys("/"), key.WithHelp("/", "filter")),
		Help:      key.NewBinding(key.WithKeys("?"), key.WithHelp("?", "help")),
		Quit:      key.NewBinding(key.WithKeys("ctrl+c"), key.WithHelp("ctrl+c", "quit")),
	}
}

func (k keyMap) ShortHelp() []key.Binding {
	return []key.Binding{k.Back, k.Upload, k.Download, k.Copy, k.Move, k.Delete, k.Help}
}

// viewHelp shows the keys which do something in the current view. The full
// help behind "?" always lists everything.
type viewHelp struct {
	keys      keyMap
	view      viewState
	editable  bool // e does something in this view
	deletable bool // d does something in this view
}

func (v viewHelp) FullHelp() [][]key.Binding { return v.keys.FullHelp() }

func (v viewHelp) ShortHelp() []key.Binding {
	k := v.keys

	switch v.view {
	case viewBuckets:
		return []key.Binding{k.Open, k.Back, k.Filter, k.Help}
	case viewObjects:
		return []key.Binding{
			k.Open, k.Upload, k.Download, k.Copy, k.Move, k.Delete, k.Versions, k.Search, k.Help,
		}
	case viewVersions:
		return []key.Binding{
			k.Open, k.Back, k.Download, k.Copy, k.Delete, k.Versions, k.Filter, k.Help,
		}
	case viewMultipart:
		return []key.Binding{k.Back, k.Open, k.Delete, k.Filter, k.Help}
	case viewParts:
		return []key.Binding{k.Back, k.Filter, k.Help}
	case viewStats:
		return []key.Binding{k.Back, k.Help}
	case viewTransfers:
		return []key.Binding{k.Cancel, k.Back, k.Filter, k.Help}
	case viewProfiles:
		return []key.Binding{k.Open, k.Filter, k.Help}
	case viewBucketMenu, viewObjectMenu:
		return []key.Binding{k.Open, k.Back, k.Help}
	case viewConfig:
		bindings := []key.Binding{}

		if v.editable {
			bindings = append(bindings, k.Edit)
		}

		if v.deletable {
			bindings = append(bindings, k.Delete)
		}

		return append(bindings, k.Back, k.Help)
	case viewPicker:
		return []key.Binding{k.Open, k.Upload, k.Random, k.Back, k.Help}
	}

	return k.ShortHelp()
}

func (k keyMap) FullHelp() [][]key.Binding {
	// The way out comes first: it is the one key the footer never shows.
	return [][]key.Binding{
		{k.Quit, k.Help},
		{k.Up, k.Down, k.Page},
		{k.Open, k.Back, k.Filter, k.Search},
		{k.Upload, k.Random, k.Download, k.Copy, k.Move},
		{k.Delete, k.Versions, k.Edit},
		{k.Transfers, k.Cancel},
	}
}
