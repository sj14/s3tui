// Package ui contains the bubbletea application: buckets -> objects -> versions.
package ui

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path"
	"strings"
	"time"

	"github.com/charmbracelet/bubbles/help"
	"github.com/charmbracelet/bubbles/key"
	"github.com/charmbracelet/bubbles/list"
	"github.com/charmbracelet/bubbles/progress"
	"github.com/charmbracelet/bubbles/spinner"
	"github.com/charmbracelet/bubbles/textinput"
	"github.com/charmbracelet/bubbles/viewport"
	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"
	"github.com/dustin/go-humanize"

	"github.com/sj14/s3tui/internal/awsclient"
	"github.com/sj14/s3tui/internal/config"
)

const (
	versioningUnknown  = "Unknown"
	versioningDisabled = "Disabled"
	versioningEnabled  = "Enabled"
	versioningSuspend  = "Suspended"
)

type viewState int

// isDetails reports whether the view is a scrollable page instead of a list.
func (v viewState) isDetails() bool {
	return v == viewConfig || v == viewStats
}

const (
	viewBuckets viewState = iota
	viewBucketMenu
	viewObjectMenu
	viewObjects
	viewVersions
	viewProfiles
	viewMultipart
	viewParts
	viewConfig
	viewStats
	viewTransfers
	viewPicker
)

// Model is the root bubbletea model.
type Model struct {
	ctx     context.Context
	client  *awsclient.Client
	cfg     config.Config
	version string

	profile    string
	profileCfg config.Profile

	view         viewState
	prevView     viewState // the view the file browser or the profile list was opened from
	buckets      list.Model
	objects      list.Model
	versions     list.Model
	profiles     list.Model
	menu         list.Model
	objectMenu   list.Model
	multipart    list.Model
	parts        list.Model
	details      viewport.Model
	transferList list.Model
	picker       list.Model
	popup        viewport.Model // scrolls the error box

	bucket   string // currently opened bucket
	prefix   string // currently opened prefix within the bucket
	search   string // server side prefix search within that prefix
	key      string // currently opened object key
	uploadID string // currently opened multipart upload

	uploadDir      string // directory the file picker starts in
	objectKey      string // object the overview and its pages are about
	objectVersion  string
	objectFrom     viewState    // the list the object overview was opened from
	target         configTarget // what the configuration view shows
	detailsContent string
	configKind     configKind // which document that view shows
	configText     string
	configNote     string
	stats          statsScan // the running or finished bucket scan

	versioning  map[string]string // bucket -> versioning status
	nextToken   *pageToken        // continues the object listing
	showDeleted bool              // list from ListObjectVersions, deleted keys included

	quitArmed    time.Time     // when the first ctrl+c of a running transfer was pressed
	transfers    []*transfer   // running and finished, in the order they started
	transferID   int           // hands out the ids the events are stamped with
	transferFrom viewState     // the view t was pressed in
	confirms     []transferAsk // overwrite questions waiting for an answer

	prompt  *promptState
	pending *pendingConfirm

	spinner  spinner.Model
	progress progress.Model
	inFlight int
	err      error
	status   string

	keys     keyMap
	help     help.Model
	showHelp bool

	width  int
	height int
}

// New creates the model.
func New(ctx context.Context, client *awsclient.Client, profileName string, profile config.Profile, cfg config.Config, version string) Model {
	newList := func(singular, plural string) list.Model {
		l := list.New(nil, rowDelegate{}, 0, 0)
		l.SetShowTitle(false)
		l.SetShowHelp(false)
		l.SetFilteringEnabled(true)
		l.SetStatusBarItemName(singular, plural)
		l.DisableQuitKeybindings()
		l.Styles.StatusBar = styleDim.Padding(0, 0, 0, 2)
		l.Styles.NoItems = styleDim.Padding(0, 0, 0, 2)

		return l
	}

	newMenuList := func() list.Model {
		menu := newList("entry", "entries")
		menu.SetShowStatusBar(false) // a fixed handful of entries needs no count
		menu.SetFilteringEnabled(false)

		return menu
	}

	spin := spinner.New()
	spin.Spinner = spinner.Dot
	spin.Style = lipgloss.NewStyle().Foreground(colorAccent)

	model := Model{
		ctx:          ctx,
		client:       client,
		cfg:          cfg,
		version:      version,
		profile:      profileName,
		profileCfg:   profile,
		buckets:      newList("bucket", "buckets"),
		objects:      newList("object", "objects"),
		versions:     newList("version", "versions"),
		profiles:     newList("profile", "profiles"),
		menu:         newMenuList(),
		objectMenu:   newMenuList(),
		multipart:    newList("multipart upload", "multipart uploads"),
		parts:        newList("part", "parts"),
		transferList: newList("transfer", "transfers"),
		details:      viewport.New(0, 0),
		picker:       newList("entry", "entries"),
		popup:        viewport.New(0, 0),
		versioning:   map[string]string{},
		spinner:      spin,
		progress:     progress.New(progress.WithDefaultGradient(), progress.WithoutPercentage()),
		keys:         defaultKeys(),
		help:         help.New(),
		inFlight:     1, // the initial bucket listing
	}

	// Without a client we start by asking which profile to connect with.
	if client == nil {
		model.view = viewProfiles
		model.inFlight = 0
		model.profiles.SetItems(profileItems(cfg, profileName))
	}

	return model
}

// Init starts the initial bucket listing.
func (m Model) Init() tea.Cmd {
	cmds := []tea.Cmd{tea.SetWindowTitle("s3tui"), m.spinner.Tick}

	if m.client != nil {
		cmds = append(cmds, listBucketsCmd(m.ctx, m.client))
	}

	return tea.Batch(cmds...)
}

// Update handles all messages.
func (m Model) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	switch msg := msg.(type) {
	case tea.WindowSizeMsg:
		m.width, m.height = msg.Width, msg.Height
		m.help.Width = msg.Width
		m.layout()

		return m, nil

	case spinner.TickMsg:
		var cmd tea.Cmd
		m.spinner, cmd = m.spinner.Update(msg)

		return m, cmd

	case errMsg:
		m.inFlight = max(0, m.inFlight-1)
		m.err = fmt.Errorf("%s: %w", msg.what, msg.err)

		return m, nil

	case bucketsMsg:
		m.inFlight = max(0, m.inFlight-1)
		m.err = nil

		return m, m.buckets.SetItems(msg.items)

	case objectsMsg:
		m.inFlight = max(0, m.inFlight-1)

		if msg.bucket != m.bucket || msg.prefix != m.prefix || msg.search != m.search {
			return m, nil // a stale response of a listing we left already
		}

		m.err = nil
		m.nextToken = msg.next

		if msg.note != "" {
			m.showDeleted = false
			m.status = msg.note
		}

		items := msg.items
		if msg.more {
			items = mergeObjects(m.objects.Items(), items)
		}

		cmd := m.objects.SetItems(items)
		if !msg.more {
			m.objects.ResetSelected()
		}

		// keep a transfer summary visible, it is cleared on the next navigation
		if hint := m.moreHint(); hint != "" && msg.note == "" {
			m.status = hint
		}

		return m, cmd

	case versionsMsg:
		m.inFlight = max(0, m.inFlight-1)

		if msg.bucket != m.bucket || msg.key != m.key {
			return m, nil
		}

		m.err = nil

		// the summary of a delete which just finished stays readable, every
		// navigation into this view clears the status by itself
		cmd := m.versions.SetItems(msg.items)
		m.versions.ResetSelected()

		return m, cmd

	case multipartMsg:
		m.inFlight = max(0, m.inFlight-1)

		if msg.bucket != m.bucket || msg.prefix != m.prefix {
			return m, nil
		}

		m.err = nil

		cmd := m.multipart.SetItems(msg.items)
		m.multipart.ResetSelected()

		return m, cmd

	case partsMsg:
		m.inFlight = max(0, m.inFlight-1)

		if msg.bucket != m.bucket || msg.uploadID != m.uploadID {
			return m, nil
		}

		m.err = nil

		cmd := m.parts.SetItems(msg.items)
		m.parts.ResetSelected()

		return m, cmd

	case versioningMsg:
		m.inFlight = max(0, m.inFlight-1)
		m.versioning[msg.bucket] = msg.status

		return m, nil

	case configMsg:
		m.inFlight = max(0, m.inFlight-1)

		if msg.target != m.target || msg.kind != m.configKind {
			return m, nil
		}

		m.configText, m.configNote = msg.document, msg.note
		m.renderConfig()

		return m, nil

	case statsPageMsg:
		m.inFlight = max(0, m.inFlight-1)

		if msg.bucket != m.stats.bucket || msg.run != m.stats.run || !m.stats.running {
			return m, nil // a page of a scan which was left already
		}

		m.err = nil
		m.stats.stats.merge(msg.page)

		if m.stats.stats.pages == 1 {
			m.stats.versions = msg.versions
			m.stats.note = msg.note
		}

		if msg.next != nil {
			m.inFlight++
			m.renderStats()

			return m, statsPageCmd(m.ctx, m.client, m.stats.bucket, m.stats.run, m.stats.versions, msg.next)
		}

		m.stats.running = false
		m.renderStats()

		return m, nil

	case editorClosedMsg:
		if msg.err != nil {
			os.Remove(msg.path) // the editor failed, its file is of no use

			m.err = fmt.Errorf("editor: %w", msg.err)

			return m, nil
		}

		m.inFlight++

		return m, saveConfigCmd(m.ctx, m.client, msg)

	case configSavedMsg:
		m.inFlight = max(0, m.inFlight-1)

		if msg.target != m.target || m.view != viewConfig || msg.kind != m.configKind {
			return m, nil
		}

		if msg.err != nil {
			m.err = msg.err

			return m, nil
		}

		m.status = msg.note

		if !msg.changed {
			return m, nil
		}

		// the breadcrumb of every view names the versioning state
		if msg.kind == configVersioning {
			m.inFlight++

			model, cmd := m.reloadConfig()

			return model, tea.Batch(cmd, bucketVersioningCmd(m.ctx, m.client, msg.target.bucket))
		}

		// show what the bucket holds now, not what was sent
		return m.reloadConfig()

	case localDirMsg:
		if msg.dir != m.uploadDir {
			return m, nil
		}

		m.err = nil

		cmd := m.picker.SetItems(msg.items)
		m.picker.ResetSelected()

		return m, cmd

	case multipartAbortedMsg:
		m.inFlight = max(0, m.inFlight-1)
		m.err = nil
		m.status = "aborted multipart upload " + short(msg.uploadID, 16)

		if m.view == viewMultipart && msg.bucket == m.bucket {
			m.inFlight++

			return m, listMultipartCmd(m.ctx, m.client, m.bucket, m.prefix)
		}

		return m, nil

	case quitExpiredMsg:
		if msg.armed.Equal(m.quitArmed) {
			m.quitArmed = time.Time{}
		}

		return m, nil

	case profileSwitchedMsg:
		return m.applyProfile(msg)

	case transferMsg:
		return m.handleTransfer(msg)

	case tea.KeyMsg:
		return m.handleKey(msg)
	}

	// Keep the cursor of an open prompt blinking.
	if m.prompt != nil {
		var cmd tea.Cmd
		m.prompt.input, cmd = m.prompt.input.Update(msg)

		return m, cmd
	}

	return m.forwardToList(msg)
}

func (m Model) handleKey(msg tea.KeyMsg) (tea.Model, tea.Cmd) {
	// The way out, from wherever: an open prompt, a question, the error box.
	if key.Matches(msg, m.keys.Quit) {
		return m.quit()
	}

	// An error is shown as a box over everything and wants an answer first.
	if m.err != nil {
		return m.handleErrorPopup(msg)
	}

	if len(m.confirms) > 0 {
		return m.handleConfirm(msg)
	}

	if m.pending != nil {
		return m.handlePending(msg)
	}

	if m.prompt != nil {
		return m.handlePrompt(msg)
	}

	// While typing a filter, every key belongs to the text input.
	if !m.view.isDetails() && m.currentList().FilterState() == list.Filtering {
		return m.forwardToList(msg)
	}

	// Inside the file browser only a handful of keys are ours, everything else
	// belongs to the list: moving, filtering, paging.
	if m.view == viewPicker {
		switch {
		case key.Matches(msg, m.keys.Help):
			m.showHelp = !m.showHelp
			m.help.ShowAll = m.showHelp
			m.layout()

			return m, nil
		case key.Matches(msg, m.keys.Back) && m.picker.FilterState() == list.Unfiltered:
			m.view = m.prevView

			return m, nil
		case key.Matches(msg, m.keys.Open):
			return m.openLocal()
		case key.Matches(msg, m.keys.Upload):
			return m.askUpload()
		case key.Matches(msg, m.keys.Random):
			return m.askRandomUpload()
		case key.Matches(msg, m.keys.Transfers):
			return m.openTransfers()
		}

		return m.forwardToList(msg)
	}

	switch {
	case key.Matches(msg, m.keys.Help):
		m.showHelp = !m.showHelp
		m.help.ShowAll = m.showHelp
		m.layout()

		return m, nil

	case key.Matches(msg, m.keys.Back):
		// An applied filter is cleared by the list itself first.
		if !m.view.isDetails() && m.currentList().FilterState() != list.Unfiltered && msg.String() == "esc" {
			return m.forwardToList(msg)
		}

		return m.back()

	case key.Matches(msg, m.keys.Open):
		return m.open()

	case key.Matches(msg, m.keys.Transfers):
		return m.openTransfers()

	case key.Matches(msg, m.keys.Cancel) && m.view == viewTransfers:
		return m.cancelTransfer()

	case key.Matches(msg, m.keys.Edit) && m.view == viewConfig:
		return m.editConfig()

	case key.Matches(msg, m.keys.Search) && m.view == viewObjects:
		return m.askSearch()

	case key.Matches(msg, m.keys.Versions):
		return m.askDeleteVersions()

	case key.Matches(msg, m.keys.Delete):
		return m.askDelete()

	case key.Matches(msg, m.keys.Copy):
		return m.askCopy(transferCopy)

	case key.Matches(msg, m.keys.Move):
		return m.askCopy(transferMove)

	case key.Matches(msg, m.keys.Upload):
		return m.askUpload()

	case key.Matches(msg, m.keys.Download):
		return m.askDownload()
	}

	return m.forwardToList(msg)
}

// quitWindow is how long the second ctrl+c has to follow the first one.
const quitWindow = time.Second

// quitExpiredMsg ends the window in which a second ctrl+c quits.
type quitExpiredMsg struct{ armed time.Time }

// quit leaves, but not while transfers are running: those want a second
// ctrl+c right after the first one, so that one stray keypress cannot throw
// away a download that is nearly done.
func (m Model) quit() (tea.Model, tea.Cmd) {
	if len(m.running()) == 0 {
		return m, tea.Quit
	}

	if !m.quitArmed.IsZero() && time.Since(m.quitArmed) <= quitWindow {
		return m, tea.Quit
	}

	m.quitArmed = time.Now()
	armed := m.quitArmed

	return m, tea.Tick(quitWindow, func(time.Time) tea.Msg { return quitExpiredMsg{armed: armed} })
}

// pendingConfirm is a yes/no question asked before a destructive action.
type pendingConfirm struct {
	question string
	action   func(Model) (tea.Model, tea.Cmd)
}

// handlePending runs or drops the action behind a confirmation question.
func (m Model) handlePending(msg tea.KeyMsg) (tea.Model, tea.Cmd) {
	action := m.pending.action
	m.pending = nil

	if msg.String() != "y" {
		m.status = "canceled"

		return m, nil
	}

	return action(m)
}

// handleConfirm answers the oldest overwrite question. Several transfers can
// ask at the same time, they are answered one after the other.
func (m Model) handleConfirm(msg tea.KeyMsg) (tea.Model, tea.Cmd) {
	answer := replyNo

	switch msg.String() {
	case "y":
		answer = replyYes
	case "n", "esc":
		answer = replyNo
	case "a":
		answer = replyAll
	case "s":
		answer = replySkipAll
	default:
		return m, nil // ignore anything else while asking
	}

	ask := m.confirms[0]
	m.confirms = m.confirms[1:]

	cmds := []tea.Cmd{answerOverwrite(ask.reply, answer)}

	if entry := m.transferByID(ask.id); entry != nil && !entry.finished {
		cmds = append(cmds, waitForTransfer(entry))
	}

	return m, tea.Batch(cmds...)
}

// handlePrompt edits and submits the path input.
func (m Model) handlePrompt(msg tea.KeyMsg) (tea.Model, tea.Cmd) {
	switch msg.String() {
	case "esc":
		m.prompt = nil

		return m, nil

	case "enter":
		prompt := m.prompt
		m.prompt = nil

		return m.startTransfer(prompt)
	}

	var cmd tea.Cmd
	m.prompt.input, cmd = m.prompt.input.Update(msg)

	return m, cmd
}

// openBucketSection opens what was picked in the bucket overview.
func (m Model) openBucketSection(selected menuItem) (tea.Model, tea.Cmd) {
	m.err, m.status = nil, ""

	switch selected.kind {
	case menuMultipart:
		return m.openMultipart()

	case menuStats:
		return m.openStats()

	case menuConfig:
		return m.openBucketConfig(selected.config)
	}

	m.view = viewObjects
	m.showDeleted = selected.kind == menuDeleted
	m.prefix, m.key, m.search = "", "", ""
	m.nextToken = nil
	m.objects.SetItems(nil)
	m.objects.ResetFilter()
	m.inFlight++

	return m, listObjectsCmd(m.ctx, m.client, m.bucket, m.prefix, m.search, m.showDeleted, nil)
}

// mergeObjects appends a page without listing a key twice: the versions of one
// key can span a page boundary.
func mergeObjects(existing, page []list.Item) []list.Item {
	seen := make(map[string]bool, len(existing))
	for _, item := range existing {
		seen[item.(objectItem).key] = true
	}

	for _, item := range page {
		if seen[item.(objectItem).key] {
			continue
		}

		existing = append(existing, item)
	}

	return existing
}

// selectedObject returns the highlighted object, refusing the ones that only
// exist as a delete marker: every operation on them would hit a 404.
func (m Model) selectedObject(action string) (objectItem, string, bool) {
	selected, ok := m.objects.SelectedItem().(objectItem)
	if !ok {
		return objectItem{}, "", false
	}

	if selected.deleted {
		return objectItem{}, fmt.Sprintf("%s is deleted, open its versions to %s it", selected.name, action), false
	}

	return selected, "", true
}

// openObjectMenu opens the overview of one object: the documents it is made
// of, from its metadata to its legal hold.
func (m Model) openObjectMenu() (tea.Model, tea.Cmd) {
	key, version := "", ""

	switch m.view {
	case viewObjects:
		selected, note, ok := m.selectedObject("inspect")
		if !ok {
			m.status = note

			return m, nil
		}

		if selected.isPrefix {
			m.status = "a prefix has no metadata, only objects have"

			return m, nil
		}

		key = selected.key

	case viewVersions:
		selected, ok := m.versions.SelectedItem().(versionItem)
		if !ok {
			return m, nil
		}

		if selected.deleteMarker {
			m.status = "a delete marker has no metadata"

			return m, nil
		}

		key, version = m.key, selected.versionID

	default:
		m.status = "select an object to see its details"

		return m, nil
	}

	m.objectFrom = m.view
	m.view = viewObjectMenu
	m.objectKey, m.objectVersion = key, version
	m.err, m.status = nil, ""
	m.objectMenu.SetItems(objectMenu())
	m.objectMenu.ResetSelected()

	return m, nil
}

// openBucketConfig shows one of the documents a bucket is configured with.
func (m Model) openBucketConfig(kind configKind) (tea.Model, tea.Cmd) {
	if m.bucket == "" {
		return m, nil
	}

	return m.openConfig(kind, configTarget{bucket: m.bucket})
}

// openObjectConfig shows one of the documents of the opened object.
func (m Model) openObjectConfig(kind configKind) (tea.Model, tea.Cmd) {
	if m.objectKey == "" {
		return m, nil
	}

	return m.openConfig(kind, configTarget{
		bucket:  m.bucket,
		key:     m.objectKey,
		version: m.objectVersion,
	})
}

func (m Model) openConfig(kind configKind, target configTarget) (tea.Model, tea.Cmd) {
	m.view = viewConfig
	m.configKind = kind
	m.target = target
	m.err, m.status = nil, ""
	m.configText, m.configNote = "", ""
	m.detailsContent = ""
	m.details.SetContent("")
	m.details.GotoTop()
	m.inFlight++

	return m, configCmd(m.ctx, m.client, target, kind)
}

// reloadConfig fetches the shown document again, after it was written.
func (m Model) reloadConfig() (tea.Model, tea.Cmd) {
	if m.target.bucket == "" {
		return m, nil
	}

	m.inFlight++
	m.details.GotoTop()

	m.configText, m.configNote = "", ""
	m.renderConfig()

	return m, configCmd(m.ctx, m.client, m.target, m.configKind)
}

// section renders one titled block of a details view.
func section(title, body, note string) string {
	text := styleAccent.Render(title) + "\n\n"

	switch {
	case note != "":
		text += styleDim.Render("  "+note) + "\n"
	case body != "":
		text += body
		if !strings.HasSuffix(body, "\n") {
			text += "\n"
		}
	default:
		text += styleDim.Render("  loading…") + "\n"
	}

	return text
}

// renderConfig rebuilds the content of the configuration view.
func (m *Model) renderConfig() {
	spec := m.configKind.spec()

	m.detailsContent = section(spec.name, indentBlock(m.configText), m.configNote)

	m.details.SetContent(m.detailsContent)
}

// canDelete reports whether d does anything here: not every configuration can
// be taken off again, the footer should not offer what would fail.
func (m Model) canDelete() bool {
	if m.view != viewConfig {
		return true
	}

	return m.configKind.spec().remove != nil
}

// canEdit reports whether e does anything here. What S3 only reports, like the
// metadata of an object, has no way back in.
func (m Model) canEdit() bool {
	return m.view == viewConfig && m.configKind.spec().put != nil
}

// openMultipart lists the unfinished multipart uploads of the current bucket.
func (m Model) openMultipart() (tea.Model, tea.Cmd) {
	m.view = viewMultipart
	m.key, m.uploadID = "", ""
	m.err, m.status = nil, ""
	m.multipart.SetItems(nil)
	m.multipart.ResetFilter()
	m.inFlight++

	return m, listMultipartCmd(m.ctx, m.client, m.bucket, m.prefix)
}

// askSearch asks for a prefix to narrow the listing with. S3 can only match
// the beginning of a key, so this is a prefix search, not a substring one.
func (m Model) askSearch() (tea.Model, tea.Cmd) {
	m.prompt = newPrompt(promptSearch,
		fmt.Sprintf("search keys starting with, in s3://%s/%s", m.bucket, m.prefix),
		m.search, m.width,
	)

	return m, textinput.Blink
}

// runSearch reloads the listing with the given prefix search.
func (m Model) runSearch(search string) (tea.Model, tea.Cmd) {
	m.search = search
	m.err, m.status = nil, ""
	m.nextToken = nil
	m.objects.SetItems(nil)
	m.inFlight++

	return m, listObjectsCmd(m.ctx, m.client, m.bucket, m.prefix, m.search, m.showDeleted, nil)
}

// finishTransfer reports the result and refreshes the listing after an upload.
func (m Model) finishTransfer(entry *transfer, msg transferDoneMsg) (tea.Model, tea.Cmd) {
	entry.finished = true
	entry.summary = msg.summary
	entry.current = ""

	// a question of this transfer will never be answered now
	m.confirms = withoutAsks(m.confirms, entry.id)

	switch {
	case msg.err != nil && errors.Is(msg.err, context.Canceled):
		entry.canceled = true

		m.status = entry.describe() + " canceled"
		if msg.summary != "" {
			m.status += " · " + msg.summary
		}
	case msg.err != nil:
		entry.err = msg.err
		m.err = fmt.Errorf("%s: %w", entry.describe(), msg.err)
	default:
		// One that did what it was asked for is nothing to look at: the
		// summary is in the status line, and the list keeps what needs a
		// second look – what failed and what was canceled.
		m.transfers = withoutTransfer(m.transfers, entry.id)
		m.status = msg.summary
	}

	m.transferList.SetItems(transferItems(m.transfers))

	// Everything but a download changes the listing we are looking at.
	if entry.kind != transferDownload && msg.err == nil {
		switch m.view {
		case viewObjects:
			m.inFlight++

			return m, listObjectsCmd(m.ctx, m.client, m.bucket, m.prefix, m.search, m.showDeleted, nil)

		case viewVersions:
			m.inFlight++

			return m, listVersionsCmd(m.ctx, m.client, m.bucket, m.key)
		}
	}

	return m, nil
}

// withoutAsks drops the open questions of one transfer.
func withoutAsks(asks []transferAsk, id int) []transferAsk {
	kept := make([]transferAsk, 0, len(asks))

	for _, ask := range asks {
		if ask.id != id {
			kept = append(kept, ask)
		}
	}

	return kept
}

// openProfiles shows the profile switcher.
func (m Model) openProfiles() (tea.Model, tea.Cmd) {
	if len(m.cfg.Profiles) == 0 {
		m.status = "no profiles configured"

		return m, nil
	}

	if m.view == viewProfiles {
		return m, nil
	}

	m.prevView = m.view
	m.view = viewProfiles
	m.err, m.status = nil, ""
	m.profiles.SetItems(profileItems(m.cfg, m.profile))
	m.profiles.ResetFilter()

	return m, nil
}

// applyProfile swaps the client and starts over with the new profile.
func (m Model) applyProfile(msg profileSwitchedMsg) (tea.Model, tea.Cmd) {
	m.inFlight = max(0, m.inFlight-1)

	m.client = msg.client
	m.profile = msg.name
	m.profileCfg = msg.profile

	m.view = viewBuckets
	m.bucket, m.prefix, m.key = "", "", ""
	m.search = ""
	m.pending = nil
	m.nextToken = nil
	m.versioning = map[string]string{}
	m.err = nil
	m.status = "switched to profile " + msg.name

	m.buckets.SetItems(nil)
	m.menu.SetItems(nil)
	m.objectMenu.SetItems(nil)
	m.objects.SetItems(nil)
	m.versions.SetItems(nil)
	m.multipart.SetItems(nil)
	m.parts.SetItems(nil)
	m.buckets.ResetFilter()
	m.inFlight++

	return m, listBucketsCmd(m.ctx, m.client)
}

// open descends into the selected item.
func (m Model) open() (tea.Model, tea.Cmd) {
	switch m.view {
	case viewProfiles:
		selected, ok := m.profiles.SelectedItem().(profileItem)
		if !ok {
			return m, nil
		}

		if selected.name == m.profile && m.client != nil {
			m.view = m.prevView

			return m, nil
		}

		m.inFlight++

		return m, switchProfileCmd(m.ctx, selected.name, selected.profile, m.version)

	case viewBuckets:
		selected, ok := m.buckets.SelectedItem().(bucketItem)
		if !ok {
			return m, nil
		}

		m.view = viewBucketMenu
		m.bucket, m.prefix, m.key = selected.name, "", ""
		m.search, m.showDeleted = "", false
		m.err, m.status = nil, ""
		m.nextToken = nil
		m.objects.SetItems(nil)
		m.objects.ResetFilter()
		m.menu.SetItems(bucketMenu())
		m.menu.ResetSelected()
		m.inFlight++

		return m, bucketVersioningCmd(m.ctx, m.client, m.bucket)

	case viewBucketMenu:
		selected, ok := m.menu.SelectedItem().(menuItem)
		if !ok {
			return m, nil
		}

		return m.openBucketSection(selected)

	case viewObjectMenu:
		selected, ok := m.objectMenu.SelectedItem().(menuItem)
		if !ok {
			return m, nil
		}

		return m.openObjectConfig(selected.config)

	case viewMultipart:
		selected, ok := m.multipart.SelectedItem().(multipartItem)
		if !ok {
			return m, nil
		}

		m.view = viewParts
		m.key, m.uploadID = selected.key, selected.uploadID
		m.err, m.status = nil, ""
		m.parts.SetItems(nil)
		m.parts.ResetFilter()
		m.inFlight++

		return m, listPartsCmd(m.ctx, m.client, m.bucket, m.key, m.uploadID)

	case viewVersions:
		// one level deeper than a version is its own overview
		return m.openObjectMenu()

	case viewObjects:
		selected, ok := m.objects.SelectedItem().(objectItem)
		if !ok {
			return m, nil
		}

		if selected.isPrefix {
			m.prefix = selected.key
			m.search = ""
			m.err, m.status = nil, ""
			m.nextToken = nil
			m.objects.SetItems(nil)
			m.objects.ResetFilter()
			m.inFlight++

			return m, listObjectsCmd(m.ctx, m.client, m.bucket, m.prefix, m.search, m.showDeleted, nil)
		}

		// without versioning there is no list of versions to step through,
		// so the overview of the object is what lies one level deeper
		switch m.versioning[m.bucket] {
		case versioningEnabled, versioningSuspend, versioningUnknown:
		default:
			return m.openObjectMenu()
		}

		m.view = viewVersions
		m.key = selected.key
		m.err, m.status = nil, ""
		m.versions.SetItems(nil)
		m.versions.ResetFilter()
		m.inFlight++

		return m, listVersionsCmd(m.ctx, m.client, m.bucket, m.key)
	}

	return m, nil
}

// back leaves the current level.
func (m Model) back() (tea.Model, tea.Cmd) {
	m.err, m.status = nil, ""

	switch m.view {
	case viewProfiles:
		return m, nil // the profile list is the root, a profile has to be picked

	case viewConfig:
		m.view = viewBucketMenu
		if m.configKind.spec().object {
			m.view = viewObjectMenu
		}

		m.target = configTarget{}

		return m, nil

	case viewObjectMenu:
		m.view = m.objectFrom
		m.objectKey, m.objectVersion = "", ""

		return m, nil

	case viewParts:
		m.view = viewMultipart
		m.key, m.uploadID = "", ""

		return m, nil

	case viewMultipart:
		m.view = viewBucketMenu

		return m, nil

	case viewStats:
		m.view = viewBucketMenu
		m.stats.running = false // the page which is on its way is the last one

		return m, nil

	case viewTransfers:
		m.view = m.transferFrom

		return m, nil

	case viewVersions:
		m.view = viewObjects
		m.key = ""
		m.status = m.moreHint()

		return m, nil

	case viewBucketMenu:
		m.view = viewBuckets
		m.bucket = ""

		return m, nil

	case viewBuckets:
		if len(m.cfg.Profiles) > 0 {
			return m.openProfiles()
		}

		return m, nil

	case viewObjects:
		// esc drops the search first, then walks up
		if m.search != "" {
			return m.runSearch("")
		}

		if m.prefix != "" {
			m.prefix = parentPrefix(m.prefix)
			m.nextToken = nil
			m.objects.SetItems(nil)
			m.objects.ResetFilter()
			m.inFlight++

			return m, listObjectsCmd(m.ctx, m.client, m.bucket, m.prefix, m.search, m.showDeleted, nil)
		}

		m.view = viewBucketMenu

		return m, nil
	}

	return m, nil
}

// loadMore fetches the next page of the object listing.
func (m Model) loadMore() (tea.Model, tea.Cmd) {
	if m.view != viewObjects || m.nextToken == nil || m.inFlight > 0 {
		return m, nil
	}

	token := m.nextToken
	m.nextToken = nil
	m.inFlight++

	return m, listObjectsCmd(m.ctx, m.client, m.bucket, m.prefix, m.search, m.showDeleted, token)
}

// forwardToList hands the message to the list of the current view.
func (m Model) forwardToList(msg tea.Msg) (tea.Model, tea.Cmd) {
	var cmd tea.Cmd

	switch m.view {
	case viewBuckets:
		m.buckets, cmd = m.buckets.Update(msg)
	case viewProfiles:
		m.profiles, cmd = m.profiles.Update(msg)
	case viewBucketMenu:
		m.menu, cmd = m.menu.Update(msg)
	case viewObjectMenu:
		m.objectMenu, cmd = m.objectMenu.Update(msg)
	case viewObjects:
		m.objects, cmd = m.objects.Update(msg)

		// Fetch the next page before the user reaches the end of the list.
		if m.nextToken != nil && m.objects.Index() >= len(m.objects.Items())-25 {
			model, moreCmd := m.loadMore()

			return model, tea.Batch(cmd, moreCmd)
		}
	case viewVersions:
		m.versions, cmd = m.versions.Update(msg)
	case viewMultipart:
		m.multipart, cmd = m.multipart.Update(msg)
	case viewParts:
		m.parts, cmd = m.parts.Update(msg)
	case viewTransfers:
		m.transferList, cmd = m.transferList.Update(msg)
	case viewPicker:
		m.picker, cmd = m.picker.Update(msg)

	case viewConfig, viewStats:
		// the viewport has no home/end binding, the lists do
		if pressed, ok := msg.(tea.KeyMsg); ok {
			switch pressed.String() {
			case "g":
				m.details.GotoTop()

				return m, nil
			case "G":
				m.details.GotoBottom()

				return m, nil
			}
		}

		m.details, cmd = m.details.Update(msg)
	}

	return m, cmd
}

func (m Model) currentList() list.Model {
	switch m.view {
	case viewObjects:
		return m.objects
	case viewVersions:
		return m.versions
	case viewProfiles:
		return m.profiles
	case viewBucketMenu:
		return m.menu
	case viewObjectMenu:
		return m.objectMenu
	case viewMultipart:
		return m.multipart
	case viewParts:
		return m.parts
	case viewTransfers:
		return m.transferList
	case viewPicker:
		return m.picker
	default:
		return m.buckets
	}
}

func (m *Model) layout() {
	// header (2 lines) + separator + list + separator + footer
	height := max(3, m.height-4-m.footerHeight())
	width := max(20, m.width-2)

	m.buckets.SetSize(width, height)
	m.objects.SetSize(width, height)
	m.versions.SetSize(width, height)
	m.profiles.SetSize(width, height)
	m.menu.SetSize(width, height)
	m.objectMenu.SetSize(width, height)
	m.multipart.SetSize(width, height)
	m.parts.SetSize(width, height)
	m.transferList.SetSize(width, height)
	m.details.Width, m.details.Height = width, height
	m.picker.SetSize(width, height)
	m.progress.Width = min(24, max(10, width/4))
}

// footerHeight is the context line plus the help.
func (m Model) footerHeight() int {
	return 1 + lipgloss.Height(m.helpView())
}

func (m Model) moreHint() string {
	if m.nextToken == nil {
		return ""
	}

	return fmt.Sprintf("%d objects loaded, more are fetched while scrolling", len(m.objects.Items()))
}

// View renders the whole screen.
func (m Model) View() string {
	if m.width == 0 {
		return "loading…"
	}

	var b strings.Builder

	b.WriteString(m.header())
	b.WriteString("\n")
	b.WriteString(m.body())
	b.WriteString("\n")
	b.WriteString(m.footer())

	if m.err != nil {
		return overlay(b.String(), m.errorPopup(), m.width)
	}

	return b.String()
}

// body renders the current view. While a listing is still on its way, the list
// would claim "No objects." in both its status bar and its body, so show a
// placeholder of the same height instead.
func (m Model) body() string {
	switch m.view {
	case viewConfig, viewStats:
		return m.details.View()
	}

	current := m.currentList()

	if len(current.Items()) == 0 {
		if m.inFlight > 0 && m.view != viewTransfers {
			return lipgloss.NewStyle().Height(current.Height()).Render(styleDim.Render("  loading…"))
		}

		// The status bar would repeat what the empty body already says.
		current.SetShowStatusBar(false)
	}

	return current.View()
}

func (m Model) header() string {
	title := styleApp.Render("s3tui")

	if m.client == nil {
		title += styleDim.Render("  select a profile")
	} else {
		target := m.profileCfg.Endpoint
		if target == "" {
			target = "aws " + m.client.Region()
		}

		// a config without profiles has no name to show, only the target
		if m.profile != "" {
			target = m.profile + " · " + target
		}

		title += styleDim.Render("  " + target)
	}

	if m.profileCfg.ReadOnly {
		title += styleDim.Render(" · read-only")
	}

	if m.inFlight > 0 {
		title += "  " + m.spinner.View()
	}

	return styleHeader.Render(title + "\n" + m.breadcrumb())
}

func (m Model) breadcrumb() string {
	if m.view == viewProfiles {
		hint := "  (enter switches, esc cancels)"
		if m.client == nil {
			hint = "  (enter connects)"
		}

		return styleCrumb.Render("profiles") + styleDim.Render(hint)
	}

	if m.view == viewTransfers {
		crumb := styleCrumb.Render("transfers")

		if running := len(m.running()); running > 0 {
			crumb += styleDim.Render(fmt.Sprintf(" [%d running]", running))
		}

		return m.clamp(crumb)
	}

	if m.view == viewStats {
		return m.clamp(styleCrumb.Render("buckets") + styleDim.Render(" / ") +
			styleCrumb.Render(m.stats.bucket) + styleDim.Render(" [statistics]"))
	}

	if m.view == viewConfig && !m.configKind.spec().object {
		return m.clamp(styleCrumb.Render("buckets") + styleDim.Render(" / ") +
			styleCrumb.Render(m.target.bucket) + styleDim.Render(" ["+m.configKind.spec().short+"]"))
	}

	if m.view == viewObjectMenu || m.view == viewConfig {
		crumb := styleCrumb.Render("buckets") + styleDim.Render(" / ") +
			styleCrumb.Render(m.bucket) + styleDim.Render(" / ") +
			styleCrumb.Render(trimPrefix(m.objectKey, m.prefix))

		if m.objectVersion != "" {
			crumb += styleDim.Render(" [" + short(m.objectVersion, 16) + "]")
		}

		if m.view == viewConfig {
			crumb += styleDim.Render(" [" + m.configKind.spec().short + "]")
		}

		return m.clamp(crumb)
	}

	if m.view == viewPicker {
		return m.clamp(styleCrumb.Render(m.uploadDir) +
			styleDim.Render(fmt.Sprintf(" → s3://%s/%s", m.bucket, m.prefix)))
	}

	crumbs := []string{styleCrumb.Render("buckets")}

	if m.view == viewBucketMenu && m.bucket != "" {
		return m.clamp(crumbs[0] + styleDim.Render(" / ") + styleCrumb.Render(m.bucket) + m.versioningBadge())
	}

	if m.bucket != "" {
		crumbs = append(crumbs, styleCrumb.Render(m.bucket)+m.versioningBadge())
	}

	if m.prefix != "" {
		crumbs = append(crumbs, styleCrumb.Render(m.prefix))
	}

	if m.showDeleted && (m.view == viewObjects || m.view == viewVersions) {
		crumbs = append(crumbs, styleWarn.Render("[with deleted]"))
	}

	if m.search != "" && (m.view == viewObjects || m.view == viewVersions) {
		crumbs = append(crumbs, styleWarn.Render(m.search+"*"))
	}

	switch m.view {
	case viewVersions:
		crumbs = append(crumbs, styleCrumb.Render(trimPrefix(m.key, m.prefix))+styleDim.Render(" [versions]"))
	case viewMultipart:
		crumbs = append(crumbs, styleCrumb.Render("multipart uploads"))
	case viewParts:
		crumbs = append(crumbs,
			styleCrumb.Render("multipart uploads"),
			styleCrumb.Render(trimPrefix(m.key, m.prefix))+styleDim.Render(" ["+short(m.uploadID, 16)+"]"),
		)
	}

	return m.clamp(strings.Join(crumbs, styleDim.Render(" / ")))
}

// versioningBadge tells whether the bucket keeps versions.
func (m Model) versioningBadge() string {
	switch m.versioning[m.bucket] {
	case versioningEnabled:
		return styleOK.Render(" [versioned]")
	case versioningSuspend:
		return styleDim.Render(" [versioning suspended]")
	case versioningDisabled:
		return styleDim.Render(" [not versioned]")
	}

	return ""
}

func (m Model) footer() string {
	return styleFooter.Render(m.contextLine() + "\n" + m.helpView())
}

// helpView renders the keys of the current view. The short help is trimmed
// from the back until it fits, instead of being cut off mid word: what does
// not fit stays reachable under "?".
func (m Model) helpView() string {
	keys := viewHelp{keys: m.keys, view: m.view, deletable: m.canDelete(), editable: m.canEdit()}

	if m.showHelp {
		return m.help.FullHelpView(keys.FullHelp())
	}

	// measuring needs a help model that does not truncate for us
	measure := m.help
	measure.Width = 0

	bindings := keys.ShortHelp()
	available := max(10, m.width-2)

	for len(bindings) > 2 && lipgloss.Width(measure.ShortHelpView(bindings)) > available {
		// drop the entry in front of the help key
		bindings = append(bindings[:len(bindings)-2], bindings[len(bindings)-1:]...)
	}

	return m.help.ShortHelpView(bindings)
}

// contextLine shows the most urgent state: a question, a prompt, the running
// transfer, an error or a status message.
func (m Model) contextLine() string {
	switch {
	case !m.quitArmed.IsZero() && len(m.running()) > 0:
		running := fmt.Sprintf("%d transfers are still running", len(m.running()))
		if len(m.running()) == 1 {
			running = "a transfer is still running"
		}

		return m.clamp(styleWarn.Render(running) + styleDim.Render("  ctrl+c again to quit"))

	case len(m.confirms) > 0:
		question := "overwrite " + m.confirms[0].path + "?"
		if more := len(m.confirms) - 1; more > 0 {
			question += fmt.Sprintf(" (+%d waiting)", more)
		}

		return m.clamp(styleWarn.Render(question) +
			styleDim.Render("  [y]es  [n]o  [a]ll  [s]kip all"))

	case m.pending != nil:
		return m.clamp(styleErr.Render(m.pending.question) + styleDim.Render("  [y]es  [n]o"))

	case m.prompt != nil:
		return m.clamp(m.prompt.view())

	case len(m.running()) > 0:
		return m.clamp(m.transferLine())

	case m.status != "":
		return m.clamp(styleDim.Render(m.status))

	case m.view.isDetails() && m.details.TotalLineCount() > m.details.Height:
		return m.clamp(styleDim.Render(fmt.Sprintf("%.0f%% · ↑/↓ scrolls", m.details.ScrollPercent()*100)))
	}

	return ""
}

// transferLine is the footer while transfers are running: the one that runs
// in detail, several of them added up.
func (m Model) transferLine() string {
	running := m.running()

	hint := styleDim.Render("  (t shows the transfers)")
	if m.view == viewTransfers {
		hint = "" // they are on screen already
	}

	if len(running) > 1 {
		var done, total int

		ratio := 0.0

		for _, entry := range running {
			done, total = done+entry.done, total+entry.total
			ratio += entry.ratio()
		}

		return strings.Join([]string{
			styleAccent.Render(fmt.Sprintf("⇅ %d transfers", len(running))),
			m.progress.ViewAs(ratio / float64(len(running))),
			fmt.Sprintf("%d/%d", done, total),
		}, " ") + hint
	}

	entry := running[0]

	parts := []string{
		styleAccent.Render(entry.kind.arrow() + " " + entry.kind.String()),
		m.progress.ViewAs(entry.ratio()),
		fmt.Sprintf("%d/%d", entry.done, entry.total),
	}

	if entry.current != "" {
		parts = append(parts, short(path.Base(entry.current), 28))
	}

	if entry.curTotal > 0 {
		parts = append(parts, fmt.Sprintf("%s/%s",
			humanize.IBytes(uint64(max(0, entry.curDone))),
			humanize.IBytes(uint64(entry.curTotal)),
		))
	}

	return strings.Join(parts, " ") + hint
}

func (m Model) clamp(line string) string {
	if m.width <= 2 {
		return line
	}

	return lipgloss.NewStyle().MaxWidth(m.width - 2).Render(line)
}

func parentPrefix(prefix string) string {
	trimmed := strings.TrimSuffix(prefix, "/")

	idx := strings.LastIndex(trimmed, "/")
	if idx < 0 {
		return ""
	}

	return trimmed[:idx+1]
}

// indentBlock indents a whole JSON document, empty stays empty.
func indentBlock(text string) string {
	if text == "" {
		return ""
	}

	return indent(text, "  ")
}

// indent prefixes every line.
func indent(text, prefix string) string {
	lines := strings.Split(strings.TrimRight(text, "\n"), "\n")
	for i, line := range lines {
		lines[i] = prefix + line
	}

	return strings.Join(lines, "\n")
}

func short(s string, width int) string {
	if len(s) <= width {
		return s
	}

	return s[:width-1] + "…"
}
