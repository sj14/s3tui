package ui

import (
	"strings"
	"testing"

	tea "github.com/charmbracelet/bubbletea"
)

// openStats picks the statistics entry of the bucket overview and lets the
// scan run to its end.
func openStats(t *testing.T, model tea.Model) tea.Model {
	t.Helper()

	model = openSection(t, model, menuStats)

	if got := model.(Model).view; got != viewStats {
		t.Fatalf("view = %v, want the statistics", got)
	}

	return model
}

func TestStatisticsCountEveryVersion(t *testing.T) {
	server := fakeS3(t)
	defer server.Close()

	model := openStats(t, openBucketMenu(t, server))
	got := model.(Model)

	if got.err != nil {
		t.Fatalf("scanning failed: %v", got.err)
	}

	scan := got.stats
	if scan.running {
		t.Error("the scan is still running although the listing ended")
	}
	if !scan.versions {
		t.Error("a versioned bucket has to be scanned with the version listing")
	}
	if scan.note != "" {
		t.Errorf("note = %q, want none", scan.note)
	}

	// the fake answers in two pages, both have to be counted
	if scan.stats.pages != 2 {
		t.Errorf("pages = %d, want 2", scan.stats.pages)
	}
	if scan.stats.entries != 6 {
		t.Errorf("entries = %d, want 6", scan.stats.entries)
	}

	if scan.stats.current.count != 3 || scan.stats.current.bytes != 2097402 {
		t.Errorf("current = %+v, want 3 objects of 2097402 bytes", scan.stats.current)
	}
	if scan.stats.noncurrent.count != 1 || scan.stats.noncurrent.bytes != 100 {
		t.Errorf("non-current = %+v, want one version of 100 bytes", scan.stats.noncurrent)
	}
	if scan.stats.markers != 2 {
		t.Errorf("delete markers = %d, want 2", scan.stats.markers)
	}
	if scan.stats.deleted != 1 {
		t.Errorf("deleted keys = %d, want 1", scan.stats.deleted)
	}

	if stored := scan.stats.stored(); stored.count != 4 || stored.bytes != 2097502 {
		t.Errorf("stored = %+v, want 4 versions of 2097502 bytes", stored)
	}

	if entry := scan.stats.classes["GLACIER"]; entry.count != 1 || entry.bytes != 100 {
		t.Errorf("GLACIER = %+v, want one version of 100 bytes", entry)
	}
	if entry := scan.stats.classes["STANDARD"]; entry.count != 3 || entry.bytes != 2097402 {
		t.Errorf("STANDARD = %+v, want 3 versions of 2097402 bytes", entry)
	}

	if scan.stats.largest.key != "big.iso" {
		t.Errorf("largest = %q, want big.iso", scan.stats.largest.key)
	}
	if scan.stats.oldest.key != "logs/nested.log" {
		t.Errorf("oldest = %q, want logs/nested.log", scan.stats.oldest.key)
	}
	if scan.stats.newest.key != "big.iso" && scan.stats.newest.key != "file.txt" {
		t.Errorf("newest = %q, want one of the two newest objects", scan.stats.newest.key)
	}

	// the histograms only count what is there right now
	if total := sum(scan.stats.sizes); total != 3 {
		t.Errorf("the size histogram holds %d objects, want 3", total)
	}
	if total := sum(scan.stats.ages); total != 3 {
		t.Errorf("the age histogram holds %d objects, want 3", total)
	}
	if scan.stats.sizes[sizeBucketOf(2097152)] != 1 {
		t.Error("big.iso is missing from its size bucket")
	}

	view := model.View()
	for _, want := range []string{"bucket-a", "[statistics]", "current objects", "2.0 MiB", "delete markers"} {
		if !strings.Contains(view, want) {
			t.Errorf("the statistics miss %q:\n%s", want, view)
		}
	}

	for _, want := range []string{"storage classes", "GLACIER", "object size", "object age", "outliers", "big.iso"} {
		if !strings.Contains(got.detailsContent, want) {
			t.Errorf("the statistics miss %q:\n%s", want, got.detailsContent)
		}
	}
}

func sum(counts []int64) int64 {
	var total int64
	for _, count := range counts {
		total += count
	}

	return total
}

func TestStatisticsFallBackWithoutVersions(t *testing.T) {
	server := fakeS3(t)
	defer server.Close()

	inner := newTestModel(t, server.URL).(Model)
	inner.buckets.Select(1) // bucket-b denies ListObjectVersions

	model := openStats(t, enter(t, tea.Model(inner)))
	got := model.(Model)

	if got.err != nil {
		t.Fatalf("the fallback must not be an error: %v", got.err)
	}
	if got.stats.versions {
		t.Error("the scan claims to have read the versions")
	}
	if !strings.Contains(got.stats.note, "s3:ListBucketVersions") {
		t.Errorf("note = %q, want the missing permission", got.stats.note)
	}
	if got.stats.stats.current.count == 0 {
		t.Error("the bucket looks empty instead of falling back to the plain listing")
	}

	// what only the version listing knows must not be claimed
	if !strings.Contains(got.detailsContent, "current objects") {
		t.Errorf("the statistics miss the objects:\n%s", got.detailsContent)
	}
	if strings.Contains(got.detailsContent, "non-current versions") {
		t.Errorf("the statistics claim to know the older versions:\n%s", got.detailsContent)
	}
}

func TestStatisticsStopOnBack(t *testing.T) {
	server := fakeS3(t)
	defer server.Close()

	model := openStats(t, openBucketMenu(t, server))

	model = step(t, model, tea.KeyMsg{Type: tea.KeyEsc})

	got := model.(Model)
	if got.view != viewBucketMenu {
		t.Errorf("view = %v, want the bucket overview", got.view)
	}
	if got.stats.running {
		t.Error("the scan kept running after esc")
	}
}
