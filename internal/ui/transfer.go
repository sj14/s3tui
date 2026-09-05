package ui

import (
	"context"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path"
	"path/filepath"
	"strings"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/feature/s3/manager"
	"github.com/aws/aws-sdk-go-v2/service/s3"
	"github.com/aws/aws-sdk-go-v2/service/s3/types"
	"github.com/charmbracelet/bubbles/textinput"
	tea "github.com/charmbracelet/bubbletea"
	"github.com/dustin/go-humanize"
)

// progressInterval throttles the progress messages of a running transfer.
const progressInterval = 100 * time.Millisecond

// The transfer manager writes a whole part at once, so the part size is what
// decides how often the progress bar can move: at 1 MiB it is one step per
// second on a 1 MB/s line, and more often on a faster one. The smaller parts
// are paid for with more requests, which the concurrency makes up for by
// keeping about as much data in flight as the 5 MiB default did.
const (
	downloadPartSize    = 1 << 20 // 1 MiB
	downloadConcurrency = 10
)

type transferKind int

const (
	transferUpload transferKind = iota
	transferDownload
	transferCopy
	transferMove
	transferDelete
)

func (k transferKind) String() string {
	switch k {
	case transferUpload:
		return "upload"
	case transferDownload:
		return "download"
	case transferMove:
		return "move"
	case transferDelete:
		return "delete"
	default:
		return "copy"
	}
}

// overwriteReply is the answer to a transferAskMsg.
type overwriteReply int

const (
	replyYes overwriteReply = iota
	replyNo
	replyAll
	replySkipAll
)

// transferMsg carries one event of one transfer. Several transfers run at the
// same time and share the update loop, so every event says where it belongs.
type transferMsg struct {
	id  int
	msg tea.Msg
}

// transferAsk is an overwrite question waiting for an answer, together with
// the transfer that asked it.
type transferAsk struct {
	id    int
	path  string
	reply chan overwriteReply
}

// transferProgressMsg reports the state of the running transfer.
type transferProgressMsg struct {
	done     int
	total    int
	current  string
	curDone  int64
	curTotal int64
}

// transferAskMsg asks the user whether an existing file may be overwritten.
type transferAskMsg struct {
	path  string
	reply chan overwriteReply
}

// transferDoneMsg ends a transfer.
type transferDoneMsg struct {
	summary string
	err     error
}

// transfer is the UI side of one upload, download, copy or delete. It stays
// in the list once it ended, with what became of it.
type transfer struct {
	id     int // handed out by the model, every event carries it
	kind   transferKind
	label  string
	events chan tea.Msg
	cancel context.CancelFunc

	done     int
	total    int
	current  string
	curDone  int64
	curTotal int64

	finished bool
	canceled bool
	summary  string
	err      error
}

// ratio returns the overall progress between 0 and 1.
func (t *transfer) ratio() float64 {
	if t.total <= 0 {
		return 0
	}

	progress := float64(t.done)
	if t.curTotal > 0 {
		progress += float64(t.curDone) / float64(t.curTotal)
	}

	return min(1, progress/float64(t.total))
}

// waitForTransfer blocks until the transfer reports its next event and stamps
// it with the transfer it came from.
func waitForTransfer(entry *transfer) tea.Cmd {
	return func() tea.Msg {
		msg, ok := <-entry.events
		if !ok {
			// the goroutine ended without a last word, do not wait forever
			return transferMsg{id: entry.id, msg: transferDoneMsg{}}
		}

		return transferMsg{id: entry.id, msg: msg}
	}
}

// answerOverwrite hands the decision back to the running transfer.
func answerOverwrite(reply chan overwriteReply, answer overwriteReply) tea.Cmd {
	return func() tea.Msg {
		reply <- answer // buffered, never blocks

		return nil
	}
}

// reporter feeds the events channel of a transfer.
type reporter struct {
	events chan tea.Msg
	ctx    context.Context

	total    int
	done     int
	current  string
	curDone  int64
	curTotal int64
	lastSent time.Time

	overwriteAll bool
	skipAll      bool
}

func (r *reporter) startFile(name string, size int64) {
	r.current, r.curDone, r.curTotal = name, 0, size
	r.lastSent = time.Time{}
	r.emit()
}

func (r *reporter) add(n int64) {
	r.curDone += n

	if time.Since(r.lastSent) < progressInterval {
		return
	}

	r.emit()
}

// finishFiles marks a whole batch as done.
func (r *reporter) finishFiles(n int) {
	r.done += n
	r.curDone, r.curTotal = 0, 0
	r.lastSent = time.Time{}
	r.emit()
}

func (r *reporter) finishFile() {
	r.done++
	r.curDone, r.curTotal = 0, 0
	r.lastSent = time.Time{}
	r.emit()
}

// emit never blocks: a dropped progress update is corrected by the next one.
func (r *reporter) emit() {
	r.lastSent = time.Now()

	msg := transferProgressMsg{
		done:     r.done,
		total:    r.total,
		current:  r.current,
		curDone:  r.curDone,
		curTotal: r.curTotal,
	}

	select {
	case r.events <- msg:
	default:
	}
}

// mayOverwrite asks the UI about an existing file and remembers a bulk answer.
func (r *reporter) mayOverwrite(path string) (bool, error) {
	switch {
	case r.overwriteAll:
		return true, nil
	case r.skipAll:
		return false, nil
	}

	reply := make(chan overwriteReply, 1)

	select {
	case r.events <- transferAskMsg{path: path, reply: reply}:
	case <-r.ctx.Done():
		return false, r.ctx.Err()
	}

	select {
	case answer := <-reply:
		switch answer {
		case replyAll:
			r.overwriteAll = true

			return true, nil
		case replySkipAll:
			r.skipAll = true

			return false, nil
		case replyYes:
			return true, nil
		default:
			return false, nil
		}
	case <-r.ctx.Done():
		return false, r.ctx.Err()
	}
}

// countingReader reports every byte read to the reporter.
type countingReader struct {
	reader   io.Reader
	reporter *reporter
}

func (c *countingReader) Read(p []byte) (int, error) {
	n, err := c.reader.Read(p)
	c.reporter.add(int64(n))

	return n, err
}

// countingWriterAt reports every byte written to the reporter.
type countingWriterAt struct {
	writer   io.WriterAt
	reporter *reporter
}

func (c *countingWriterAt) WriteAt(p []byte, off int64) (int, error) {
	n, err := c.writer.WriteAt(p, off)
	c.reporter.add(int64(n))

	return n, err
}

type uploadFile struct {
	path string // local path
	key  string // target key
	size int64
}

// startUpload uploads a file or a directory into bucket/prefix. Local errors
// are reported right away, the transfer itself runs in the background.
func startUpload(parent context.Context, client *s3.Client, bucket, prefix, local string) (*transfer, error) {
	local = expandPath(local)

	info, err := os.Stat(local)
	if err != nil {
		return nil, err
	}

	var (
		files []uploadFile
		total int64
	)

	if info.IsDir() {
		base := filepath.Base(strings.TrimSuffix(local, string(filepath.Separator)))

		err := filepath.WalkDir(local, func(path string, entry fs.DirEntry, err error) error {
			if err != nil {
				return err
			}
			if !entry.Type().IsRegular() {
				return nil
			}

			rel, err := filepath.Rel(local, path)
			if err != nil {
				return err
			}

			stat, err := entry.Info()
			if err != nil {
				return err
			}

			files = append(files, uploadFile{
				path: path,
				key:  prefix + base + "/" + filepath.ToSlash(rel),
				size: stat.Size(),
			})
			total += stat.Size()

			return nil
		})
		if err != nil {
			return nil, err
		}
	} else {
		files = append(files, uploadFile{path: local, key: prefix + filepath.Base(local), size: info.Size()})
		total = info.Size()
	}

	if len(files) == 0 {
		return nil, fmt.Errorf("%s contains no files", local)
	}

	ctx, cancel := context.WithCancel(parent)

	transfer := &transfer{
		kind:   transferUpload,
		label:  fmt.Sprintf("%s → s3://%s/%s", local, bucket, prefix),
		events: make(chan tea.Msg, 16),
		cancel: cancel,
		total:  len(files),
	}

	go runUpload(ctx, client, transfer, bucket, files, total)

	return transfer, nil
}

func runUpload(ctx context.Context, client *s3.Client, transfer *transfer, bucket string, files []uploadFile, total int64) {
	defer close(transfer.events)

	report := &reporter{events: transfer.events, ctx: ctx, total: len(files)}
	uploader := manager.NewUploader(client)

	var uploaded int64

	for _, file := range files {
		if ctx.Err() != nil {
			break
		}

		report.startFile(file.key, file.size)

		handle, err := os.Open(file.path)
		if err != nil {
			transfer.events <- transferDoneMsg{err: err}

			return
		}

		_, err = uploader.Upload(ctx, &s3.PutObjectInput{
			Bucket: aws.String(bucket),
			Key:    aws.String(file.key),
			Body:   &countingReader{reader: handle, reporter: report},
		})
		handle.Close()

		if err != nil {
			transfer.events <- transferDoneMsg{err: transferErr(ctx, err), summary: summary("uploaded", report.done, len(files), uploaded)}

			return
		}

		uploaded += file.size
		report.finishFile()
	}

	transfer.events <- transferDoneMsg{
		err:     transferErr(ctx, nil),
		summary: summary("uploaded", report.done, len(files), uploaded),
	}
}

// versionIDOrNil turns the empty string into "the newest version".
func versionIDOrNil(versionID string) *string {
	if versionID == "" {
		return nil
	}

	return aws.String(versionID)
}

// startDownload downloads a single object, a single version or a whole prefix.
func startDownload(parent context.Context, client *s3.Client, bucket, key, versionID, target string, recursive bool) (*transfer, error) {
	target = expandPath(target)

	if recursive {
		if err := os.MkdirAll(target, 0o755); err != nil {
			return nil, err
		}
	} else if dir := filepath.Dir(target); dir != "" {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			return nil, err
		}
	}

	ctx, cancel := context.WithCancel(parent)

	transfer := &transfer{
		kind:   transferDownload,
		label:  fmt.Sprintf("s3://%s/%s → %s", bucket, key, target),
		events: make(chan tea.Msg, 16),
		cancel: cancel,
		total:  1,
	}

	go runDownload(ctx, client, transfer, bucket, key, versionID, target, recursive)

	return transfer, nil
}

func runDownload(ctx context.Context, client *s3.Client, transfer *transfer, bucket, key, versionID, target string, recursive bool) {
	defer close(transfer.events)

	report := &reporter{events: transfer.events, ctx: ctx, total: 1}
	downloader := manager.NewDownloader(client, func(d *manager.Downloader) {
		d.PartSize = downloadPartSize
		d.Concurrency = downloadConcurrency
	})

	type remoteFile struct {
		key   string
		local string
		size  int64
	}

	var files []remoteFile

	if recursive {
		listed, err := listAllObjects(ctx, client, bucket, key)
		if err != nil {
			transfer.events <- transferDoneMsg{err: transferErr(ctx, err)}

			return
		}

		for _, object := range listed {
			objectKey := aws.ToString(object.Key)
			if strings.HasSuffix(objectKey, "/") {
				continue // placeholder object of a "folder"
			}

			local, err := safeJoin(target, strings.TrimPrefix(objectKey, key))
			if err != nil {
				transfer.events <- transferDoneMsg{err: err}

				return
			}

			files = append(files, remoteFile{key: objectKey, local: local, size: aws.ToInt64(object.Size)})
		}

		if len(files) == 0 {
			transfer.events <- transferDoneMsg{summary: "nothing to download"}

			return
		}
	} else {
		// Without the size the progress of a single file cannot be known at
		// all: the bar would sit at zero until the last byte arrived. A failed
		// HeadObject is not fatal, the download itself will say what is wrong.
		file := remoteFile{key: key, local: target}

		head, err := client.HeadObject(ctx, &s3.HeadObjectInput{
			Bucket:    aws.String(bucket),
			Key:       aws.String(key),
			VersionId: versionIDOrNil(versionID),
		})
		if err == nil {
			file.size = aws.ToInt64(head.ContentLength)
		}

		files = append(files, file)
	}

	report.total = len(files)

	var (
		written int64
		skipped int
	)

	for _, file := range files {
		if ctx.Err() != nil {
			break
		}

		if _, err := os.Stat(file.local); err == nil {
			allowed, err := report.mayOverwrite(file.local)
			if err != nil {
				break
			}
			if !allowed {
				skipped++
				report.finishFile()

				continue
			}
		}

		report.startFile(file.key, file.size)

		if err := os.MkdirAll(filepath.Dir(file.local), 0o755); err != nil {
			transfer.events <- transferDoneMsg{err: err}

			return
		}

		// Write next to the target and rename only once everything arrived: a
		// download that fails must leave neither half a file behind nor take
		// an existing one with it.
		handle, err := os.CreateTemp(filepath.Dir(file.local), "."+filepath.Base(file.local)+".*.s3tui")
		if err != nil {
			transfer.events <- transferDoneMsg{err: err}

			return
		}

		partial := handle.Name()

		input := &s3.GetObjectInput{Bucket: aws.String(bucket), Key: aws.String(file.key)}
		if versionID != "" {
			input.VersionId = aws.String(versionID)
		}

		n, err := downloader.Download(ctx, &countingWriterAt{writer: handle, reporter: report}, input)
		handle.Close()

		if err == nil {
			// CreateTemp is owner-only, an ordinary download is not
			if err = os.Chmod(partial, 0o644); err == nil {
				err = os.Rename(partial, file.local)
			}
		}

		if err != nil {
			os.Remove(partial)

			transfer.events <- transferDoneMsg{
				err:     transferErr(ctx, err),
				summary: downloadSummary(report.done, len(files), skipped, written),
			}

			return
		}

		written += n
		report.finishFile()
	}

	transfer.events <- transferDoneMsg{
		err:     transferErr(ctx, nil),
		summary: downloadSummary(report.done-skipped, len(files), skipped, written),
	}
}

// listAllObjects lists every object below prefix, without a delimiter.
func listAllObjects(ctx context.Context, client *s3.Client, bucket, prefix string) ([]types.Object, error) {
	var objects []types.Object

	paginator := s3.NewListObjectsV2Paginator(client, &s3.ListObjectsV2Input{
		Bucket:  aws.String(bucket),
		Prefix:  aws.String(prefix),
		MaxKeys: aws.Int32(pageSize),
	})

	for paginator.HasMorePages() {
		page, err := paginator.NextPage(ctx)
		if err != nil {
			return nil, err
		}

		objects = append(objects, page.Contents...)
	}

	return objects, nil
}

// safeJoin joins base and rel and makes sure the result stays below base.
func safeJoin(base, rel string) (string, error) {
	joined := filepath.Join(base, filepath.FromSlash(rel))

	if joined != base && !strings.HasPrefix(joined, base+string(filepath.Separator)) {
		return "", fmt.Errorf("key %q would escape %q", rel, base)
	}

	return joined, nil
}

// expandPath resolves a leading ~ and cleans the path.
func expandPath(path string) string {
	// an absolute path typed behind a "./" prefill would become relative
	if rest := strings.TrimPrefix(path, "./"); rest != path &&
		(strings.HasPrefix(rest, "/") || strings.HasPrefix(rest, "~")) {
		path = rest
	}

	if path == "~" || strings.HasPrefix(path, "~/") {
		if home, err := os.UserHomeDir(); err == nil {
			path = filepath.Join(home, strings.TrimPrefix(path, "~"))
		}
	}

	return filepath.Clean(path)
}

// transferErr turns a cancelation into a readable error.
func transferErr(ctx context.Context, err error) error {
	if ctx.Err() != nil && (err == nil || errors.Is(err, context.Canceled)) {
		return context.Canceled
	}

	return err
}

func summary(verb string, done, total int, bytes int64) string {
	return fmt.Sprintf("%s %d/%d files (%s)", verb, done, total, humanize.IBytes(uint64(max(0, bytes))))
}

func downloadSummary(done, total, skipped int, bytes int64) string {
	text := summary("downloaded", done, total, bytes)
	if skipped > 0 {
		text += fmt.Sprintf(", %d skipped", skipped)
	}

	return text
}

// askUpload opens the local file browser, or uploads the highlighted entry
// when the browser is already open.
func (m Model) askUpload() (tea.Model, tea.Cmd) {
	if m.view == viewPicker {
		selected, ok := m.picker.SelectedItem().(localItem)
		if !ok || selected.up {
			return m, nil
		}

		return m.startUploadOf(selected.path)
	}

	if m.view != viewObjects {
		m.status = "open a bucket first to upload into it"

		return m, nil
	}

	if m.profileCfg.ReadOnly {
		m.status = "profile is read-only, upload blocked"

		return m, nil
	}

	if m.uploadDir == "" {
		m.uploadDir, _ = os.Getwd()
	}

	m.prevView = m.view
	m.view = viewPicker
	m.err, m.status = nil, ""
	m.picker.SetItems(nil)
	m.picker.ResetFilter()

	return m, readLocalDirCmd(m.ctx, m.uploadDir)
}

// openLocal walks into the highlighted directory, files are not opened.
func (m Model) openLocal() (tea.Model, tea.Cmd) {
	selected, ok := m.picker.SelectedItem().(localItem)
	if !ok || !selected.isDir {
		return m, nil
	}

	m.uploadDir = selected.path
	m.err, m.status = nil, ""
	m.picker.SetItems(nil)
	m.picker.ResetFilter()

	return m, readLocalDirCmd(m.ctx, m.uploadDir)
}

// startUploadOf uploads the picked file or directory into the current prefix.
func (m Model) startUploadOf(path string) (tea.Model, tea.Cmd) {
	m.view = m.prevView

	client, err := m.client.ForBucket(m.ctx, m.bucket)
	if err != nil {
		m.err = fmt.Errorf("resolving bucket region: %w", err)

		return m, nil
	}

	transfer, err := startUpload(m.ctx, client, m.bucket, m.prefix, path)
	if err != nil {
		m.err = fmt.Errorf("upload: %w", err)

		return m, nil
	}

	return m, m.track(transfer)
}

// askDownload opens the prompt for the local target of the selection.
func (m Model) askDownload() (tea.Model, tea.Cmd) {
	prompt := &promptState{kind: promptDownload, bucket: m.bucket}

	switch m.view {
	case viewObjects:
		selected, note, ok := m.selectedObject("download")
		if !ok {
			m.status = note

			return m, nil
		}

		prompt.key = selected.key
		prompt.recursive = selected.isPrefix

		name := path.Base(strings.TrimSuffix(selected.key, "/"))
		prompt.title = fmt.Sprintf("download s3://%s/%s to", m.bucket, selected.key)
		prompt = m.fillPrompt(prompt, "./"+name)

	case viewVersions:
		selected, ok := m.versions.SelectedItem().(versionItem)
		if !ok {
			return m, nil
		}

		if selected.deleteMarker {
			m.status = "a delete marker has no content to download"

			return m, nil
		}

		prompt.key = m.key
		prompt.versionID = selected.versionID
		prompt.title = fmt.Sprintf("download version %s to", short(selected.versionID, 12))
		prompt = m.fillPrompt(prompt, "./"+path.Base(m.key))

	default:
		m.status = "select an object or a prefix to download"

		return m, nil
	}

	m.prompt = prompt

	return m, textinput.Blink
}

func (m Model) fillPrompt(prompt *promptState, value string) *promptState {
	filled := newPrompt(prompt.kind, prompt.title, value, m.width)
	filled.bucket, filled.key = prompt.bucket, prompt.key
	filled.versionID, filled.recursive = prompt.versionID, prompt.recursive
	filled.size = prompt.size

	return filled
}

// startTransfer launches the upload or download of a submitted prompt.
func (m Model) startTransfer(prompt *promptState) (tea.Model, tea.Cmd) {
	target := strings.TrimSpace(prompt.input.Value())

	if prompt.kind == promptSearch {
		return m.runSearch(target)
	}

	if target == "" {
		return m, nil
	}

	client, err := m.client.ForBucket(m.ctx, m.bucket)
	if err != nil {
		m.err = fmt.Errorf("resolving bucket region: %w", err)

		return m, nil
	}

	var transfer *transfer

	switch prompt.kind {
	case promptDownload:
		transfer, err = startDownload(m.ctx, client, prompt.bucket, prompt.key, prompt.versionID, target, prompt.recursive)

	case promptCopy, promptMove:
		kind := transferCopy
		if prompt.kind == promptMove {
			kind = transferMove
		}

		var destination copyRef

		destination, err = parseTarget(target, prompt.bucket)
		if err == nil {
			source := copyRef{
				bucket:    prompt.bucket,
				key:       prompt.key,
				versionID: prompt.versionID,
				recursive: prompt.recursive,
				size:      prompt.size,
			}

			transfer, err = startCopy(m.ctx, client, kind, source, destination)
		}

	case promptRandom:
		var spec randomSpec

		spec, err = parseRandomSpec(target)
		if err == nil {
			transfer, err = startRandomUpload(m.ctx, client, m.bucket, m.prefix, spec)
		}
	}

	if err != nil {
		m.err = fmt.Errorf("%s: %w", prompt.kind, err)

		return m, nil
	}

	// the file browser has nothing to do with a random upload, go back to the
	// listing the objects will show up in
	if prompt.kind == promptRandom && m.view == viewPicker {
		m.view = m.prevView
	}

	return m, m.track(transfer)
}
