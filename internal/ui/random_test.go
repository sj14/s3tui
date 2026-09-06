package ui

import (
	"bytes"
	"io"
	"strings"
	"testing"

	tea "github.com/charmbracelet/bubbletea"
)

func TestParseRandomSpec(t *testing.T) {
	for _, test := range []struct {
		input string
		size  int64
		count int
	}{
		{"1 MiB 10", 1 << 20, 10},
		{"1MiB 10", 1 << 20, 10},
		{"512 B 3", 512, 3},
		{"4 KiB", 4 << 10, 1},
		{"1 GB 2", 1_000_000_000, 2},
		{"  2 MiB   5  ", 2 << 20, 5},
	} {
		spec, err := parseRandomSpec(test.input)
		if err != nil {
			t.Errorf("%q: %v", test.input, err)

			continue
		}

		if spec.size != test.size || spec.count != test.count {
			t.Errorf("%q = %+v, want %d bytes × %d", test.input, spec, test.size, test.count)
		}
	}

	for _, input := range []string{"", "   ", "many MiB 3", "1 MiB 0", "1 MiB -2", "1 MiB lots"} {
		if spec, err := parseRandomSpec(input); err == nil {
			t.Errorf("%q was accepted as %+v", input, spec)
		}
	}
}

func TestRandomReaderYieldsExactlyItsSize(t *testing.T) {
	read, err := io.ReadAll(newRandomReader(3000))
	if err != nil {
		t.Fatalf("reading: %v", err)
	}

	if len(read) != 3000 {
		t.Errorf("got %d bytes, want 3000", len(read))
	}
	if bytes.Equal(read, make([]byte, 3000)) {
		t.Error("the random data is all zeros")
	}

	empty, err := io.ReadAll(newRandomReader(0))
	if err != nil || len(empty) != 0 {
		t.Errorf("a size of zero gave %d bytes: %v", len(empty), err)
	}
}

func TestRandomUploadFromTheFileBrowser(t *testing.T) {
	server := fakeS3(t)

	model := browseTo(t, openBucket(t, server), t.TempDir())

	if got := model.(Model).view; got != viewPicker {
		t.Fatalf("view = %v, want the file browser", got)
	}
	if !strings.Contains(model.View(), "r random objects") {
		t.Errorf("the file browser does not offer the random upload:\n%s", model.View())
	}

	model = pressNoRun(model, 'r')

	form := model.(Model).randomForm
	if form == nil {
		t.Fatal("no random upload form was opened")
	}
	if form.prefix.Value() != "rand/" || form.size.Value() != "1 MiB" || form.count.Value() != "10" {
		t.Errorf("form defaults = prefix %q, size %q, count %q", form.prefix.Value(), form.size.Value(), form.count.Value())
	}

	form.prefix.SetValue("generated")
	form.size.SetValue("1 KiB")
	form.count.SetValue("3")
	model = enter(t, model)

	got := model.(Model)
	if got.err != nil {
		t.Fatalf("the upload failed: %v", got.err)
	}
	if got.view != viewObjects {
		t.Errorf("view = %v, want the objects the upload lands in", got.view)
	}
	if !strings.Contains(got.status, "uploaded 3/3") {
		t.Errorf("status = %q", got.status)
	}

	uploaded := server.uploaded()
	if len(uploaded) != 3 {
		t.Fatalf("got %d objects, want 3: %v", len(uploaded), keysOf(uploaded))
	}

	for key, body := range uploaded {
		if !strings.HasPrefix(key, "generated/rand-") || !strings.HasSuffix(key, ".bin") {
			t.Errorf("unexpected key %q", key)
		}
		if len(body) != 1024 {
			t.Errorf("%q is %d bytes, want 1024", key, len(body))
		}
	}
}

func TestRandomUploadBlockedInReadOnlyProfile(t *testing.T) {
	server := fakeS3(t)

	inner := browseTo(t, openBucket(t, server), t.TempDir()).(Model)
	inner.profileCfg.ReadOnly = true

	model := pressNoRun(tea.Model(inner), 'r')

	if model.(Model).prompt != nil {
		t.Error("a prompt was opened in a read-only profile")
	}
	if status := model.(Model).status; !strings.Contains(status, "read-only") {
		t.Errorf("status = %q", status)
	}
}
