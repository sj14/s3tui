package ui

import (
	"fmt"
	"io"
	"strings"
	"time"

	"github.com/charmbracelet/bubbles/list"
	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"
	"github.com/charmbracelet/x/ansi"
	"github.com/dustin/go-humanize"
)

const (
	sizeWidth = 10
	timeWidth = 19
)

// rowItem is a list item which renders itself as a single line.
type rowItem interface {
	list.Item
	row(width int, selected bool) string
}

type bucketItem struct {
	name    string
	region  string
	created time.Time
}

func (i bucketItem) FilterValue() string { return i.name }

func (i bucketItem) row(width int, selected bool) string {
	return renderRow(width, selected, false, i.name, "",
		cell(i.region, 16, false),
		cell(formatTime(i.created), timeWidth, false),
	)
}

type objectItem struct {
	key      string // full key, including the prefix
	name     string // name relative to the current prefix
	isPrefix bool   // a "folder", i.e. a common prefix
	deleted  bool   // newest version is a delete marker
	size     int64
	modified time.Time
	class    string
}

func (i objectItem) FilterValue() string { return i.name }

func (i objectItem) row(width int, selected bool) string {
	if i.isPrefix {
		return renderRow(width, selected, true, strings.TrimSuffix(i.name, "/"), "/",
			cell("", sizeWidth, true),
			cell("", timeWidth, false),
		)
	}

	if i.deleted {
		return renderRow(width, selected, false, i.name, " [deleted]",
			cell("-", sizeWidth, true),
			cell(formatTime(i.modified), timeWidth, false),
		)
	}

	return renderRow(width, selected, false, i.name, storageClass(i.class),
		cell(formatSize(i.size), sizeWidth, true),
		cell(formatTime(i.modified), timeWidth, false),
	)
}

type versionItem struct {
	versionID    string
	latest       bool
	deleteMarker bool
	size         int64
	modified     time.Time
	class        string
}

func (i versionItem) FilterValue() string { return i.versionID }

func (i versionItem) row(width int, selected bool) string {
	id := i.versionID
	if id == "" || id == "null" {
		id = "null (unversioned)"
	}

	marks := storageClass(i.class)
	if i.latest {
		marks += " [latest]"
	}
	if i.deleteMarker {
		marks += " [delete-marker]"
	}

	size := formatSize(i.size)
	if i.deleteMarker {
		size = "-"
	}

	return renderRow(width, selected, false, id, marks,
		cell(size, sizeWidth, true),
		cell(formatTime(i.modified), timeWidth, false),
	)
}

// rowDelegate renders any rowItem.
type rowDelegate struct{}

func (rowDelegate) Height() int                         { return 1 }
func (rowDelegate) Spacing() int                        { return 0 }
func (rowDelegate) Update(tea.Msg, *list.Model) tea.Cmd { return nil }
func (d rowDelegate) Render(w io.Writer, m list.Model, index int, item list.Item) {
	row, ok := item.(rowItem)
	if !ok {
		return
	}

	fmt.Fprint(w, row.row(m.Width(), index == m.Index()))
}

// renderRow lays out a line: a cursor, the flexible name, a suffix and the
// right aligned meta columns.
func renderRow(width int, selected, folder bool, name, suffix string, meta ...string) string {
	cursor := "  "
	if selected {
		cursor = "▸ "
	}

	right := strings.Join(meta, " ")

	available := width - lipgloss.Width(cursor) - lipgloss.Width(right) - lipgloss.Width(suffix) - 2
	if available < 4 {
		available = 4
	}

	name = ansi.Truncate(name, available, "…")
	left := name + suffix + strings.Repeat(" ", max(0, available-lipgloss.Width(name)))

	switch {
	case selected:
		return styleSelected.Render(cursor + left + " " + right)
	case folder:
		return cursor + stylePrefixRow.Render(left) + " " + styleDim.Render(right)
	default:
		return cursor + left + " " + styleDim.Render(right)
	}
}

// cell pads s to the given width, optionally right aligned.
func cell(s string, width int, right bool) string {
	s = ansi.Truncate(s, width, "…")

	padding := strings.Repeat(" ", max(0, width-lipgloss.Width(s)))
	if right {
		return padding + s
	}

	return s + padding
}

// storageClass renders anything but the default class as a suffix.
func storageClass(class string) string {
	if class == "" || class == "STANDARD" {
		return ""
	}

	return " [" + class + "]"
}

func formatSize(size int64) string {
	if size < 0 {
		return ""
	}

	return humanize.IBytes(uint64(size))
}

func formatTime(t time.Time) string {
	if t.IsZero() {
		return ""
	}

	return t.Local().Format("2006-01-02 15:04:05")
}

type multipartItem struct {
	key       string
	uploadID  string
	initiated time.Time
	class     string
}

func (i multipartItem) FilterValue() string { return i.key }

func (i multipartItem) row(width int, selected bool) string {
	return renderRow(width, selected, false, i.key, storageClass(i.class),
		cell(i.uploadID, 24, false),
		cell(formatTime(i.initiated), timeWidth, false),
	)
}

type partItem struct {
	number   int32
	etag     string
	size     int64
	modified time.Time
}

func (i partItem) FilterValue() string { return fmt.Sprintf("part %d", i.number) }

func (i partItem) row(width int, selected bool) string {
	return renderRow(width, selected, false, fmt.Sprintf("part %d", i.number), "",
		cell(strings.Trim(i.etag, `"`), 34, false),
		cell(formatSize(i.size), sizeWidth, true),
		cell(formatTime(i.modified), timeWidth, false),
	)
}

// menuNameWidth aligns the descriptions of the bucket overview.
const menuNameWidth = 28

type menuKind int

const (
	menuObjects menuKind = iota
	menuDeleted
	menuMultipart
	menuStats
	menuConfig // one of the documents the bucket or the object is configured with
)

// menuItem is one entry of the overview a bucket opens with.
type menuItem struct {
	kind        menuKind
	config      configKind // which document, when kind is menuConfig
	name        string
	description string
}

func (i menuItem) FilterValue() string { return i.name }

func (i menuItem) row(width int, selected bool) string {
	cursor := "  "
	name := cell(i.name, menuNameWidth, false)

	if selected {
		cursor = "▸ "
		name = styleSelected.Render(name)
	}

	line := cursor + name + styleDim.Render("  "+i.description)

	return ansi.Truncate(line, max(10, width), "…")
}

// bucketMenu is the overview of what can be looked at in a bucket: the objects
// first, then every configuration of the bucket in the order of configSpecs.
func bucketMenu() []list.Item {
	items := []list.Item{
		menuItem{kind: menuObjects, name: "objects", description: "the current objects of the bucket"},
		menuItem{
			kind: menuDeleted, name: "objects with delete markers",
			description: "including keys whose newest version is a delete marker",
		},
		menuItem{
			kind: menuMultipart, name: "multipart uploads",
			description: "started but never finished, they still cost storage",
		},
		menuItem{
			kind: menuStats, name: "statistics",
			description: "counts, sizes and ages of everything the bucket holds",
		},
	}

	return append(items, configEntries(false)...)
}

// objectMenu is the same overview for one object or version. Everything about
// an object is a document of its own, so it is only configurations.
func objectMenu() []list.Item {
	return configEntries(true)
}

// configEntries lists the configurations of one scope, in the order of the
// table they come from.
func configEntries(object bool) []list.Item {
	var items []list.Item

	for kind, spec := range configSpecs {
		if spec.object != object {
			continue
		}

		items = append(items, menuItem{
			kind: menuConfig, config: configKind(kind), name: spec.entry, description: spec.about,
		})
	}

	return items
}
