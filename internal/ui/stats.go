package ui

import (
	"context"
	"fmt"
	"math"
	"sort"
	"strings"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/s3"
	tea "github.com/charmbracelet/bubbletea"
	"github.com/dustin/go-humanize"

	"github.com/sj14/s3tui/internal/awsclient"
)

// S3 has no API which reports what a bucket holds, so the statistics are the
// listing itself, added up page by page. Every page is one request and one
// message, which keeps the numbers growing while the scan runs and lets esc
// stop it at any point.

// group counts objects and the bytes they take.
type group struct {
	count int64
	bytes int64
}

func (g *group) add(size int64) {
	g.count++
	g.bytes += max(size, 0)
}

func (g group) plus(other group) group {
	return group{count: g.count + other.count, bytes: g.bytes + other.bytes}
}

// sizeBuckets and ageBuckets are the rows of the two histograms: an object
// belongs to the first bucket whose limit it does not exceed.
var sizeBuckets = []struct {
	label string
	limit int64
}{
	{"empty", 0},
	{"≤ 1 KiB", 1 << 10},
	{"≤ 1 MiB", 1 << 20},
	{"≤ 10 MiB", 10 << 20},
	{"≤ 100 MiB", 100 << 20},
	{"≤ 1 GiB", 1 << 30},
	{"≤ 10 GiB", 10 << 30},
	{"larger", math.MaxInt64},
}

var ageBuckets = []struct {
	label string
	days  float64
}{
	{"today", 1},
	{"≤ 1 week", 7},
	{"≤ 1 month", 30},
	{"≤ 3 months", 90},
	{"≤ 1 year", 365},
	{"older", math.MaxFloat64},
}

func sizeBucketOf(size int64) int {
	for i, bucket := range sizeBuckets {
		if size <= bucket.limit {
			return i
		}
	}

	return len(sizeBuckets) - 1
}

func ageBucketOf(modified, now time.Time) int {
	days := now.Sub(modified).Hours() / 24

	for i, bucket := range ageBuckets {
		if days <= bucket.days {
			return i
		}
	}

	return len(ageBuckets) - 1
}

// objectRef names one object of the listing, for the extremes.
type objectRef struct {
	key      string
	size     int64
	modified time.Time
}

// bucketStats is what a scan adds up to. Only whole pages are merged into it,
// so what is on screen is always a consistent state.
type bucketStats struct {
	current    group            // the newest version of a key, what is there now
	noncurrent group            // older versions, they still take storage
	deleted    int64            // keys whose newest version is a delete marker
	markers    int64            // delete markers, the current and the older ones
	classes    map[string]group // every stored version, by storage class
	sizes      []int64          // current objects per size bucket
	ages       []int64          // current objects per age bucket
	oldest     objectRef
	newest     objectRef
	largest    objectRef
	entries    int64 // what the listing returned, the progress is measured in it
	pages      int
}

func newStats() bucketStats {
	return bucketStats{
		classes: map[string]group{},
		sizes:   make([]int64, len(sizeBuckets)),
		ages:    make([]int64, len(ageBuckets)),
	}
}

// stored is everything which takes space: the current objects and the older
// versions of them.
func (s bucketStats) stored() group { return s.current.plus(s.noncurrent) }

// class books one stored version under its storage class.
func (s *bucketStats) class(name string, size int64) {
	if name == "" {
		name = "STANDARD"
	}

	entry := s.classes[name]
	entry.add(size)
	s.classes[name] = entry
}

// observe files one current object into the histograms and the extremes.
func (s *bucketStats) observe(key string, size int64, modified, now time.Time) {
	s.sizes[sizeBucketOf(size)]++

	ref := objectRef{key: key, size: size, modified: modified}

	if !modified.IsZero() {
		s.ages[ageBucketOf(modified, now)]++

		if s.oldest.key == "" || modified.Before(s.oldest.modified) {
			s.oldest = ref
		}

		if s.newest.key == "" || modified.After(s.newest.modified) {
			s.newest = ref
		}
	}

	if s.largest.key == "" || size > s.largest.size {
		s.largest = ref
	}
}

// merge adds a finished page to the running total.
func (s *bucketStats) merge(page bucketStats) {
	s.current = s.current.plus(page.current)
	s.noncurrent = s.noncurrent.plus(page.noncurrent)
	s.deleted += page.deleted
	s.markers += page.markers
	s.entries += page.entries
	s.pages += page.pages

	for name, entry := range page.classes {
		s.classes[name] = s.classes[name].plus(entry)
	}

	for i, count := range page.sizes {
		s.sizes[i] += count
	}

	for i, count := range page.ages {
		s.ages[i] += count
	}

	if page.oldest.key != "" && (s.oldest.key == "" || page.oldest.modified.Before(s.oldest.modified)) {
		s.oldest = page.oldest
	}

	if page.newest.key != "" && (s.newest.key == "" || page.newest.modified.After(s.newest.modified)) {
		s.newest = page.newest
	}

	if page.largest.key != "" && (s.largest.key == "" || page.largest.size > s.largest.size) {
		s.largest = page.largest
	}
}

// statsScan is the state of the scan the statistics view shows.
type statsScan struct {
	bucket   string
	run      int // pages of an older scan are ignored
	versions bool
	running  bool
	note     string // why the numbers are less than what was asked for
	stats    bucketStats
}

type statsPageMsg struct {
	bucket   string
	run      int
	page     bucketStats
	next     *pageToken
	versions bool
	note     string
}

// statsPageCmd reads one page of the listing and adds it up. Versioned
// buckets are scanned with ListObjectVersions, which is the only way to see
// the older versions and the delete markers at all; where that is not allowed
// the scan falls back to the plain listing.
func statsPageCmd(ctx context.Context, client *awsclient.Client, bucket string, run int, versions bool, token *pageToken) tea.Cmd {
	return func() tea.Msg {
		s3Client, err := client.ForBucket(ctx, bucket)
		if err != nil {
			return fail("resolving bucket region", err)
		}

		if versions {
			page, next, err := scanVersions(ctx, s3Client, bucket, token)
			if err == nil {
				return statsPageMsg{bucket: bucket, run: run, page: page, next: next, versions: true}
			}

			if token != nil {
				return fail(fmt.Sprintf("scanning %q", bucket), err)
			}

			note := describeMissing(err, "no versions")
			if note == "no versions" {
				note = err.Error()
			}

			page, next, listErr := scanCurrent(ctx, s3Client, bucket, nil)
			if listErr != nil {
				return fail(fmt.Sprintf("scanning %q", bucket), listErr)
			}

			return statsPageMsg{
				bucket: bucket, run: run, page: page, next: next,
				note: "older versions need s3:ListBucketVersions: " + note,
			}
		}

		page, next, err := scanCurrent(ctx, s3Client, bucket, token)
		if err != nil {
			return fail(fmt.Sprintf("scanning %q", bucket), err)
		}

		return statsPageMsg{bucket: bucket, run: run, page: page, next: next}
	}
}

// scanVersions adds up one page of the version listing: the whole bucket, no
// delimiter, so every key below every prefix is counted.
func scanVersions(ctx context.Context, client *s3.Client, bucket string, token *pageToken) (bucketStats, *pageToken, error) {
	input := &s3.ListObjectVersionsInput{Bucket: aws.String(bucket), MaxKeys: aws.Int32(pageSize)}
	if token != nil {
		input.KeyMarker, input.VersionIdMarker = token.keyMarker, token.versionMarker
	}

	resp, err := client.ListObjectVersions(ctx, input)
	if err != nil {
		return bucketStats{}, nil, err
	}

	page, now := newStats(), time.Now()
	page.pages = 1

	for _, version := range resp.Versions {
		size := aws.ToInt64(version.Size)
		page.entries++
		page.class(string(version.StorageClass), size)

		if !aws.ToBool(version.IsLatest) {
			page.noncurrent.add(size)

			continue
		}

		page.current.add(size)
		page.observe(aws.ToString(version.Key), size, timeOf(version.LastModified), now)
	}

	for _, marker := range resp.DeleteMarkers {
		page.entries++
		page.markers++

		if aws.ToBool(marker.IsLatest) {
			page.deleted++
		}
	}

	var next *pageToken
	if aws.ToBool(resp.IsTruncated) {
		next = &pageToken{keyMarker: resp.NextKeyMarker, versionMarker: resp.NextVersionIdMarker}
	}

	return page, next, nil
}

// scanCurrent adds up one page of the plain listing, which only knows the
// objects which are there right now.
func scanCurrent(ctx context.Context, client *s3.Client, bucket string, token *pageToken) (bucketStats, *pageToken, error) {
	input := &s3.ListObjectsV2Input{Bucket: aws.String(bucket), MaxKeys: aws.Int32(pageSize)}
	if token != nil {
		input.ContinuationToken = token.continuation
	}

	resp, err := client.ListObjectsV2(ctx, input)
	if err != nil {
		return bucketStats{}, nil, err
	}

	page, now := newStats(), time.Now()
	page.pages = 1

	for _, object := range resp.Contents {
		size := aws.ToInt64(object.Size)
		page.entries++
		page.class(string(object.StorageClass), size)
		page.current.add(size)
		page.observe(aws.ToString(object.Key), size, timeOf(object.LastModified), now)
	}

	var next *pageToken
	if resp.NextContinuationToken != nil {
		next = &pageToken{continuation: resp.NextContinuationToken}
	}

	return page, next, nil
}

func timeOf(t *time.Time) time.Time {
	if t == nil {
		return time.Time{}
	}

	return *t
}

// statLabelWidth aligns the labels of every row of the statistics.
const statLabelWidth = 22

// view renders the whole page.
func (s statsScan) view() string {
	blocks := []string{section("s3://"+s.bucket, s.summary(), "")}

	if len(s.stats.classes) > 0 {
		blocks = append(blocks, section("storage classes", s.storageClasses(), ""))
	}

	if s.stats.current.count > 0 {
		blocks = append(blocks,
			section("object size", histogram(sizeLabels(), s.stats.sizes), ""),
			section("object age", histogram(ageLabels(), s.stats.ages), ""),
			section("outliers", s.outliers(), ""),
		)
	}

	return strings.Join(blocks, "\n")
}

// summary is what the bucket holds, in one block.
func (s statsScan) summary() string {
	rows := []string{statRow("current objects", s.stats.current.count, s.stats.current.bytes, true)}

	if s.versions {
		stored := s.stats.stored()

		rows = append(rows,
			statRow("non-current versions", s.stats.noncurrent.count, s.stats.noncurrent.bytes, true),
			statRow("delete markers", s.stats.markers, 0, false),
			statRow("deleted keys", s.stats.deleted, 0, false),
			"",
			statRow("stored versions", stored.count, stored.bytes, true),
		)
	}

	rows = append(rows, "", "  "+styleDim.Render(s.progress()))

	if s.note != "" {
		rows = append(rows, "  "+styleWarn.Render(s.note))
	}

	return strings.Join(rows, "\n")
}

// progress says how far the listing got, it is the only thing which moves
// while a big bucket is scanned.
func (s statsScan) progress() string {
	entries := fmt.Sprintf("%s entries in %s requests",
		humanize.Comma(s.stats.entries), humanize.Comma(int64(s.stats.pages)))

	if s.running {
		return "scanning… " + entries + ", esc stops"
	}

	return "scanned " + entries
}

// storageClasses lists what lies in which class, the biggest one first.
func (s statsScan) storageClasses() string {
	names := make([]string, 0, len(s.stats.classes))
	for name := range s.stats.classes {
		names = append(names, name)
	}

	sort.Slice(names, func(a, b int) bool {
		first, second := s.stats.classes[names[a]], s.stats.classes[names[b]]
		if first.bytes != second.bytes {
			return first.bytes > second.bytes
		}

		return names[a] < names[b]
	})

	rows := make([]string, 0, len(names))
	for _, name := range names {
		entry := s.stats.classes[name]
		rows = append(rows, statRow(name, entry.count, entry.bytes, true))
	}

	return strings.Join(rows, "\n")
}

// outliers are the objects worth looking at: the ends of both histograms.
func (s statsScan) outliers() string {
	rows := []string{
		refRow("largest", s.stats.largest),
		refRow("oldest", s.stats.oldest),
		refRow("newest", s.stats.newest),
	}

	kept := rows[:0]

	for _, row := range rows {
		if row != "" {
			kept = append(kept, row)
		}
	}

	return strings.Join(kept, "\n")
}

func refRow(label string, ref objectRef) string {
	if ref.key == "" {
		return ""
	}

	return "  " + cell(label, 10, false) + cell(formatSize(ref.size), sizeWidth, true) + "  " +
		cell(formatTime(ref.modified), timeWidth, false) + "  " + ref.key
}

// statRow is one label with a count and, where bytes mean something, a size.
func statRow(label string, count, bytes int64, withBytes bool) string {
	row := "  " + cell(label, statLabelWidth, false) + cell(humanize.Comma(count), 12, true)

	if withBytes {
		row += "  " + cell(formatSize(bytes), sizeWidth, true)
	}

	return row
}

// histogram renders counts as bars relative to the fullest bucket.
func histogram(labels []string, counts []int64) string {
	const barWidth = 24

	var largest int64
	for _, count := range counts {
		largest = max(largest, count)
	}

	rows := make([]string, 0, len(labels))

	for i, label := range labels {
		bar := ""
		if largest > 0 && counts[i] > 0 {
			bar = strings.Repeat("█", max(1, int(counts[i]*barWidth/largest)))
		}

		rows = append(rows, "  "+cell(label, statLabelWidth, false)+
			cell(humanize.Comma(counts[i]), 12, true)+"  "+styleAccent.Render(bar))
	}

	return strings.Join(rows, "\n")
}

func sizeLabels() []string {
	labels := make([]string, 0, len(sizeBuckets))
	for _, bucket := range sizeBuckets {
		labels = append(labels, bucket.label)
	}

	return labels
}

func ageLabels() []string {
	labels := make([]string, 0, len(ageBuckets))
	for _, bucket := range ageBuckets {
		labels = append(labels, bucket.label)
	}

	return labels
}

// openStats scans the bucket and counts what it finds. The scan is a listing
// of the whole bucket, so it costs one request per 1000 entries: it reports
// while it runs and esc stops it.
func (m Model) openStats() (tea.Model, tea.Cmd) {
	if m.bucket == "" {
		return m, nil
	}

	m.view = viewStats
	m.err, m.status = nil, ""
	m.stats = statsScan{
		bucket:   m.bucket,
		run:      m.stats.run + 1,
		versions: m.versioning[m.bucket] != versioningDisabled,
		running:  true,
		stats:    newStats(),
	}
	m.details.GotoTop()
	m.renderStats()
	m.inFlight++
	ctx := m.startLoad()

	return m, m.scopeLoad(statsPageCmd(ctx, m.client, m.stats.bucket, m.stats.run, m.stats.versions, nil))
}

// renderStats rebuilds the content of the statistics view.
func (m *Model) renderStats() {
	m.detailsContent = m.stats.view()
	m.details.SetContent(m.detailsContent)
}
