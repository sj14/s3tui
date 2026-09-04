package ui

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"time"

	"github.com/charmbracelet/bubbles/list"
	tea "github.com/charmbracelet/bubbletea"
)

// upEntry is the name of the entry that leaves the current directory.
const upEntry = ".."

// localItem is one entry of the local file browser.
type localItem struct {
	name     string
	path     string
	isDir    bool
	up       bool
	size     int64
	modified time.Time
}

func (i localItem) FilterValue() string { return i.name }

func (i localItem) row(width int, selected bool) string {
	if i.up {
		return renderRow(width, selected, true, upEntry, "",
			cell("", sizeWidth, true),
			cell("", timeWidth, false),
		)
	}

	if i.isDir {
		return renderRow(width, selected, true, i.name, "/",
			cell("", sizeWidth, true),
			cell(formatTime(i.modified), timeWidth, false),
		)
	}

	return renderRow(width, selected, false, i.name, "",
		cell(formatSize(i.size), sizeWidth, true),
		cell(formatTime(i.modified), timeWidth, false),
	)
}

// localDirMsg carries the entries of a local directory.
type localDirMsg struct {
	dir   string
	items []list.Item
}

// readLocalDirCmd lists a local directory: ".." first, then the directories,
// then the files. Hidden entries are skipped.
func readLocalDirCmd(_ context.Context, dir string) tea.Cmd {
	return func() tea.Msg {
		entries, err := os.ReadDir(dir)
		if err != nil {
			return fail(fmt.Sprintf("reading %q", dir), err)
		}

		var dirs, files []list.Item

		for _, entry := range entries {
			if entry.Name()[0] == '.' {
				continue
			}

			path := filepath.Join(dir, entry.Name())

			info, err := os.Stat(path) // resolves symlinks
			if err != nil {
				continue // vanished or not readable, skip it
			}

			item := localItem{
				name:     entry.Name(),
				path:     path,
				isDir:    info.IsDir(),
				size:     info.Size(),
				modified: info.ModTime(),
			}

			if item.isDir {
				dirs = append(dirs, item)
			} else {
				files = append(files, item)
			}
		}

		byName := func(items []list.Item) {
			sort.Slice(items, func(a, b int) bool {
				return items[a].(localItem).name < items[b].(localItem).name
			})
		}

		byName(dirs)
		byName(files)

		items := make([]list.Item, 0, len(dirs)+len(files)+1)

		if parent := filepath.Dir(dir); parent != dir {
			items = append(items, localItem{name: upEntry, path: parent, isDir: true, up: true})
		}

		items = append(items, dirs...)
		items = append(items, files...)

		return localDirMsg{dir: dir, items: items}
	}
}
