package ui

import (
	"slices"
	"strings"
	"testing"

	tea "github.com/charmbracelet/bubbletea"
)

func TestDeleteObjectAsksFirst(t *testing.T) {
	server := fakeS3(t)
	model := openBucket(t, server)

	inner := model.(Model)
	inner.objects.Select(1) // file.txt
	model = press(t, tea.Model(inner), 'd')

	pending := model.(Model).pending
	if pending == nil {
		t.Fatal("no confirmation was asked")
	}
	if !strings.Contains(pending.question, "s3://bucket-a/file.txt") {
		t.Errorf("question = %q", pending.question)
	}
	if !strings.Contains(model.View(), "[y]es") {
		t.Error("the footer does not offer the answers")
	}

	// nothing happens before the answer
	if _, ok := server.object("file.txt"); !ok {
		t.Fatal("the object was deleted before the confirmation")
	}

	model = press(t, model, 'y')

	if _, ok := server.object("file.txt"); ok {
		t.Error("the object was not deleted")
	}
	if status := model.(Model).status; !strings.Contains(status, "deleted 1/1") {
		t.Errorf("status = %q", status)
	}
}

func TestDeleteCanceled(t *testing.T) {
	server := fakeS3(t)
	model := openBucket(t, server)

	inner := model.(Model)
	inner.objects.Select(1)
	model = press(t, tea.Model(inner), 'd')
	model = press(t, model, 'n')

	if model.(Model).pending != nil {
		t.Error("the question stayed open")
	}
	if _, ok := server.object("file.txt"); !ok {
		t.Error("the object was deleted although the answer was no")
	}
	if status := model.(Model).status; !strings.Contains(status, "canceled") {
		t.Errorf("status = %q", status)
	}
}

func TestDeletePrefixRecursively(t *testing.T) {
	server := fakeS3(t)
	model := openBucket(t, server)

	inner := model.(Model)
	inner.objects.Select(0) // the logs/ prefix
	model = press(t, tea.Model(inner), 'd')

	pending := model.(Model).pending
	if pending == nil {
		t.Fatal("no confirmation was asked")
	}
	if !strings.Contains(pending.question, "recursive") {
		t.Errorf("question = %q, want a recursive warning", pending.question)
	}

	model = press(t, model, 'y')

	for _, key := range []string{"logs/nested.log", "logs/old.log"} {
		if _, ok := server.object(key); ok {
			t.Errorf("%q was not deleted", key)
		}
	}
	if status := model.(Model).status; !strings.Contains(status, "deleted 2/2") {
		t.Errorf("status = %q", status)
	}
}

func TestDeleteVersion(t *testing.T) {
	server := fakeS3(t)
	model := openBucket(t, server)

	inner := model.(Model)
	inner.objects.Select(1)
	model = enter(t, tea.Model(inner)) // versions of file.txt

	inner = model.(Model)
	inner.versions.Select(1) // v1
	model = press(t, tea.Model(inner), 'd')

	pending := model.(Model).pending
	if pending == nil {
		t.Fatal("no confirmation was asked")
	}
	if !strings.Contains(pending.question, "version v1") {
		t.Errorf("question = %q", pending.question)
	}

	model = press(t, model, 'y')

	version, ok := server.deletedVersion("file.txt")
	if !ok {
		t.Fatal("no delete reached the server")
	}
	if version != "v1" {
		t.Errorf("deleted versionId = %q, want v1", version)
	}
	if _, ok := server.object("file.txt"); !ok {
		t.Error("a versioned delete must not remove the key itself")
	}
}

func TestAbortMultipartUpload(t *testing.T) {
	server := fakeS3(t)
	model := openSection(t, openBucketMenu(t, server), menuMultipart)

	model = press(t, model, 'd')

	pending := model.(Model).pending
	if pending == nil {
		t.Fatal("no confirmation was asked")
	}
	if !strings.Contains(pending.question, "abort multipart upload") {
		t.Errorf("question = %q", pending.question)
	}

	model = press(t, model, 'y')

	uploadID, ok := server.aborted("big.iso")
	if !ok {
		t.Fatal("no abort reached the server")
	}
	if uploadID != "upload-1" {
		t.Errorf("aborted uploadId = %q, want upload-1", uploadID)
	}
	if status := model.(Model).status; !strings.Contains(status, "aborted multipart upload") {
		t.Errorf("status = %q", status)
	}
	if got := model.(Model).view; got != viewMultipart {
		t.Errorf("view = %v, want multipart", got)
	}
}

func TestDeleteBlockedInReadOnlyProfile(t *testing.T) {
	server := fakeS3(t)

	model := newTestModel(t, server.URL)
	inner := model.(Model)
	inner.profileCfg.ReadOnly = true
	model = enter(t, tea.Model(inner))

	model = press(t, model, 'd')

	if model.(Model).pending != nil {
		t.Error("a confirmation was asked in a read-only profile")
	}
	if status := model.(Model).status; !strings.Contains(status, "read-only") {
		t.Errorf("status = %q", status)
	}
}

func TestDeleteNotOfferedInBucketView(t *testing.T) {
	server := fakeS3(t)
	model := newTestModel(t, server.URL)

	model = press(t, model, 'd')

	if model.(Model).pending != nil {
		t.Error("buckets must not be deletable")
	}
	if status := model.(Model).status; !strings.Contains(status, "select an object") {
		t.Errorf("status = %q", status)
	}
}

func TestFooterListsTheActionKeys(t *testing.T) {
	server := fakeS3(t)

	model := openBucket(t, server)
	model = step(t, model, tea.WindowSizeMsg{Width: 140, Height: 24})

	view := model.View()
	for _, want := range []string{"enter open", "u upload", "l download", "c copy", "m move", "d delete", "/ search"} {
		if !strings.Contains(view, want) {
			t.Errorf("footer misses %q:\n%s", want, view)
		}
	}

	// a narrow terminal drops whole entries instead of cutting a word in half
	model = step(t, model, tea.WindowSizeMsg{Width: 70, Height: 24})

	footer := lastLine(model.View())
	if strings.Contains(footer, "…") {
		t.Errorf("the help was cut instead of trimmed: %q", footer)
	}
	for _, want := range []string{"u upload", "? help"} {
		if !strings.Contains(footer, want) {
			t.Errorf("footer misses %q: %q", want, footer)
		}
	}
}

func TestDeleteEveryVersionOfAnObject(t *testing.T) {
	server := fakeS3(t)
	model := openBucket(t, server)

	inner := model.(Model)
	inner.objects.Select(1) // file.txt
	model = press(t, tea.Model(inner), 'D')

	pending := model.(Model).pending
	if pending == nil {
		t.Fatal("no confirmation was asked")
	}
	for _, want := range []string{"s3://bucket-a/file.txt", "every version", "irreversible"} {
		if !strings.Contains(pending.question, want) {
			t.Errorf("question = %q, want %q in it", pending.question, want)
		}
	}

	if len(server.deleteLog()) > 0 {
		t.Fatal("something was deleted before the confirmation")
	}

	model = press(t, model, 'y')

	// the whole history goes, delete markers included
	want := []string{"file.txt@v2", "file.txt@v1", "file.txt@dm1"}
	if got := server.deleteLog(); !slices.Equal(got, want) {
		t.Errorf("deleted %v, want %v", got, want)
	}
	if status := model.(Model).status; !strings.Contains(status, "deleted 3/3 versions") {
		t.Errorf("status = %q", status)
	}
}

func TestDeleteEveryVersionFromTheVersionList(t *testing.T) {
	server := fakeS3(t)
	model := openBucket(t, server)

	inner := model.(Model)
	inner.objects.Select(1)
	model = enter(t, tea.Model(inner)) // versions of file.txt

	inner = model.(Model)
	inner.versions.Select(1) // the selection does not matter, all of them go
	model = press(t, tea.Model(inner), 'D')

	pending := model.(Model).pending
	if pending == nil {
		t.Fatal("no confirmation was asked")
	}
	if !strings.Contains(pending.question, "all 3 versions") {
		t.Errorf("question = %q, want the number of versions", pending.question)
	}

	model = press(t, model, 'y')

	if got := server.deleteLog(); len(got) != 3 {
		t.Errorf("deleted %v, want all three versions", got)
	}
	if status := model.(Model).status; !strings.Contains(status, "deleted 3/3 versions") {
		t.Errorf("status = %q", status)
	}
}

func TestDeleteEveryVersionBelowAPrefix(t *testing.T) {
	server := fakeS3(t)
	model := openBucket(t, server)

	inner := model.(Model)
	inner.objects.Select(0) // the logs/ prefix
	model = press(t, tea.Model(inner), 'D')

	pending := model.(Model).pending
	if pending == nil {
		t.Fatal("no confirmation was asked")
	}
	for _, want := range []string{"every version below", "recursive"} {
		if !strings.Contains(pending.question, want) {
			t.Errorf("question = %q, want %q in it", pending.question, want)
		}
	}

	model = press(t, model, 'y')

	// the delete marker of a key which only exists as one goes too
	want := []string{"logs/nested.log@only", "logs/old.log@only", "logs/vanished.log@dm-logs/vanished.log"}
	if got := server.deleteLog(); !slices.Equal(got, want) {
		t.Errorf("deleted %v, want %v", got, want)
	}
}

// A key whose newest version is a delete marker is refused by every other
// action, this is the one that has to accept it.
func TestDeleteEveryVersionOfADeletedKey(t *testing.T) {
	server := fakeS3(t)
	model := openSection(t, openBucketMenu(t, server), menuDeleted)

	inner := model.(Model)

	index := -1
	for i, item := range inner.objects.Items() {
		if item.(objectItem).deleted {
			index = i

			break
		}
	}

	if index < 0 {
		t.Fatal("the listing has no deleted key")
	}

	inner.objects.Select(index)

	refused := press(t, tea.Model(inner), 'd')
	if refused.(Model).pending != nil {
		t.Error("d offered to delete a key that is already deleted")
	}

	model = press(t, tea.Model(inner), 'D')

	pending := model.(Model).pending
	if pending == nil {
		t.Fatal("no confirmation was asked for the deleted key")
	}

	model = press(t, model, 'y')

	if got := server.deleteLog(); len(got) == 0 {
		t.Error("the versions of the deleted key are still there")
	}
}

func TestDeleteEveryVersionBlockedInReadOnlyProfile(t *testing.T) {
	server := fakeS3(t)

	model := openBucket(t, server)
	inner := model.(Model)
	inner.profileCfg.ReadOnly = true
	inner.objects.Select(1)

	model = press(t, tea.Model(inner), 'D')

	if model.(Model).pending != nil {
		t.Error("a confirmation was asked in a read-only profile")
	}
	if status := model.(Model).status; !strings.Contains(status, "read-only") {
		t.Errorf("status = %q", status)
	}
	if len(server.deleteLog()) > 0 {
		t.Error("a read-only profile deleted something")
	}
}

func TestDeleteEveryVersionNotOfferedInBucketView(t *testing.T) {
	server := fakeS3(t)
	model := newTestModel(t, server.URL)

	model = press(t, model, 'D')

	if model.(Model).pending != nil {
		t.Error("buckets must not be deletable")
	}
	if status := model.(Model).status; !strings.Contains(status, "select an object") {
		t.Errorf("status = %q", status)
	}
}
