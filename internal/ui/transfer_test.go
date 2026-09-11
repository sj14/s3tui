package ui

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/aws/aws-sdk-go-v2/service/s3"
	"github.com/charmbracelet/bubbles/list"
	tea "github.com/charmbracelet/bubbletea"

	"github.com/sj14/s3tui/internal/awsclient"
	"github.com/sj14/s3tui/internal/config"
)

// press sends a single rune key.
func press(t *testing.T, model tea.Model, r rune) tea.Model {
	t.Helper()

	return step(t, model, tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{r}})
}

// pressNoRun sends a key without running the returned command. It is used for
// the keys which only open a prompt, whose blink command would block the test.
func pressNoRun(model tea.Model, r rune) tea.Model {
	updated, _ := model.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{r}})

	return updated
}

// openBucketMenu selects bucket-a and stops at its overview.
func openBucketMenu(t *testing.T, server *fakeServer) tea.Model {
	t.Helper()

	return enter(t, newTestModel(t, server.URL))
}

// openSection picks one entry of the bucket overview.
func openSection(t *testing.T, model tea.Model, kind menuKind) tea.Model {
	t.Helper()

	return pickSection(t, model, func(item menuItem) bool { return item.kind == kind })
}

// openConfigSection picks the entry of one bucket configuration.
func openConfigSection(t *testing.T, model tea.Model, kind configKind) tea.Model {
	t.Helper()

	return pickSection(t, model, func(item menuItem) bool {
		return item.kind == menuConfig && item.config == kind
	})
}

// openDetails picks the metadata entry of the object overview.
func openDetails(t *testing.T, model tea.Model) tea.Model {
	t.Helper()

	return openConfigSection(t, model, configObjectMetadata)
}

// pickSection picks one entry of the bucket or the object overview.
func pickSection(t *testing.T, model tea.Model, match func(menuItem) bool) tea.Model {
	t.Helper()

	inner := model.(Model)

	menu := &inner.menu

	switch inner.view {
	case viewBucketMenu:
	case viewObjectMenu:
		menu = &inner.objectMenu
	default:
		t.Fatalf("view = %v, want an overview", inner.view)
	}

	index := -1
	for i, item := range menu.Items() {
		if match(item.(menuItem)) {
			index = i

			break
		}
	}

	if index < 0 {
		t.Fatal("the overview has no such entry")
	}

	menu.Select(index)

	return enter(t, tea.Model(inner))
}

// openBucket lists the objects of bucket-a.
func openBucket(t *testing.T, server *fakeServer) tea.Model {
	t.Helper()

	return openSection(t, openBucketMenu(t, server), menuObjects)
}

// browseTo opens the file browser and points it at dir.
func browseTo(t *testing.T, model tea.Model, dir string) tea.Model {
	t.Helper()

	model = press(t, model, 'u')

	if got := model.(Model).view; got != viewPicker {
		t.Fatalf("view = %v, want the file browser", got)
	}

	inner := model.(Model)
	inner.uploadDir = dir

	return runCmd(t, tea.Model(inner), readLocalDirCmd(inner.ctx, dir), 0)
}

// pickUpload browses to dir and uploads the entry at position index with "u".
// Index 0 is the ".." entry.
func pickUpload(t *testing.T, model tea.Model, dir string, index int) tea.Model {
	t.Helper()

	model = browseTo(t, model, dir)

	for range index {
		model = step(t, model, tea.KeyMsg{Type: tea.KeyDown})
	}

	return press(t, model, 'u')
}

// submitPrompt puts value into the open prompt and confirms it.
func submitPrompt(t *testing.T, model tea.Model, value string) tea.Model {
	t.Helper()

	prompt := model.(Model).prompt
	if prompt == nil {
		t.Fatal("no prompt is open")
	}

	prompt.input.SetValue(value)

	return enter(t, model)
}

func TestUploadFile(t *testing.T) {
	server := fakeS3(t)
	model := openBucket(t, server)

	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "hello.txt"), []byte("hello world"), 0o600); err != nil {
		t.Fatal(err)
	}

	model = pickUpload(t, model, dir, 1) // 0 is ".."

	if got := model.(Model).view; got != viewObjects {
		t.Errorf("view = %v, want objects after the upload", got)
	}

	if got := model.(Model).running(); len(got) != 0 {
		t.Errorf("transfer still running: %+v", got)
	}

	uploaded := server.uploaded()
	if body, ok := uploaded["hello.txt"]; !ok || string(body) != "hello world" {
		t.Fatalf("unexpected uploads: %v", keysOf(uploaded))
	}

	if status := model.(Model).status; !strings.Contains(status, "uploaded 1/1") {
		t.Errorf("status = %q", status)
	}
}

func TestUploadDirectoryRecursively(t *testing.T) {
	server := fakeS3(t)
	model := openBucket(t, server)

	parent := t.TempDir()
	dir := filepath.Join(parent, "assets")
	if err := os.MkdirAll(filepath.Join(dir, "sub"), 0o755); err != nil {
		t.Fatal(err)
	}
	for name, content := range map[string]string{"a.txt": "aaa", "sub/b.txt": "bbb"} {
		if err := os.WriteFile(filepath.Join(dir, filepath.FromSlash(name)), []byte(content), 0o600); err != nil {
			t.Fatal(err)
		}
	}

	// "u" on the highlighted directory uploads it recursively
	model = pickUpload(t, model, parent, 1) // 0 is ".."

	uploaded := server.uploaded()
	for _, want := range []string{"assets/a.txt", "assets/sub/b.txt"} {
		if _, ok := uploaded[want]; !ok {
			t.Errorf("missing upload %q, got %v", want, keysOf(uploaded))
		}
	}

	if status := model.(Model).status; !strings.Contains(status, "uploaded 2/2") {
		t.Errorf("status = %q", status)
	}
}

func TestUploadBlockedInReadOnlyProfile(t *testing.T) {
	server := fakeS3(t)

	model := newTestModel(t, server.URL)
	inner := model.(Model)
	inner.profileCfg.ReadOnly = true
	model = openSection(t, enter(t, tea.Model(inner)), menuObjects)

	model = pressNoRun(model, 'u')

	if model.(Model).prompt != nil {
		t.Error("a prompt was opened in a read-only profile")
	}
	if status := model.(Model).status; !strings.Contains(status, "read-only") {
		t.Errorf("status = %q", status)
	}
}

func TestDownloadObject(t *testing.T) {
	server := fakeS3(t)
	model := openBucket(t, server)

	inner := model.(Model)
	inner.objects.Select(1) // file.txt
	model = pressNoRun(tea.Model(inner), 'l')

	prompt := model.(Model).prompt
	if prompt == nil {
		t.Fatal("no prompt is open")
	}
	if got := prompt.input.Value(); got != "./file.txt" {
		t.Errorf("prefilled path = %q, want ./file.txt", got)
	}

	target := filepath.Join(t.TempDir(), "out.txt")
	model = submitPrompt(t, model, target)

	content, err := os.ReadFile(target)
	if err != nil {
		t.Fatalf("reading the download: %v", err)
	}
	if len(content) != 200 {
		t.Errorf("downloaded %d bytes, want 200", len(content))
	}

	if status := model.(Model).status; !strings.Contains(status, "downloaded 1/1") {
		t.Errorf("status = %q", status)
	}
}

func TestDownloadPrefixRecursively(t *testing.T) {
	server := fakeS3(t)
	model := openBucket(t, server)

	inner := model.(Model)
	inner.objects.Select(0) // the logs/ prefix
	model = pressNoRun(tea.Model(inner), 'l')

	if got := model.(Model).prompt.input.Value(); got != "./logs" {
		t.Errorf("prefilled path = %q, want ./logs", got)
	}

	target := filepath.Join(t.TempDir(), "logs")
	model = submitPrompt(t, model, target)

	content, err := os.ReadFile(filepath.Join(target, "nested.log"))
	if err != nil {
		t.Fatalf("reading the download: %v", err)
	}
	if len(content) != 42 {
		t.Errorf("downloaded %d bytes, want 42", len(content))
	}
}

func TestDownloadAsksBeforeOverwriting(t *testing.T) {
	server := fakeS3(t)

	target := filepath.Join(t.TempDir(), "out.txt")
	if err := os.WriteFile(target, []byte("keep me"), 0o600); err != nil {
		t.Fatal(err)
	}

	// answering "n" keeps the file
	model := openBucket(t, server)
	inner := model.(Model)
	inner.objects.Select(1)
	model = pressNoRun(tea.Model(inner), 'l')
	model = submitPrompt(t, model, target)

	if len(model.(Model).confirms) == 0 {
		t.Fatal("no overwrite question was asked")
	}

	model = press(t, model, 'n')

	content, err := os.ReadFile(target)
	if err != nil {
		t.Fatal(err)
	}
	if string(content) != "keep me" {
		t.Errorf("file was overwritten: %q", content)
	}
	if status := model.(Model).status; !strings.Contains(status, "1 skipped") {
		t.Errorf("status = %q", status)
	}

	// answering "a" overwrites
	model = openBucket(t, server)
	inner = model.(Model)
	inner.objects.Select(1)
	model = pressNoRun(tea.Model(inner), 'l')
	model = submitPrompt(t, model, target)

	if len(model.(Model).confirms) == 0 {
		t.Fatal("no overwrite question was asked")
	}

	model = press(t, model, 'a')

	content, err = os.ReadFile(target)
	if err != nil {
		t.Fatal(err)
	}
	if len(content) != 200 {
		t.Errorf("file was not overwritten: %d bytes", len(content))
	}
}

func TestDownloadVersion(t *testing.T) {
	server := fakeS3(t)
	model := openBucket(t, server)

	inner := model.(Model)
	inner.objects.Select(1) // file.txt
	model = enter(t, tea.Model(inner))

	if got := model.(Model).view; got != viewVersions {
		t.Fatalf("view = %v, want versions", got)
	}

	// the delete marker has no content
	inner = model.(Model)
	inner.versions.Select(2)
	model = pressNoRun(tea.Model(inner), 'l')

	if model.(Model).prompt != nil {
		t.Error("a delete marker must not open a download prompt")
	}

	inner = model.(Model)
	inner.versions.Select(1) // the older version
	model = pressNoRun(tea.Model(inner), 'l')

	prompt := model.(Model).prompt
	if prompt == nil {
		t.Fatal("no prompt is open")
	}
	if prompt.versionID != "v1" {
		t.Errorf("versionID = %q, want v1", prompt.versionID)
	}

	target := filepath.Join(t.TempDir(), "old.txt")
	model = submitPrompt(t, model, target)

	if _, err := os.Stat(target); err != nil {
		t.Fatalf("version was not downloaded: %v", err)
	}
}

func TestSwitchProfile(t *testing.T) {
	server := fakeS3(t)

	// profiles are the level above the buckets: objects, overview, buckets
	model := openBucket(t, server) // we are inside bucket-a
	for range 3 {
		model = step(t, model, tea.KeyMsg{Type: tea.KeyEsc})
	}

	if got := model.(Model).view; got != viewProfiles {
		t.Fatalf("view = %v, want profiles", got)
	}
	if got := len(model.(Model).profiles.Items()); got != 2 {
		t.Fatalf("got %d profiles, want 2", got)
	}

	inner := model.(Model)
	inner.profiles.Select(1) // "other", sorted after "default"
	model = enter(t, tea.Model(inner))

	got := model.(Model)
	if got.profile != "other" {
		t.Errorf("profile = %q, want other", got.profile)
	}
	if got.view != viewBuckets {
		t.Errorf("view = %v, want buckets", got.view)
	}
	if got.bucket != "" {
		t.Errorf("bucket = %q, want empty", got.bucket)
	}
	if len(got.buckets.Items()) == 0 {
		t.Error("the buckets were not reloaded")
	}
	if !strings.Contains(got.status, "switched to profile other") {
		t.Errorf("status = %q", got.status)
	}
}

func TestProfileSwitchDropsStaleBucketListing(t *testing.T) {
	server := fakeS3(t)
	model := newTestModel(t, server.URL)

	inner := model.(Model)
	client := inner.client

	// Profile "other" connects and starts a bucket listing.
	inner.switchRun = 1
	inner.inFlight++
	model, _ = inner.Update(profileSwitchedMsg{
		run: 1, name: "other", profile: inner.cfg.Profiles["other"], client: client,
	})
	otherRun := model.(Model).profileRun

	// Before that listing returns, switch back to "default" and start its
	// listing. The earlier response must not replace the current buckets.
	inner = model.(Model)
	inner.switchRun = 2
	inner.inFlight++
	model, _ = inner.Update(profileSwitchedMsg{
		run: 2, name: "default", profile: inner.cfg.Profiles["default"], client: client,
	})
	defaultRun := model.(Model).profileRun

	model, _ = model.Update(bucketsMsg{
		run: otherRun,
		items: []list.Item{
			bucketItem{name: "wrong-profile-bucket"},
		},
	})
	if got := len(model.(Model).buckets.Items()); got != 0 {
		t.Fatalf("stale listing installed %d buckets", got)
	}

	model, _ = model.Update(bucketsMsg{
		run: defaultRun,
		items: []list.Item{
			bucketItem{name: "default-bucket"},
		},
	})
	items := model.(Model).buckets.Items()
	if len(items) != 1 || items[0].(bucketItem).name != "default-bucket" {
		t.Fatalf("current listing was not installed: %+v", items)
	}
}

func TestLatestProfileConnectionWins(t *testing.T) {
	server := fakeS3(t)
	model := newTestModel(t, server.URL)

	inner := model.(Model)
	inner.switchRun = 2
	inner.inFlight += 2

	model, _ = inner.Update(profileSwitchedMsg{
		run: 1, name: "other", profile: inner.cfg.Profiles["other"], client: inner.client,
	})
	if got := model.(Model).profile; got != "default" {
		t.Fatalf("stale connection switched to profile %q", got)
	}
}

func TestBackCancelsForegroundLoad(t *testing.T) {
	server := fakeS3(t)
	inner := newTestModel(t, server.URL).(Model)
	inner.bucket = "bucket-a"
	inner.view = viewObjects
	inner.startBucket()
	ctx := inner.startLoad()
	bucketRun, loadRun := inner.bucketRun, inner.loadRun
	inner.inFlight++

	model, _ := inner.back()
	select {
	case <-ctx.Done():
	default:
		t.Fatal("leaving the view did not cancel its request context")
	}

	// Even if the command completed just before cancellation and its message
	// was already queued, its error is ignored with the obsolete generation.
	model, _ = model.Update(loadScopedMsg{
		bucketRun: bucketRun,
		loadRun:   loadRun,
		msg:       errMsg{what: "listing objects", err: context.Canceled},
	})
	if err := model.(Model).err; err != nil {
		t.Errorf("obsolete cancellation became an error: %v", err)
	}
}

func TestBucketGenerationRejectsABAResponse(t *testing.T) {
	server := fakeS3(t)
	inner := newTestModel(t, server.URL).(Model)

	inner.bucket = "bucket-a"
	inner.view = viewObjects
	inner.startBucket()
	inner.startLoad()
	oldBucketRun, oldLoadRun := inner.bucketRun, inner.loadRun

	inner.stopBucket()
	inner.bucket = "bucket-b"
	inner.startBucket()
	inner.stopBucket()
	inner.bucket = "bucket-a"
	inner.startBucket()
	inner.startLoad()
	inner.inFlight++

	model, _ := inner.Update(loadScopedMsg{
		bucketRun: oldBucketRun,
		loadRun:   oldLoadRun,
		msg: objectsMsg{
			bucket: "bucket-a",
			items:  []list.Item{objectItem{name: "stale.txt", key: "stale.txt"}},
		},
	})
	if got := len(model.(Model).objects.Items()); got != 0 {
		t.Fatalf("the first visit to bucket-a installed %d stale objects after returning", got)
	}
}

func TestProfileSwitcherCancels(t *testing.T) {
	server := fakeS3(t)

	model := openBucket(t, server)
	for range 3 {
		model = step(t, model, tea.KeyMsg{Type: tea.KeyEsc}) // objects, overview, buckets
	}
	model = step(t, model, tea.KeyMsg{Type: tea.KeyEsc}) // profiles
	model = step(t, model, tea.KeyMsg{Type: tea.KeyEsc}) // the root, nothing happens

	got := model.(Model)
	if got.view != viewProfiles {
		t.Errorf("view = %v, the profile list is the root", got.view)
	}
	if got.profile != "default" {
		t.Errorf("profile = %q, want default", got.profile)
	}
}

func TestSafeJoin(t *testing.T) {
	base := filepath.Join(string(filepath.Separator), "tmp", "dl")

	if _, err := safeJoin(base, "../../etc/passwd"); err == nil {
		t.Error("expected an error for an escaping key")
	}

	got, err := safeJoin(base, "a/b.txt")
	if err != nil {
		t.Fatal(err)
	}
	if want := filepath.Join(base, "a", "b.txt"); got != want {
		t.Errorf("safeJoin = %q, want %q", got, want)
	}
}

func keysOf(m map[string][]byte) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}

	return out
}

func TestExpandPath(t *testing.T) {
	home, err := os.UserHomeDir()
	if err != nil {
		t.Skip("no home directory")
	}

	tests := map[string]string{
		".//tmp/x":     "/tmp/x", // an absolute path typed behind the "./" prefill
		"./tmp/x":      "tmp/x",
		"/tmp/x":       "/tmp/x",
		"~/docs":       filepath.Join(home, "docs"),
		"./~/docs":     filepath.Join(home, "docs"),
		"./a/../b.txt": "b.txt",
	}

	for in, want := range tests {
		if got := expandPath(in); got != want {
			t.Errorf("expandPath(%q) = %q, want %q", in, got, want)
		}
	}
}

// newDisconnectedModel starts s3tui without a client, as it does when no
// profile was chosen on the command line.
func newDisconnectedModel(t *testing.T, endpoint string) tea.Model {
	t.Helper()

	profile := config.Profile{
		Endpoint:  endpoint,
		Region:    "eu-central-1",
		AccessKey: "key",
		SecretKey: "secret",
		PathStyle: true,
	}

	cfg := config.Config{Profiles: map[string]config.Profile{"default": profile, "other": profile}}

	model := tea.Model(New(context.Background(), nil, "", config.Profile{}, cfg, "test"))
	model = step(t, model, tea.WindowSizeMsg{Width: 100, Height: 24})

	return runCmd(t, model, model.Init(), 0)
}

func TestStartsInTheProfileView(t *testing.T) {
	server := fakeS3(t)
	model := newDisconnectedModel(t, server.URL)

	got := model.(Model)
	if got.view != viewProfiles {
		t.Fatalf("view = %v, want profiles", got.view)
	}
	if len(got.profiles.Items()) != 2 {
		t.Errorf("got %d profiles, want 2", len(got.profiles.Items()))
	}
	if len(got.buckets.Items()) != 0 {
		t.Error("the buckets were listed although no profile was chosen yet")
	}

	view := model.View()
	for _, want := range []string{"select a profile", "enter connects"} {
		if !strings.Contains(view, want) {
			t.Errorf("view misses %q:\n%s", want, view)
		}
	}

	// esc has nowhere to go before a profile is picked
	model = step(t, model, tea.KeyMsg{Type: tea.KeyEsc})
	if got := model.(Model).view; got != viewProfiles {
		t.Errorf("view = %v, want to stay in profiles", got)
	}

	// enter connects and lists the buckets
	model = enter(t, model)

	got = model.(Model)
	if got.view != viewBuckets {
		t.Fatalf("view = %v, want buckets", got.view)
	}
	if got.profile != "default" {
		t.Errorf("profile = %q, want default", got.profile)
	}
	if len(got.buckets.Items()) == 0 {
		t.Error("the buckets were not listed after connecting")
	}
}

func TestPickerNavigatesWithEnterAndUploadsWithU(t *testing.T) {
	server := fakeS3(t)
	model := openBucket(t, server)

	parent := t.TempDir()
	if err := os.MkdirAll(filepath.Join(parent, "nested"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(parent, "nested", "deep.txt"), []byte("deep"), 0o600); err != nil {
		t.Fatal(err)
	}

	model = browseTo(t, model, parent)

	// enter only walks into the directory, it must not start a transfer
	model = step(t, model, tea.KeyMsg{Type: tea.KeyDown}) // skip ".."
	model = enter(t, model)

	got := model.(Model)
	if got.view != viewPicker {
		t.Fatalf("view = %v, enter must stay in the browser", got.view)
	}
	if len(got.transfers) != 0 || len(server.uploaded()) != 0 {
		t.Fatal("enter started an upload")
	}
	if got.uploadDir != filepath.Join(parent, "nested") {
		t.Errorf("current directory = %q, want the nested one", got.uploadDir)
	}

	// "u" uploads the highlighted file
	model = step(t, model, tea.KeyMsg{Type: tea.KeyDown}) // skip ".."
	model = press(t, model, 'u')

	if _, ok := server.uploaded()["deep.txt"]; !ok {
		t.Errorf("deep.txt was not uploaded, got %v", keysOf(server.uploaded()))
	}
	if got := model.(Model).view; got != viewObjects {
		t.Errorf("view = %v, want objects", got)
	}
	if got := model.(Model).uploadDir; got != filepath.Join(parent, "nested") {
		t.Errorf("uploadDir = %q, the browser should reopen there", got)
	}
}

func TestPickerCancels(t *testing.T) {
	server := fakeS3(t)
	model := openBucket(t, server)

	model = press(t, model, 'u')
	if got := model.(Model).view; got != viewPicker {
		t.Fatalf("view = %v, want the file browser", got)
	}

	model = step(t, model, tea.KeyMsg{Type: tea.KeyEsc})

	if got := model.(Model).view; got != viewObjects {
		t.Errorf("view = %v, want objects", got)
	}
	if len(server.uploaded()) != 0 {
		t.Error("something was uploaded although the browser was cancelled")
	}
}

func TestPickerGoesUpWithDotDot(t *testing.T) {
	server := fakeS3(t)
	model := openBucket(t, server)

	parent := t.TempDir()
	child := filepath.Join(parent, "child")
	if err := os.MkdirAll(child, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(parent, "top.txt"), []byte("top"), 0o600); err != nil {
		t.Fatal(err)
	}

	model = browseTo(t, model, child)

	// the first entry is ".."
	first, ok := model.(Model).picker.SelectedItem().(localItem)
	if !ok {
		t.Fatal("the browser has no entries")
	}
	if first.name != upEntry {
		t.Fatalf("first entry = %q, want %q", first.name, upEntry)
	}
	if !strings.Contains(model.View(), upEntry) {
		t.Errorf("the browser does not show %q:\n%s", upEntry, model.View())
	}

	// entering it walks up
	model = enter(t, model)

	got := model.(Model)
	if got.uploadDir != parent {
		t.Errorf("directory = %q, want %q", got.uploadDir, parent)
	}
	if got.view != viewPicker {
		t.Errorf("view = %v, want to stay in the browser", got.view)
	}

	// "u" on ".." must not upload the parent directory
	inner := got
	inner.picker.Select(0)
	model = press(t, tea.Model(inner), 'u')

	if len(server.uploaded()) != 0 {
		t.Errorf("%q was uploaded: %v", upEntry, keysOf(server.uploaded()))
	}
	if got := model.(Model).view; got != viewPicker {
		t.Errorf("view = %v, want to stay in the browser", got)
	}
}

func TestPickerListsDirectoriesFirst(t *testing.T) {
	server := fakeS3(t)
	model := openBucket(t, server)

	dir := t.TempDir()
	for _, name := range []string{"b.txt", "a.txt"} {
		if err := os.WriteFile(filepath.Join(dir, name), []byte("x"), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	for _, name := range []string{"zeta", "alpha"} {
		if err := os.MkdirAll(filepath.Join(dir, name), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.WriteFile(filepath.Join(dir, ".hidden"), []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}

	model = browseTo(t, model, dir)

	var names []string
	for _, item := range model.(Model).picker.Items() {
		names = append(names, item.(localItem).name)
	}

	want := []string{upEntry, "alpha", "zeta", "a.txt", "b.txt"}
	if strings.Join(names, ",") != strings.Join(want, ",") {
		t.Errorf("entries = %v, want %v", names, want)
	}
}

// s3ClientFor builds a plain SDK client against the fake, for the parts of a
// transfer that are easier to watch without the model around them.
func s3ClientFor(t *testing.T, server *fakeServer, bucket string) *s3.Client {
	t.Helper()

	client, err := awsclient.New(t.Context(), config.Profile{
		Endpoint:  server.URL,
		Region:    "eu-central-1",
		AccessKey: "key",
		SecretKey: "secret",
		PathStyle: true,
	}, "test")
	if err != nil {
		t.Fatalf("creating client: %v", err)
	}

	s3Client, err := client.ForBucket(t.Context(), bucket)
	if err != nil {
		t.Fatalf("resolving bucket region: %v", err)
	}

	return s3Client
}

// The progress bar of a single object used to sit at zero until the last byte
// arrived: nobody had asked how big the object is.
func TestDownloadKnowsHowBigTheObjectIs(t *testing.T) {
	server := fakeS3(t)

	transfer, err := startDownload(t.Context(), s3ClientFor(t, server, "bucket-a"),
		"bucket-a", "file.txt", "", filepath.Join(t.TempDir(), "out.txt"), false)
	if err != nil {
		t.Fatalf("starting the download: %v", err)
	}

	var (
		sized  bool
		failed error
	)

	for msg := range transfer.events {
		switch event := msg.(type) {
		case transferProgressMsg:
			if event.curTotal == 200 {
				sized = true
			}
		case transferDoneMsg:
			failed = event.err
		}
	}

	if failed != nil {
		t.Fatalf("the download failed: %v", failed)
	}
	if !sized {
		t.Error("the progress never learned the size of the object, so the bar cannot move")
	}
}

// A download that fails must not take the file it was about to overwrite with
// it, and must not leave its own leftovers either.
func TestFailedDownloadKeepsTheExistingFile(t *testing.T) {
	server := fakeS3(t)

	dir := t.TempDir()
	target := filepath.Join(dir, "out.txt")

	if err := os.WriteFile(target, []byte("keep me"), 0o600); err != nil {
		t.Fatal(err)
	}

	transfer, err := startDownload(t.Context(), s3ClientFor(t, server, "bucket-a"),
		"bucket-a", "gone-forever.txt", "", target, false)
	if err != nil {
		t.Fatalf("starting the download: %v", err)
	}

	var failed error

	for msg := range transfer.events {
		switch event := msg.(type) {
		case transferAskMsg:
			event.reply <- replyYes // yes, overwrite it
		case transferDoneMsg:
			failed = event.err
		}
	}

	if failed == nil {
		t.Fatal("downloading a missing key did not fail")
	}

	content, err := os.ReadFile(target)
	if err != nil {
		t.Fatalf("the existing file is gone: %v", err)
	}
	if string(content) != "keep me" {
		t.Errorf("the existing file was overwritten with %q", content)
	}

	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 1 {
		t.Errorf("the failed download left something behind: %v", entries)
	}
}
