package ui

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	tea "github.com/charmbracelet/bubbletea"
)

// closedEvents is the event channel of a transfer nobody feeds: waiting on it
// ends right away instead of blocking the test.
func closedEvents() chan tea.Msg {
	events := make(chan tea.Msg)
	close(events)

	return events
}

// fakeTransfer adds a transfer which is still running, without any goroutine
// behind it: the tests drive every real transfer to its end in one step.
func fakeTransfer(m Model, kind transferKind, label string, canceled *bool) Model {
	m.transferID++

	m.transfers = append(m.transfers, &transfer{
		id:     m.transferID,
		kind:   kind,
		label:  label,
		events: closedEvents(),
		cancel: func() { *canceled = true },
		done:   1,
		total:  4,
	})

	m.transferList.SetItems(transferItems(m.transfers))

	return m
}

func TestTransfersRunSideBySide(t *testing.T) {
	server := fakeS3(t)
	model := openBucket(t, server)

	dir := t.TempDir()

	inner := model.(Model)
	inner.objects.Select(1) // file.txt
	model = pressNoRun(tea.Model(inner), 'l')
	model = submitPrompt(t, model, filepath.Join(dir, "file.txt"))

	// a second action right afterwards must not be refused
	inner = model.(Model)
	inner.objects.Select(1)
	model = press(t, tea.Model(inner), 'd')
	model = submitPrompt(t, model, "file.txt")
	model = press(t, model, 'y')

	got := model.(Model)
	if strings.Contains(got.status, "already running") {
		t.Fatalf("the second transfer was refused: %q", got.status)
	}
	// both worked, so both are gone again
	if len(got.transfers) != 0 {
		t.Fatalf("%d transfers left, want a clean list", len(got.transfers))
	}

	if _, err := os.Stat(filepath.Join(dir, "file.txt")); err != nil {
		t.Errorf("the download did not happen: %v", err)
	}
	if _, ok := server.object("file.txt"); ok {
		t.Error("the delete did not happen")
	}
}

func TestTransferViewListsThem(t *testing.T) {
	server := fakeS3(t)
	canceled := false

	inner := fakeTransfer(openBucket(t, server).(Model), transferDownload,
		"s3://bucket-a/file.txt → /tmp/file.txt", &canceled)
	inner.transfers[0].finished = true
	inner.transfers[0].err = errors.New("access denied")
	inner = fakeTransfer(inner, transferUpload, "/tmp/fotos → s3://bucket-a/fotos/", &canceled)

	model := press(t, tea.Model(inner), 't')

	got := model.(Model)
	if got.view != viewTransfers {
		t.Fatalf("view = %v, want the transfers", got.view)
	}

	view := model.View()
	for _, want := range []string{
		"transfers", "[1 running]", "download", "file.txt", "failed · access denied",
		"upload", "25%", "x cancel",
	} {
		if !strings.Contains(view, want) {
			t.Errorf("the transfer view misses %q:\n%s", want, view)
		}
	}

	// esc returns to the view t was pressed in
	model = step(t, model, tea.KeyMsg{Type: tea.KeyEsc})

	if back := model.(Model).view; back != viewObjects {
		t.Errorf("view = %v, want the objects again", back)
	}
}

func TestEscapeNoLongerCancelsATransfer(t *testing.T) {
	server := fakeS3(t)

	canceled := false
	inner := fakeTransfer(openBucket(t, server).(Model), transferDownload, "download s3://bucket-a/big.iso", &canceled)

	// the objects view is one level below the bucket overview
	model := step(t, tea.Model(inner), tea.KeyMsg{Type: tea.KeyEsc})

	if canceled {
		t.Error("esc canceled the running transfer")
	}
	if got := model.(Model).view; got != viewBucketMenu {
		t.Errorf("view = %v, esc must navigate while a transfer runs", got)
	}
	if len(model.(Model).running()) != 1 {
		t.Error("the transfer is gone")
	}
}

func TestCancelAndClearInTheTransferView(t *testing.T) {
	server := fakeS3(t)

	canceled := false
	inner := fakeTransfer(openBucket(t, server).(Model), transferUpload, "upload /tmp/big.iso", &canceled)

	model := press(t, tea.Model(inner), 't')
	model = press(t, model, 'x')

	if model.(Model).pending == nil {
		t.Fatal("x canceled the transfer without asking")
	}
	if canceled {
		t.Fatal("the transfer was canceled before the answer")
	}

	model = press(t, model, 'y')

	got := model.(Model)
	if !canceled {
		t.Error("x did not cancel the running transfer")
	}
	if !strings.Contains(got.status, "canceling") {
		t.Errorf("status = %q", got.status)
	}
	if len(got.transfers) != 1 {
		t.Error("the transfer left the list before it ended")
	}

	// once it has ended, the same key clears the row
	got.transfers[0].finished = true
	got.transfers[0].summary = "uploaded 4/4 objects"
	got.transferList.SetItems(transferItems(got.transfers))

	model = press(t, tea.Model(got), 'x')

	if left := model.(Model).transfers; len(left) != 0 {
		t.Errorf("%d transfers left, want the finished one cleared", len(left))
	}
}

func TestOverwriteQuestionsAreAnsweredOneAfterTheOther(t *testing.T) {
	model := newTestModel(t, fakeS3(t).URL).(Model)

	first := make(chan overwriteReply, 1)
	second := make(chan overwriteReply, 1)

	model.transfers = []*transfer{
		{id: 1, kind: transferDownload, label: "download a", events: closedEvents()},
		{id: 2, kind: transferDownload, label: "download b", events: closedEvents()},
	}
	model.confirms = []transferAsk{
		{id: 1, path: "/tmp/a", reply: first},
		{id: 2, path: "/tmp/b", reply: second},
	}

	view := model.View()
	for _, want := range []string{"overwrite /tmp/a?", "+1 waiting"} {
		if !strings.Contains(view, want) {
			t.Errorf("the question misses %q:\n%s", want, view)
		}
	}

	answered := step(t, tea.Model(model), tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{'y'}})

	if got := <-first; got != replyYes {
		t.Errorf("first answer = %v, want yes", got)
	}
	if left := answered.(Model).confirms; len(left) != 1 || left[0].id != 2 {
		t.Errorf("confirms = %+v, want the second question left", left)
	}
	if !strings.Contains(answered.View(), "overwrite /tmp/b?") {
		t.Errorf("the second question is not asked:\n%s", answered.View())
	}
}

// A transfer that did what it was asked for leaves the list on its own, one
// that did not stays there to be looked at.
func TestOnlySuccessfulTransfersDisappear(t *testing.T) {
	server := fakeS3(t)

	t.Run("success", func(t *testing.T) {
		model := openBucket(t, server)

		inner := model.(Model)
		inner.objects.Select(1) // file.txt
		model = pressNoRun(tea.Model(inner), 'l')
		model = submitPrompt(t, model, filepath.Join(t.TempDir(), "file.txt"))

		got := model.(Model)
		if len(got.transfers) != 0 {
			t.Errorf("the finished download is still listed: %+v", got.transfers[0])
		}
		if !strings.Contains(got.status, "downloaded 1/1") {
			t.Errorf("status = %q, want the summary of the download", got.status)
		}
	})

	t.Run("failure", func(t *testing.T) {
		canceled := false
		inner := fakeTransfer(openBucket(t, server).(Model), transferUpload, "/tmp/x → s3://bucket-a/x", &canceled)
		entry := inner.transfers[0]

		model := step(t, tea.Model(inner), transferMsg{
			id:  entry.id,
			msg: transferDoneMsg{err: errors.New("access denied"), summary: "uploaded 2/9 objects"},
		})

		got := model.(Model)
		if len(got.transfers) != 1 {
			t.Fatalf("%d transfers left, the failed one has to stay", len(got.transfers))
		}
		if !got.transfers[0].finished || got.transfers[0].err == nil {
			t.Errorf("transfer = %+v, want it finished with its error", got.transfers[0])
		}
		if got.err == nil {
			t.Error("the failure was not reported at all")
		}
	})

	t.Run("canceled", func(t *testing.T) {
		canceled := false
		inner := fakeTransfer(openBucket(t, server).(Model), transferDownload, "s3://bucket-a/big.iso → /tmp/big.iso", &canceled)
		entry := inner.transfers[0]

		model := step(t, tea.Model(inner), transferMsg{
			id:  entry.id,
			msg: transferDoneMsg{err: context.Canceled, summary: "downloaded 2/7 files"},
		})

		got := model.(Model)
		if len(got.transfers) != 1 {
			t.Fatalf("%d transfers left, the canceled one has to stay", len(got.transfers))
		}
		if !got.transfers[0].canceled {
			t.Error("the transfer is not marked as canceled")
		}
	})
}

// ctrl+c is the only way out. It leaves right away, unless it would throw a
// running transfer away.
func TestQuitAsksAgainWhileTransfersRun(t *testing.T) {
	server := fakeS3(t)
	ctrlC := tea.KeyMsg{Type: tea.KeyCtrlC}

	t.Run("nothing running", func(t *testing.T) {
		_, cmd := openBucket(t, server).Update(ctrlC)
		if cmd == nil {
			t.Fatal("ctrl+c did nothing")
		}
		if _, ok := cmd().(tea.QuitMsg); !ok {
			t.Error("ctrl+c did not quit although nothing was running")
		}
	})

	t.Run("a transfer running", func(t *testing.T) {
		canceled := false
		inner := fakeTransfer(openBucket(t, server).(Model), transferDownload, "s3://bucket-a/big.iso → /tmp/big.iso", &canceled)

		// the first press only warns, and must not run the tick command here
		asked, _ := tea.Model(inner).Update(ctrlC)

		got := asked.(Model)
		if got.quitArmed.IsZero() {
			t.Fatal("ctrl+c quit while a transfer was running")
		}
		if view := asked.View(); !strings.Contains(view, "a transfer is still running") ||
			!strings.Contains(view, "ctrl+c again to quit") {
			t.Errorf("the warning is missing:\n%s", view)
		}

		// the second one within the window leaves
		again, cmd := asked.Update(ctrlC)
		if cmd == nil {
			t.Fatal("the second ctrl+c did nothing")
		}
		if _, ok := cmd().(tea.QuitMsg); !ok {
			t.Error("the second ctrl+c did not quit")
		}

		// once the window is over, the warning goes away again
		expired := step(t, again, quitExpiredMsg{armed: got.quitArmed})
		if !expired.(Model).quitArmed.IsZero() {
			t.Error("the window stayed open")
		}
	})

	t.Run("too late", func(t *testing.T) {
		canceled := false
		inner := fakeTransfer(openBucket(t, server).(Model), transferUpload, "/tmp/x → s3://bucket-a/x", &canceled)
		inner.quitArmed = time.Now().Add(-2 * quitWindow)

		late, _ := tea.Model(inner).Update(ctrlC)

		if late.(Model).quitArmed.Before(time.Now().Add(-quitWindow)) {
			t.Error("a press long after the first one has to start a new window")
		}
	})
}
