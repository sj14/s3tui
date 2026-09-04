package ui

import (
	"strings"
	"testing"

	tea "github.com/charmbracelet/bubbletea"
)

func TestMultipartUploadsAndParts(t *testing.T) {
	server := fakeS3(t)
	model := openSection(t, openBucketMenu(t, server), menuMultipart)

	got := model.(Model)
	if got.view != viewMultipart {
		t.Fatalf("view = %v, want multipart", got.view)
	}
	if len(got.multipart.Items()) != 2 {
		t.Fatalf("got %d uploads, want 2", len(got.multipart.Items()))
	}

	view := model.View()
	for _, want := range []string{"multipart uploads", "big.iso", "upload-1", "logs/huge.log", "[GLACIER]"} {
		if !strings.Contains(view, want) {
			t.Errorf("multipart view misses %q:\n%s", want, view)
		}
	}

	// enter shows the parts of the selected upload
	model = enter(t, model)

	got = model.(Model)
	if got.view != viewParts {
		t.Fatalf("view = %v, want parts", got.view)
	}
	if got.uploadID != "upload-1" {
		t.Errorf("uploadID = %q, want upload-1", got.uploadID)
	}

	view = model.View()
	for _, want := range []string{"big.iso", "part 1", "part 2", "aaa", "5.0 MiB", "1.0 MiB"} {
		if !strings.Contains(view, want) {
			t.Errorf("parts view misses %q:\n%s", want, view)
		}
	}

	// esc walks back up
	model = step(t, model, tea.KeyMsg{Type: tea.KeyEsc})
	if got := model.(Model).view; got != viewMultipart {
		t.Errorf("view = %v, want multipart", got)
	}

	model = step(t, model, tea.KeyMsg{Type: tea.KeyEsc})
	if got := model.(Model).view; got != viewBucketMenu {
		t.Errorf("view = %v, want the bucket overview", got)
	}
}

func TestCopyObject(t *testing.T) {
	server := fakeS3(t)
	model := openBucket(t, server)

	inner := model.(Model)
	inner.objects.Select(1) // file.txt
	model = pressNoRun(tea.Model(inner), 'c')

	prompt := model.(Model).prompt
	if prompt == nil {
		t.Fatal("no prompt is open")
	}
	if got := prompt.input.Value(); got != "s3://bucket-a/file.txt" {
		t.Errorf("prefilled target = %q", got)
	}

	model = submitPrompt(t, model, "s3://bucket-a/backup/file.txt")

	body, ok := server.object("backup/file.txt")
	if !ok {
		t.Fatalf("the copy is missing, server has %v", keysOf(server.uploaded()))
	}
	if len(body) != 200 {
		t.Errorf("copy has %d bytes, want 200", len(body))
	}
	if _, ok := server.object("file.txt"); !ok {
		t.Error("a copy must not remove the source")
	}
	if status := model.(Model).status; !strings.Contains(status, "copied 1/1") {
		t.Errorf("status = %q", status)
	}
}

func TestMoveObjectDeletesSource(t *testing.T) {
	server := fakeS3(t)
	model := openBucket(t, server)

	inner := model.(Model)
	inner.objects.Select(1) // file.txt
	model = pressNoRun(tea.Model(inner), 'm')

	model = submitPrompt(t, model, "s3://bucket-a/renamed.txt")

	if _, ok := server.object("renamed.txt"); !ok {
		t.Error("the target is missing")
	}
	if _, ok := server.object("file.txt"); ok {
		t.Error("the source was not deleted")
	}
	if status := model.(Model).status; !strings.Contains(status, "moved 1/1") {
		t.Errorf("status = %q", status)
	}
}

func TestCopyPrefixRecursively(t *testing.T) {
	server := fakeS3(t)
	model := openBucket(t, server)

	inner := model.(Model)
	inner.objects.Select(0) // the logs/ prefix
	model = pressNoRun(tea.Model(inner), 'c')

	if got := model.(Model).prompt.input.Value(); got != "s3://bucket-a/logs/" {
		t.Errorf("prefilled target = %q", got)
	}

	model = submitPrompt(t, model, "s3://bucket-a/archive/")

	if _, ok := server.object("archive/nested.log"); !ok {
		t.Errorf("the recursive copy is missing, server has %v", keysOf(server.uploaded()))
	}
	if _, ok := server.object("logs/nested.log"); !ok {
		t.Error("a copy must not remove the source")
	}
}

func TestCopyRejectsInvalidTargets(t *testing.T) {
	server := fakeS3(t)

	tests := map[string]string{
		"identical target":        "s3://bucket-a/file.txt",
		"target without a name":   "s3://bucket-a/",
		"target without a bucket": "s3://",
	}

	for name, target := range tests {
		t.Run(name, func(t *testing.T) {
			model := openBucket(t, server)

			inner := model.(Model)
			inner.objects.Select(1)
			model = pressNoRun(tea.Model(inner), 'c')
			model = submitPrompt(t, model, target)

			got := model.(Model)
			if got.err == nil {
				t.Fatalf("expected an error for target %q", target)
			}
			if len(got.transfers) != 0 {
				t.Error("a transfer was started anyway")
			}
		})
	}
}

func TestCopyIntoOwnPrefixIsRejected(t *testing.T) {
	server := fakeS3(t)
	model := openBucket(t, server)

	inner := model.(Model)
	inner.objects.Select(0) // logs/
	model = pressNoRun(tea.Model(inner), 'c')
	model = submitPrompt(t, model, "s3://bucket-a/logs/nested/")

	if model.(Model).err == nil {
		t.Error("copying a prefix into itself must be rejected")
	}
}

func TestCopyBlockedInReadOnlyProfile(t *testing.T) {
	server := fakeS3(t)

	model := newTestModel(t, server.URL)
	inner := model.(Model)
	inner.profileCfg.ReadOnly = true
	model = enter(t, tea.Model(inner))

	model = press(t, model, 'c')

	if model.(Model).prompt != nil {
		t.Error("a prompt was opened in a read-only profile")
	}
	if status := model.(Model).status; !strings.Contains(status, "read-only") {
		t.Errorf("status = %q", status)
	}
}

func TestCopyVersion(t *testing.T) {
	server := fakeS3(t)
	model := openBucket(t, server)

	inner := model.(Model)
	inner.objects.Select(1)
	model = enter(t, tea.Model(inner)) // versions of file.txt

	// a single version cannot be moved
	model = press(t, model, 'm')
	if model.(Model).prompt != nil {
		t.Error("move must not be offered for a single version")
	}

	inner = model.(Model)
	inner.versions.Select(1) // v1
	model = pressNoRun(tea.Model(inner), 'c')

	prompt := model.(Model).prompt
	if prompt == nil {
		t.Fatal("no prompt is open")
	}
	if prompt.versionID != "v1" {
		t.Errorf("versionID = %q, want v1", prompt.versionID)
	}

	model = submitPrompt(t, model, "s3://bucket-a/restored.txt")

	if _, ok := server.object("restored.txt"); !ok {
		t.Error("the version copy is missing")
	}
}

func TestParseTarget(t *testing.T) {
	tests := []struct {
		in         string
		wantBucket string
		wantKey    string
		wantErr    bool
	}{
		{in: "s3://other/dir/file.txt", wantBucket: "other", wantKey: "dir/file.txt"},
		{in: "dir/file.txt", wantBucket: "current", wantKey: "dir/file.txt"},
		{in: "/dir/file.txt", wantBucket: "current", wantKey: "dir/file.txt"},
		{in: "s3://only-bucket", wantErr: true},
		{in: "   ", wantErr: true},
	}

	for _, test := range tests {
		got, err := parseTarget(test.in, "current")
		if test.wantErr {
			if err == nil {
				t.Errorf("parseTarget(%q) did not fail", test.in)
			}

			continue
		}

		if err != nil {
			t.Errorf("parseTarget(%q): %v", test.in, err)

			continue
		}
		if got.bucket != test.wantBucket || got.key != test.wantKey {
			t.Errorf("parseTarget(%q) = %s, want s3://%s/%s", test.in, got, test.wantBucket, test.wantKey)
		}
	}
}

func TestCopySourceEncoding(t *testing.T) {
	if got, want := copySource("my-bucket", "a b/c+d.txt", ""), "/my-bucket/a%20b/c+d.txt"; got != want {
		t.Errorf("copySource = %q, want %q", got, want)
	}

	if got, want := copySource("b", "k", "v1"), "/b/k?versionId=v1"; got != want {
		t.Errorf("copySource = %q, want %q", got, want)
	}
}
