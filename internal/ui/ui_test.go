package ui

import (
	"bytes"
	"context"
	"encoding/xml"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"slices"
	"sort"
	"strconv"
	"strings"
	"sync"
	"testing"

	"github.com/charmbracelet/bubbles/spinner"
	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"

	"github.com/sj14/s3tui/internal/awsclient"
	"github.com/sj14/s3tui/internal/config"
)

// fakeServer is a minimal S3 endpoint: it lists, serves and accepts objects.
type fakeServer struct {
	*httptest.Server

	mu      sync.Mutex
	objects map[string][]byte
	puts    map[string][]byte
	markers map[string]bool   // keys whose newest version is a delete marker
	deletes map[string]string // key -> versionId
	deleted []string          // every delete in order, as "key@versionId"
	aborts  map[string]string // key -> uploadId

	// resources are the bucket level documents, keyed by their sub-resource:
	// "policy", "lifecycle", "acl", "cors" and so on.
	resources map[string]*configResource

	lastListPrefix string // the Prefix of the last ListObjectsV2
}

func (f *fakeServer) listedPrefix() string {
	f.mu.Lock()
	defer f.mu.Unlock()

	return f.lastListPrefix
}

// configResource is one bucket level document the fake serves. A bucket
// without one answers with code, or with fallback where S3 always has an
// answer – every bucket has an ACL and a versioning state.
type configResource struct {
	store    map[string]string // bucket -> stored body
	code     string            // error code for a bucket which has none
	fallback string            // served instead when code is empty
	json     bool              // the body is JSON, everything else is XML
}

// config returns the document a bucket has for one sub-resource.
func (f *fakeServer) config(resource, bucket string) (string, bool) {
	f.mu.Lock()
	defer f.mu.Unlock()

	document, ok := f.resources[resource].store[bucket]

	return document, ok
}

func (f *fakeServer) setConfig(resource, bucket, document string) {
	f.mu.Lock()
	defer f.mu.Unlock()

	if document == "" {
		delete(f.resources[resource].store, bucket)

		return
	}

	f.resources[resource].store[bucket] = document
}

func (f *fakeServer) recordDelete(key, versionID string) {
	f.mu.Lock()
	defer f.mu.Unlock()

	f.deletes[key] = versionID
	f.deleted = append(f.deleted, key+"@"+versionID)

	// a versioned delete only adds a marker, the key itself stays
	if versionID == "" {
		delete(f.objects, key)
	}
}

// deleteLog is every delete the server saw, as "key@versionId".
func (f *fakeServer) deleteLog() []string {
	f.mu.Lock()
	defer f.mu.Unlock()

	return slices.Clone(f.deleted)
}

func (f *fakeServer) deletedVersion(key string) (string, bool) {
	f.mu.Lock()
	defer f.mu.Unlock()

	version, ok := f.deletes[key]

	return version, ok
}

func (f *fakeServer) recordAbort(key, uploadID string) {
	f.mu.Lock()
	defer f.mu.Unlock()

	f.aborts[key] = uploadID
}

func (f *fakeServer) aborted(key string) (string, bool) {
	f.mu.Lock()
	defer f.mu.Unlock()

	uploadID, ok := f.aborts[key]

	return uploadID, ok
}

func (f *fakeServer) put(key string, body []byte) {
	f.mu.Lock()
	defer f.mu.Unlock()

	f.puts[key] = body
	f.objects[key] = body
}

func (f *fakeServer) uploaded() map[string][]byte {
	f.mu.Lock()
	defer f.mu.Unlock()

	out := make(map[string][]byte, len(f.puts))
	for k, v := range f.puts {
		out[k] = v
	}

	return out
}

func (f *fakeServer) object(key string) ([]byte, bool) {
	f.mu.Lock()
	defer f.mu.Unlock()

	body, ok := f.objects[key]

	return body, ok
}

// listObjects renders a ListObjectsV2 response over the stored objects,
// grouped by the "/" delimiter, so the Prefix of a request really narrows it.
func (f *fakeServer) listObjects(w http.ResponseWriter, bucket, prefix string) {
	f.mu.Lock()
	f.lastListPrefix = prefix

	keys := make([]string, 0, len(f.objects))
	sizes := make(map[string]int, len(f.objects))

	for key, body := range f.objects {
		keys = append(keys, key)
		sizes[key] = len(body)
	}

	f.mu.Unlock()
	sort.Strings(keys)

	var contents, prefixes strings.Builder

	seen := map[string]bool{}

	for _, key := range keys {
		if !strings.HasPrefix(key, prefix) {
			continue
		}

		if idx := strings.Index(key[len(prefix):], "/"); idx >= 0 {
			common := key[:len(prefix)+idx+1]
			if !seen[common] {
				seen[common] = true

				fmt.Fprintf(&prefixes, "<CommonPrefixes><Prefix>%s</Prefix></CommonPrefixes>", common)
			}

			continue
		}

		fmt.Fprintf(&contents,
			`<Contents><Key>%s</Key><Size>%d</Size><LastModified>2024-03-02T10:00:00.000Z</LastModified><StorageClass>STANDARD</StorageClass></Contents>`,
			key, sizes[key])
	}

	fmt.Fprintf(w, `<?xml version="1.0" encoding="UTF-8"?>
<ListBucketResult><Name>%s</Name><Prefix>%s</Prefix><IsTruncated>false</IsTruncated>%s%s</ListBucketResult>`,
		bucket, prefix, prefixes.String(), contents.String())
}

// listVersionsDelimited answers the listing the "deleted" switch asks for:
// the newest version of every key below prefix, delete markers included.
func (f *fakeServer) listVersionsDelimited(w http.ResponseWriter, prefix string) {
	f.mu.Lock()

	keys := make([]string, 0, len(f.objects)+len(f.markers))
	sizes := map[string]int{}

	for key, body := range f.objects {
		keys = append(keys, key)
		sizes[key] = len(body)
	}

	deleted := map[string]bool{}

	for key := range f.markers {
		keys = append(keys, key)
		deleted[key] = true
	}

	f.mu.Unlock()
	sort.Strings(keys)

	var entries, prefixes strings.Builder

	seen := map[string]bool{}

	for _, key := range keys {
		if !strings.HasPrefix(key, prefix) {
			continue
		}

		if idx := strings.Index(key[len(prefix):], "/"); idx >= 0 {
			common := key[:len(prefix)+idx+1]
			if !seen[common] {
				seen[common] = true

				fmt.Fprintf(&prefixes, "<CommonPrefixes><Prefix>%s</Prefix></CommonPrefixes>", common)
			}

			continue
		}

		if deleted[key] {
			fmt.Fprintf(&entries,
				`<DeleteMarker><Key>%s</Key><VersionId>dm-%s</VersionId><IsLatest>true</IsLatest><LastModified>2024-04-01T08:00:00.000Z</LastModified></DeleteMarker>`,
				key, key)

			continue
		}

		fmt.Fprintf(&entries,
			`<Version><Key>%s</Key><VersionId>v-%s</VersionId><IsLatest>true</IsLatest><Size>%d</Size><LastModified>2024-03-02T10:00:00.000Z</LastModified><StorageClass>STANDARD</StorageClass></Version>`,
			key, key, sizes[key])
	}

	fmt.Fprintf(w, `<?xml version="1.0" encoding="UTF-8"?>
<ListVersionsResult><Name>bucket-a</Name><Prefix>%s</Prefix><IsTruncated>false</IsTruncated>%s%s</ListVersionsResult>`,
		prefix, prefixes.String(), entries.String())
}

// listVersionsOf answers with one entry per stored key below prefix, a delete
// marker where the key only exists as one. Only file.txt has a history of its
// own, everything else exists exactly once.
func (f *fakeServer) listVersionsOf(w http.ResponseWriter, prefix string) {
	f.mu.Lock()

	keys := make([]string, 0, len(f.objects)+len(f.markers))
	sizes := make(map[string]int, len(f.objects))

	for key, body := range f.objects {
		if !strings.HasPrefix(key, prefix) {
			continue
		}

		keys = append(keys, key)
		sizes[key] = len(body)
	}

	deleted := map[string]bool{}

	for key := range f.markers {
		if !strings.HasPrefix(key, prefix) {
			continue
		}

		keys = append(keys, key)
		deleted[key] = true
	}

	f.mu.Unlock()
	sort.Strings(keys)

	var entries strings.Builder

	for _, key := range keys {
		if deleted[key] {
			fmt.Fprintf(&entries,
				`<DeleteMarker><Key>%s</Key><VersionId>dm-%s</VersionId><IsLatest>true</IsLatest><LastModified>2024-04-01T08:00:00.000Z</LastModified></DeleteMarker>`,
				key, key)

			continue
		}

		fmt.Fprintf(&entries,
			`<Version><Key>%s</Key><VersionId>only</VersionId><IsLatest>true</IsLatest><Size>%d</Size><LastModified>2024-03-02T10:00:00.000Z</LastModified><StorageClass>STANDARD</StorageClass></Version>`,
			key, sizes[key])
	}

	fmt.Fprintf(w, `<?xml version="1.0" encoding="UTF-8"?>
<ListVersionsResult><IsTruncated>false</IsTruncated>%s</ListVersionsResult>`, entries.String())
}

// serveObject answers a GET, including the ranged requests of the downloader.
func (f *fakeServer) serveObject(w http.ResponseWriter, r *http.Request, body []byte) {
	w.Header().Set("Content-Type", "application/octet-stream")
	w.Header().Set("Accept-Ranges", "bytes")

	start, end := 0, len(body)-1

	if header := r.Header.Get("Range"); strings.HasPrefix(header, "bytes=") {
		parts := strings.SplitN(strings.TrimPrefix(header, "bytes="), "-", 2)

		if v, err := strconv.Atoi(parts[0]); err == nil {
			start = v
		}
		if len(parts) > 1 && parts[1] != "" {
			if v, err := strconv.Atoi(parts[1]); err == nil && v < end {
				end = v
			}
		}

		if start > end || start >= len(body) {
			w.WriteHeader(http.StatusRequestedRangeNotSatisfiable)

			return
		}

		w.Header().Set("Content-Range", fmt.Sprintf("bytes %d-%d/%d", start, end, len(body)))
		w.Header().Set("Content-Length", strconv.Itoa(end-start+1))
		w.WriteHeader(http.StatusPartialContent)
		w.Write(body[start : end+1])

		return
	}

	w.Header().Set("Content-Length", strconv.Itoa(len(body)))
	w.Write(body)
}

// fakeS3 serves the operations the TUI needs.
func fakeS3(t *testing.T) *fakeServer {
	t.Helper()

	fake := &fakeServer{
		objects: map[string][]byte{
			"file.txt":        bytes.Repeat([]byte("x"), 200),
			"logs/nested.log": bytes.Repeat([]byte("l"), 42),
			"logs/old.log":    bytes.Repeat([]byte("o"), 17),
		},
		puts:    map[string][]byte{},
		markers: map[string]bool{"gone.txt": true, "logs/vanished.log": true},
		deletes: map[string]string{},
		aborts:  map[string]string{},

		resources: map[string]*configResource{
			"policy": {
				store: map[string]string{"bucket-a": defaultPolicy},
				code:  "NoSuchBucketPolicy",
				json:  true,
			},
			"lifecycle": {
				store: map[string]string{"bucket-a": defaultLifecycle},
				code:  "NoSuchLifecycleConfiguration",
			},
			"acl": {
				store:    map[string]string{},
				fallback: defaultACL,
			},
			"publicAccessBlock": {
				store: map[string]string{"bucket-a": defaultPublicAccess},
				code:  "NoSuchPublicAccessBlockConfiguration",
			},
			"cors": {
				store: map[string]string{"bucket-a": defaultCORS},
				code:  "NoSuchCORSConfiguration",
			},
			"versioning": {
				store: map[string]string{
					"bucket-a": defaultVersioning,
					"bucket-b": defaultVersioning,
				},
				fallback: `<VersioningConfiguration></VersioningConfiguration>`,
			},
			"object-lock": {
				store: map[string]string{"bucket-a": defaultObjectLock},
				code:  "ObjectLockConfigurationNotFoundError",
			},
			"tagging": {
				store:    map[string]string{"bucket-a/file.txt": defaultTags},
				fallback: `<Tagging><TagSet></TagSet></Tagging>`,
			},
			"legal-hold": {
				store: map[string]string{"bucket-a/file.txt": defaultLegalHold},
				code:  "NoSuchObjectLockConfiguration",
			},
			"retention": {
				store: map[string]string{"bucket-a/file.txt": defaultRetention},
				code:  "NoSuchObjectLockConfiguration",
			},
		},
	}

	fake.Server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		query := r.URL.Query()
		bucket, _, _ := strings.Cut(strings.TrimPrefix(r.URL.Path, "/"), "/")
		key := strings.TrimPrefix(r.URL.Path, "/")
		if idx := strings.Index(key, "/"); idx >= 0 {
			key = key[idx+1:] // strip the bucket of the path style URL
		} else {
			key = ""
		}

		if r.Method == http.MethodHead && key != "" {
			body, ok := fake.object(key)
			if !ok {
				w.WriteHeader(http.StatusNotFound)

				return
			}

			w.Header().Set("Content-Type", "text/plain")
			w.Header().Set("Content-Length", strconv.Itoa(len(body)))
			w.Header().Set("ETag", `"d41d8cd98f00b204e9800998ecf8427e"`)
			w.Header().Set("Last-Modified", "Sat, 02 Mar 2024 10:00:00 GMT")
			w.Header().Set("Cache-Control", "max-age=3600")
			w.Header().Set("x-amz-storage-class", "STANDARD_IA")
			w.Header().Set("x-amz-server-side-encryption", "AES256")
			w.Header().Set("x-amz-meta-owner", "simon")

			if version := query.Get("versionId"); version != "" {
				w.Header().Set("x-amz-version-id", version)
			}

			return
		}

		if name := configResourceName(query); name != "" {
			switch r.Method {
			case http.MethodPut:
				body, _ := io.ReadAll(r.Body)
				fake.setConfig(name, configTargetOf(bucket, key), string(body))
				w.WriteHeader(http.StatusOK)

				return
			case http.MethodDelete:
				fake.setConfig(name, configTargetOf(bucket, key), "")
				w.WriteHeader(http.StatusNoContent)

				return
			}
		}

		if r.Method == http.MethodDelete {
			if uploadID := query.Get("uploadId"); uploadID != "" {
				fake.recordAbort(key, uploadID)
				w.WriteHeader(http.StatusNoContent)

				return
			}

			fake.recordDelete(key, query.Get("versionId"))
			w.WriteHeader(http.StatusNoContent)

			return
		}

		if r.Method == http.MethodPost && query.Has("delete") {
			var payload struct {
				XMLName xml.Name `xml:"Delete"`
				Objects []struct {
					Key       string `xml:"Key"`
					VersionID string `xml:"VersionId"`
				} `xml:"Object"`
			}

			if err := xml.NewDecoder(r.Body).Decode(&payload); err != nil {
				w.WriteHeader(http.StatusBadRequest)

				return
			}

			for _, object := range payload.Objects {
				fake.recordDelete(object.Key, object.VersionID)
			}

			w.Header().Set("Content-Type", "application/xml")
			fmt.Fprint(w, `<DeleteResult></DeleteResult>`)

			return
		}

		if r.Method == http.MethodPut {
			if source := r.Header.Get("X-Amz-Copy-Source"); source != "" {
				unescaped, err := url.PathUnescape(strings.TrimPrefix(source, "/"))
				if err != nil {
					w.WriteHeader(http.StatusBadRequest)

					return
				}

				srcKey := unescaped
				if _, after, found := strings.Cut(unescaped, "/"); found {
					srcKey = after
				}
				srcKey, _, _ = strings.Cut(srcKey, "?") // drop a versionId

				body, ok := fake.object(srcKey)
				if !ok {
					w.WriteHeader(http.StatusNotFound)

					return
				}

				fake.put(key, body)
				w.Header().Set("Content-Type", "application/xml")
				fmt.Fprint(w, `<CopyObjectResult><ETag>"fake"</ETag><LastModified>2024-03-02T10:00:00.000Z</LastModified></CopyObjectResult>`)

				return
			}

			body, _ := io.ReadAll(r.Body)
			fake.put(key, body)
			w.Header().Set("ETag", `"fake"`)

			return
		}

		// the SDK appends ?x-id=GetObject, so match on the listing markers
		isListing := query.Has("list-type") || query.Has("versions") ||
			query.Has("location") || query.Has("uploads") || query.Has("uploadId") ||
			configResourceName(query) != ""

		if r.Method == http.MethodGet && key != "" && !isListing {
			body, ok := fake.object(key)
			if !ok {
				w.WriteHeader(http.StatusNotFound)

				return
			}

			fake.serveObject(w, r, body)

			return
		}

		w.Header().Set("Content-Type", "application/xml")

		if name := configResourceName(query); name != "" {
			fake.serveConfig(w, name, configTargetOf(bucket, key))

			return
		}

		switch {
		case query.Has("uploads"):
			if strings.HasPrefix(r.URL.Path, "/bucket-b") {
				fmt.Fprint(w, `<?xml version="1.0" encoding="UTF-8"?>
<ListMultipartUploadsResult><Bucket>bucket-b</Bucket><IsTruncated>false</IsTruncated></ListMultipartUploadsResult>`)

				return
			}

			fmt.Fprint(w, `<?xml version="1.0" encoding="UTF-8"?>
<ListMultipartUploadsResult><Bucket>bucket-a</Bucket><IsTruncated>false</IsTruncated>
<Upload><Key>big.iso</Key><UploadId>upload-1</UploadId><Initiated>2024-04-01T08:00:00.000Z</Initiated><StorageClass>STANDARD</StorageClass></Upload>
<Upload><Key>logs/huge.log</Key><UploadId>upload-2</UploadId><Initiated>2024-04-02T09:30:00.000Z</Initiated><StorageClass>GLACIER</StorageClass></Upload>
</ListMultipartUploadsResult>`)

		case query.Has("uploadId"):
			fmt.Fprint(w, `<?xml version="1.0" encoding="UTF-8"?>
<ListPartsResult><Bucket>bucket-a</Bucket><Key>big.iso</Key><UploadId>upload-1</UploadId><IsTruncated>false</IsTruncated>
<Part><PartNumber>1</PartNumber><ETag>"aaa"</ETag><Size>5242880</Size><LastModified>2024-04-01T08:01:00.000Z</LastModified></Part>
<Part><PartNumber>2</PartNumber><ETag>"bbb"</ETag><Size>1048576</Size><LastModified>2024-04-01T08:02:00.000Z</LastModified></Part>
</ListPartsResult>`)

		case r.URL.Path == "/":
			fmt.Fprint(w, `<?xml version="1.0" encoding="UTF-8"?>
<ListAllMyBucketsResult><Buckets>
<Bucket><Name>bucket-a</Name><CreationDate>2024-01-02T03:04:05.000Z</CreationDate><BucketRegion>eu-central-1</BucketRegion></Bucket>
<Bucket><Name>bucket-b</Name><CreationDate>2023-06-07T08:09:10.000Z</CreationDate><BucketRegion>eu-central-1</BucketRegion></Bucket>
</Buckets></ListAllMyBucketsResult>`)

		case query.Has("versions") && query.Get("delimiter") == "/":
			if strings.HasPrefix(r.URL.Path, "/bucket-b") {
				w.WriteHeader(http.StatusForbidden)
				fmt.Fprint(w, `<Error><Code>AccessDenied</Code><Message>not authorized to perform s3:ListBucketVersions</Message></Error>`)

				return
			}

			fake.listVersionsDelimited(w, query.Get("prefix"))

		case query.Has("versions") && query.Get("prefix") == "":
			// the whole bucket, which the statistics scan walks through
			if strings.HasPrefix(r.URL.Path, "/bucket-b") {
				w.WriteHeader(http.StatusForbidden)
				fmt.Fprint(w, `<Error><Code>AccessDenied</Code><Message>not authorized to perform s3:ListBucketVersions</Message></Error>`)

				return
			}

			if query.Get("key-marker") == "" {
				fmt.Fprint(w, `<?xml version="1.0" encoding="UTF-8"?>
<ListVersionsResult><Name>bucket-a</Name>
<Version><Key>big.iso</Key><VersionId>v1</VersionId><IsLatest>true</IsLatest><Size>2097152</Size><LastModified>2024-03-02T10:00:00.000Z</LastModified><StorageClass>STANDARD</StorageClass></Version>
<Version><Key>file.txt</Key><VersionId>v2</VersionId><IsLatest>true</IsLatest><Size>200</Size><LastModified>2024-03-02T10:00:00.000Z</LastModified><StorageClass>STANDARD</StorageClass></Version>
<Version><Key>file.txt</Key><VersionId>v1</VersionId><IsLatest>false</IsLatest><Size>100</Size><LastModified>2024-03-01T10:00:00.000Z</LastModified><StorageClass>GLACIER</StorageClass></Version>
<DeleteMarker><Key>file.txt</Key><VersionId>dm1</VersionId><IsLatest>false</IsLatest><LastModified>2024-03-01T12:00:00.000Z</LastModified></DeleteMarker>
<IsTruncated>true</IsTruncated><NextKeyMarker>file.txt</NextKeyMarker><NextVersionIdMarker>dm1</NextVersionIdMarker>
</ListVersionsResult>`)

				return
			}

			fmt.Fprint(w, `<?xml version="1.0" encoding="UTF-8"?>
<ListVersionsResult><Name>bucket-a</Name>
<Version><Key>logs/nested.log</Key><VersionId>only</VersionId><IsLatest>true</IsLatest><Size>50</Size><LastModified>2024-02-01T10:00:00.000Z</LastModified><StorageClass>STANDARD</StorageClass></Version>
<DeleteMarker><Key>gone.txt</Key><VersionId>dm2</VersionId><IsLatest>true</IsLatest><LastModified>2024-04-01T08:00:00.000Z</LastModified></DeleteMarker>
<IsTruncated>false</IsTruncated>
</ListVersionsResult>`)

		case query.Has("versions"):
			// the version history below a prefix: one key for the version
			// view, a whole prefix for the hard delete
			if prefix := query.Get("prefix"); prefix != "file.txt" {
				fake.listVersionsOf(w, prefix)

				return
			}

			fmt.Fprint(w, `<?xml version="1.0" encoding="UTF-8"?>
<ListVersionsResult>
<Version><Key>file.txt</Key><VersionId>v2</VersionId><IsLatest>true</IsLatest><Size>200</Size><LastModified>2024-03-02T10:00:00.000Z</LastModified><StorageClass>STANDARD</StorageClass></Version>
<Version><Key>file.txt</Key><VersionId>v1</VersionId><IsLatest>false</IsLatest><Size>100</Size><LastModified>2024-03-01T10:00:00.000Z</LastModified><StorageClass>GLACIER</StorageClass></Version>
<DeleteMarker><Key>file.txt</Key><VersionId>dm1</VersionId><IsLatest>false</IsLatest><LastModified>2024-03-01T12:00:00.000Z</LastModified></DeleteMarker>
<IsTruncated>false</IsTruncated>
</ListVersionsResult>`)

		default:
			fake.listObjects(w, strings.TrimPrefix(r.URL.Path, "/"), query.Get("prefix"))
		}
	}))

	t.Cleanup(fake.Close)

	return fake
}

// step feeds a message into the model and runs the resulting commands.
func step(t *testing.T, model tea.Model, msg tea.Msg) tea.Model {
	t.Helper()

	updated, cmd := model.Update(msg)

	return runCmd(t, updated, cmd, 0)
}

func runCmd(t *testing.T, model tea.Model, cmd tea.Cmd, depth int) tea.Model {
	t.Helper()

	if cmd == nil || depth > 400 {
		return model
	}

	switch msg := cmd().(type) {
	case nil:
	case spinner.TickMsg: // would tick forever
	case tea.BatchMsg:
		for _, batched := range msg {
			model = runCmd(t, model, batched, depth+1)
		}
	default:
		// the blinking cursor of a prompt would blink forever
		if strings.Contains(fmt.Sprintf("%T", msg), "cursor.") {
			return model
		}

		updated, next := model.Update(msg)
		model = runCmd(t, updated, next, depth+1)
	}

	return model
}

func newTestModel(t *testing.T, endpoint string) tea.Model {
	t.Helper()

	profile := config.Profile{
		Endpoint:  endpoint,
		Region:    "eu-central-1",
		AccessKey: "key",
		SecretKey: "secret",
		PathStyle: true,
	}

	client, err := awsclient.New(t.Context(), profile, "test")
	if err != nil {
		t.Fatalf("creating client: %v", err)
	}

	cfg := config.Config{Profiles: map[string]config.Profile{"default": profile, "other": {Endpoint: endpoint, Region: "us-east-1"}}}

	model := tea.Model(New(context.Background(), client, "default", profile, cfg, "test"))
	model = step(t, model, tea.WindowSizeMsg{Width: 100, Height: 24})

	return runCmd(t, model, model.Init(), 0)
}

func enter(t *testing.T, model tea.Model) tea.Model {
	t.Helper()

	return step(t, model, tea.KeyMsg{Type: tea.KeyEnter})
}

func TestBrowseBucketsObjectsVersions(t *testing.T) {
	server := fakeS3(t)
	defer server.Close()

	model := newTestModel(t, server.URL)

	// the buckets are listed on startup
	view := model.View()
	for _, want := range []string{"buckets", "bucket-a", "bucket-b", "eu-central-1", "2024-01-02"} {
		if !strings.Contains(view, want) {
			t.Errorf("bucket view misses %q:\n%s", want, view)
		}
	}

	// enter the first bucket and pick the object listing
	model = openSection(t, enter(t, model), menuObjects)

	view = model.View()
	for _, want := range []string{"bucket-a", "versioned", "logs/", "file.txt", "200 B"} {
		if !strings.Contains(view, want) {
			t.Errorf("object view misses %q:\n%s", want, view)
		}
	}
	if got := model.(Model).view; got != viewObjects {
		t.Errorf("view = %v, want objects", got)
	}

	// descend into the "logs/" prefix
	model = enter(t, model)

	if got := model.(Model).prefix; got != "logs/" {
		t.Errorf("prefix = %q, want logs/", got)
	}
	if view := model.View(); !strings.Contains(view, "nested.log") {
		t.Errorf("prefix view misses nested.log:\n%s", view)
	}

	// back to the bucket root
	model = step(t, model, tea.KeyMsg{Type: tea.KeyEsc})

	if got := model.(Model).prefix; got != "" {
		t.Errorf("prefix = %q, want empty", got)
	}

	// select file.txt and open its versions
	inner := model.(Model)
	inner.objects.Select(1)
	model = enter(t, tea.Model(inner))

	if got := model.(Model).view; got != viewVersions {
		t.Errorf("view = %v, want versions", got)
	}

	view = model.View()
	for _, want := range []string{"file.txt", "[versions]", "v2", "[latest]", "v1", "[GLACIER]", "dm1", "[delete-marker]"} {
		if !strings.Contains(view, want) {
			t.Errorf("version view misses %q:\n%s", want, view)
		}
	}

	// back to the objects, then to the overview, then to the buckets
	model = step(t, model, tea.KeyMsg{Type: tea.KeyEsc})
	if got := model.(Model).view; got != viewObjects {
		t.Errorf("view = %v, want objects", got)
	}

	model = step(t, model, tea.KeyMsg{Type: tea.KeyEsc})
	if got := model.(Model).view; got != viewBucketMenu {
		t.Errorf("view = %v, want the bucket overview", got)
	}

	model = step(t, model, tea.KeyMsg{Type: tea.KeyEsc})
	if got := model.(Model).view; got != viewBuckets {
		t.Errorf("view = %v, want buckets", got)
	}
}

func TestUnversionedBucketOpensTheDetails(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/xml")

		switch {
		case r.URL.Path == "/":
			fmt.Fprint(w, `<ListAllMyBucketsResult><Buckets><Bucket><Name>plain</Name></Bucket></Buckets></ListAllMyBucketsResult>`)
		case r.URL.Query().Has("versioning"):
			fmt.Fprint(w, `<VersioningConfiguration></VersioningConfiguration>`)
		default:
			fmt.Fprint(w, `<ListBucketResult><Name>plain</Name><IsTruncated>false</IsTruncated>
<Contents><Key>file.txt</Key><Size>1</Size><LastModified>2024-03-02T10:00:00.000Z</LastModified></Contents>
</ListBucketResult>`)
		}
	}))
	defer server.Close()

	model := openSection(t, enter(t, newTestModel(t, server.URL)), menuObjects)

	if view := model.View(); !strings.Contains(view, "not versioned") {
		t.Errorf("view misses the versioning hint:\n%s", view)
	}

	model = enter(t, model) // there is no version list to step through
	model = openDetails(t, model)

	got := model.(Model)
	if got.view != viewConfig || got.configKind != configObjectMetadata {
		t.Fatalf("view = %v, want the object details", got.view)
	}
	if got.objectKey != "file.txt" {
		t.Errorf("objectKey = %q", got.objectKey)
	}

	// esc returns through the overview to the objects, not to a version list
	for _, want := range []viewState{viewObjectMenu, viewObjects} {
		model = step(t, model, tea.KeyMsg{Type: tea.KeyEsc})

		if got := model.(Model).view; got != want {
			t.Errorf("view = %v, want %v", got, want)
		}
	}
}

func TestKeyNavigation(t *testing.T) {
	server := fakeS3(t)
	defer server.Close()

	model := openBucket(t, server)

	model = step(t, model, tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("j")})
	if got := model.(Model).objects.Index(); got != 1 {
		t.Errorf(`index after "j" = %d, want 1`, got)
	}

	// "j" selected file.txt, so enter opens its versions
	model = enter(t, model)
	if got := model.(Model).view; got != viewVersions {
		t.Errorf("view = %v, want versions", got)
	}
}

func TestParentPrefix(t *testing.T) {
	tests := map[string]string{
		"":         "",
		"a/":       "",
		"a/b/":     "a/",
		"a/b/c/":   "a/b/",
		"no-slash": "",
	}

	for in, want := range tests {
		if got := parentPrefix(in); got != want {
			t.Errorf("parentPrefix(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestLoadingListingShowsNoEmptyMessage(t *testing.T) {
	server := fakeS3(t)

	// pick the object listing but do not deliver the response yet
	model := openBucketMenu(t, server)
	loading, _ := model.Update(tea.KeyMsg{Type: tea.KeyEnter})

	view := loading.View()
	if strings.Contains(view, "No objects") {
		t.Errorf("the empty list message showed up while loading:\n%s", view)
	}
	if !strings.Contains(view, "loading") {
		t.Errorf("no loading placeholder:\n%s", view)
	}
	if got, want := strings.Count(view, "\n"), strings.Count(model.View(), "\n"); got != want {
		t.Errorf("the placeholder changed the layout: %d lines instead of %d", got, want)
	}
}

func TestEmptyListSaysItOnce(t *testing.T) {
	server := fakeS3(t)
	model := newTestModel(t, server.URL)

	inner := model.(Model)
	inner.buckets.Select(1) // bucket-b has no multipart uploads
	model = openSection(t, enter(t, tea.Model(inner)), menuMultipart)

	if got := model.(Model).view; got != viewMultipart {
		t.Fatalf("view = %v, want multipart", got)
	}

	view := model.View()
	if got := strings.Count(view, "No multipart uploads"); got != 1 {
		t.Errorf("the empty message shows %d times, want once:\n%s", got, view)
	}
	if strings.Contains(view, "loading") {
		t.Errorf("still loading after the response arrived:\n%s", view)
	}
}

func TestSearchAsksTheServer(t *testing.T) {
	server := fakeS3(t)
	model := openBucket(t, server) // file.txt, logs/nested.log, logs/old.log

	if got := len(model.(Model).objects.Items()); got != 2 {
		t.Fatalf("got %d entries, want logs/ and file.txt", got)
	}

	model = pressNoRun(model, '/')

	prompt := model.(Model).prompt
	if prompt == nil {
		t.Fatal("no search prompt is open")
	}
	if !strings.Contains(prompt.title, "bucket-a") {
		t.Errorf("title = %q", prompt.title)
	}

	model = submitPrompt(t, model, "file")

	got := model.(Model)
	if got.search != "file" {
		t.Errorf("search = %q", got.search)
	}
	if prefix := server.listedPrefix(); prefix != "file" {
		t.Errorf("the server was asked for prefix %q, want %q", prefix, "file")
	}
	if len(got.objects.Items()) != 1 {
		t.Fatalf("got %d entries, want only file.txt", len(got.objects.Items()))
	}
	if item := got.objects.Items()[0].(objectItem); item.key != "file.txt" {
		t.Errorf("entry = %q, want file.txt", item.key)
	}
	if !strings.Contains(model.View(), "file*") {
		t.Errorf("the breadcrumb does not show the search:\n%s", model.View())
	}

	// esc drops the search and restores the full listing
	model = step(t, model, tea.KeyMsg{Type: tea.KeyEsc})

	got = model.(Model)
	if got.search != "" {
		t.Errorf("search = %q, want it cleared", got.search)
	}
	if got.view != viewObjects {
		t.Errorf("view = %v, esc should clear the search before leaving", got.view)
	}
	if len(got.objects.Items()) != 2 {
		t.Errorf("got %d entries after clearing, want 2", len(got.objects.Items()))
	}
}

func TestSearchKeepsFolderGrouping(t *testing.T) {
	server := fakeS3(t)
	model := openBucket(t, server)

	model = pressNoRun(model, '/')
	model = submitPrompt(t, model, "log")

	items := model.(Model).objects.Items()
	if len(items) != 1 {
		t.Fatalf("got %d entries, want the logs/ prefix", len(items))
	}

	item := items[0].(objectItem)
	if !item.isPrefix || item.key != "logs/" {
		t.Errorf("entry = %+v, want the logs/ prefix", item)
	}

	// entering a prefix drops the search, it belonged to the level above
	model = enter(t, model)

	got := model.(Model)
	if got.search != "" {
		t.Errorf("search = %q, want it cleared when descending", got.search)
	}
	if got.prefix != "logs/" {
		t.Errorf("prefix = %q", got.prefix)
	}
	if len(got.objects.Items()) != 2 {
		t.Errorf("got %d entries in logs/, want 2", len(got.objects.Items()))
	}
}

func TestErrorPopsUpAndIsReadableInFull(t *testing.T) {
	long := strings.Repeat("operation error S3: ListObjectsV2, https response error StatusCode: 403, "+
		"RequestID: ABC123DEF456, HostID: verylonghostidthatmattersforsupport, api error AccessDenied. ", 30)

	server := fakeS3(t)
	model := newTestModel(t, server.URL)
	model = step(t, model, errMsg{what: "listing objects", err: errors.New(long)})

	// the box is there without any key being pressed
	view := model.View()
	for _, want := range []string{"error", "listing objects", "RequestID: ABC123DEF456", "↑/↓ scrolls · esc closes"} {
		if !strings.Contains(view, want) {
			t.Errorf("the error box misses %q:\n%s", want, view)
		}
	}

	// it is drawn over the bucket list, which is still there around it
	if !strings.Contains(view, "bucket-a") {
		t.Errorf("the box replaced the view instead of covering it:\n%s", view)
	}
	quiet := model.(Model)
	quiet.err = nil

	if lipgloss.Height(view) != lipgloss.Height(quiet.View()) {
		t.Errorf("the box changed the height of the screen: %d instead of %d lines",
			lipgloss.Height(view), lipgloss.Height(quiet.View()))
	}

	// scrolling stays inside the box
	scrolled := press(t, model, 'j')
	if got := scrolled.(Model); got.err == nil {
		t.Error("scrolling closed the box")
	}
	if scrolled.View() == view {
		t.Error("the box did not scroll")
	}

	// and any other key acknowledges the error
	model = step(t, scrolled, tea.KeyMsg{Type: tea.KeyEsc})

	got := model.(Model)
	if got.err != nil {
		t.Fatalf("esc did not close the box: %v", got.err)
	}
	if got.view != viewBuckets {
		t.Errorf("view = %v, want the list we were on", got.view)
	}
	if strings.Contains(model.View(), "RequestID") {
		t.Errorf("the error is still on screen:\n%s", model.View())
	}
}

func TestErrorBoxSwallowsTheKeyThatClosesIt(t *testing.T) {
	server := fakeS3(t)
	model := openBucket(t, server)
	model = step(t, model, errMsg{what: "listing objects", err: errors.New("boom")})

	// enter would open something, here it only acknowledges the error
	model = enter(t, model)

	got := model.(Model)
	if got.err != nil {
		t.Fatal("enter did not close the box")
	}
	if got.view != viewObjects {
		t.Errorf("view = %v, enter walked on although it closed the box", got.view)
	}
}

func TestNoKeyOpensAnErrorView(t *testing.T) {
	server := fakeS3(t)

	// "e" edits a bucket configuration, everywhere else it does nothing
	model := press(t, newTestModel(t, server.URL), 'e')

	if got := model.(Model).view; got != viewBuckets {
		t.Errorf("view = %v, want the buckets", got)
	}
	if strings.Contains(model.View(), "error") {
		t.Errorf("a key conjured an error box:\n%s", model.View())
	}
}

func lastLine(view string) string {
	lines := strings.Split(strings.TrimRight(view, "\n"), "\n")

	return lines[len(lines)-1]
}

func TestObjectDetails(t *testing.T) {
	server := fakeS3(t)
	model := openBucket(t, server)

	inner := model.(Model)
	inner.objects.Select(1)            // file.txt
	model = enter(t, tea.Model(inner)) // its versions
	model = enter(t, model)            // the overview of the newest one
	model = openDetails(t, model)

	got := model.(Model)
	if got.view != viewConfig || got.configKind != configObjectMetadata {
		t.Fatalf("view = %v, want the object details", got.view)
	}
	if got.objectKey != "file.txt" {
		t.Errorf("objectKey = %q", got.objectKey)
	}

	view := model.View() + got.detailsContent
	for _, want := range []string{
		"file.txt", "[metadata]",
		"metadata", `"ContentType": "text/plain"`, `"ContentLength": 200`,
		`"ETag": "\"d41d8cd98f00b204e9800998ecf8427e\""`,
		`"StorageClass": "STANDARD_IA"`, `"ServerSideEncryption": "AES256"`,
		`"owner": "simon"`, `"CacheControl": "max-age=3600"`,
	} {
		if !strings.Contains(view, want) {
			t.Errorf("the metadata miss %q:\n%s", want, view)
		}
	}
	if strings.Contains(view, "ResultMetadata") {
		t.Error("the SDK internals leaked into the view")
	}

	// the tags are a page of their own now
	if strings.Contains(view, `"env": "prod"`) {
		t.Errorf("the tags leaked into the metadata:\n%s", view)
	}

	// e does nothing here, HeadObject has no way back in
	edited := press(t, model, 'e')
	if !strings.Contains(edited.(Model).status, "cannot be edited") {
		t.Errorf("status = %q, want the note that metadata is read-only", edited.(Model).status)
	}
	if strings.Contains(model.View(), "e edit") {
		t.Errorf("the footer offers an edit which does not exist:\n%s", model.View())
	}

	model = step(t, model, tea.KeyMsg{Type: tea.KeyEsc})
	model = step(t, model, tea.KeyMsg{Type: tea.KeyEsc})

	if got := model.(Model).view; got != viewVersions {
		t.Errorf("view = %v, want the versions", got)
	}
}

func TestObjectDetailsOfAVersion(t *testing.T) {
	server := fakeS3(t)
	model := openBucket(t, server)

	inner := model.(Model)
	inner.objects.Select(1)
	model = enter(t, tea.Model(inner)) // versions of file.txt

	inner = model.(Model)
	inner.versions.Select(1) // v1
	model = openDetails(t, enter(t, tea.Model(inner)))

	got := model.(Model)
	if got.view != viewConfig || got.configKind != configObjectMetadata {
		t.Fatalf("view = %v, want the object details", got.view)
	}
	if got.objectVersion != "v1" {
		t.Errorf("objectVersion = %q, want v1", got.objectVersion)
	}
	if !strings.Contains(got.detailsContent, `"VersionId": "v1"`) {
		t.Errorf("the details are not those of the version:\n%s", got.detailsContent)
	}

	// esc returns through the overview to the versions, not to the objects
	model = step(t, model, tea.KeyMsg{Type: tea.KeyEsc})
	if got := model.(Model).view; got != viewObjectMenu {
		t.Errorf("view = %v, want the object overview", got)
	}

	model = step(t, model, tea.KeyMsg{Type: tea.KeyEsc})
	if got := model.(Model).view; got != viewVersions {
		t.Errorf("view = %v, want versions", got)
	}
}

func TestObjectTags(t *testing.T) {
	server := fakeS3(t)
	model := openBucket(t, server)

	inner := model.(Model)
	inner.objects.Select(1)            // file.txt
	model = enter(t, tea.Model(inner)) // its versions
	model = openConfigSection(t, enter(t, model), configObjectTags)

	got := model.(Model)
	view := model.View() + got.detailsContent

	for _, want := range []string{"[tags]", "tag set", `"env": "prod"`, `"owner": "platform"`} {
		if !strings.Contains(view, want) {
			t.Errorf("the tag view misses %q:\n%s", want, view)
		}
	}
	if strings.Contains(view, "ContentType") {
		t.Errorf("the metadata leaked into the tags:\n%s", view)
	}
}

func TestObjectWithoutTags(t *testing.T) {
	server := fakeS3(t)
	model := openBucket(t, server)

	model = enter(t, model) // into logs/, whose objects carry no tags

	inner := model.(Model)
	inner.objects.Select(0)
	model = enter(t, tea.Model(inner)) // versions of logs/nested.log
	model = openConfigSection(t, enter(t, model), configObjectTags)

	content := model.(Model).detailsContent
	if !strings.Contains(content, "no tags") {
		t.Errorf("the empty tag set is not reported:\n%s", content)
	}
	if strings.Contains(content, "error") {
		t.Errorf("an empty tag set must not be an error:\n%s", content)
	}
}

func TestPrefixHasNoDetails(t *testing.T) {
	server := fakeS3(t)
	model := openBucket(t, server)

	inner := model.(Model)
	inner.objects.Select(0) // the logs/ prefix
	model = enter(t, tea.Model(inner))

	got := model.(Model)
	if got.view != viewObjects {
		t.Errorf("view = %v, a prefix should just be entered", got.view)
	}
	if got.prefix != "logs/" {
		t.Errorf("prefix = %q, enter should descend", got.prefix)
	}
}

func TestObjectDetailsScroll(t *testing.T) {
	server := fakeS3(t)
	model := openBucket(t, server)

	inner := model.(Model)
	inner.objects.Select(1)
	model = enter(t, tea.Model(inner))
	model = enter(t, model)

	if got := model.(Model).details.YOffset; got != 0 {
		t.Fatalf("the details do not start at the top: %d", got)
	}
	if model.(Model).details.TotalLineCount() <= model.(Model).details.Height {
		t.Skip("the metadata fits on one screen, nothing to scroll")
	}

	model = press(t, model, 'G')

	if got := model.(Model).details.YOffset; got == 0 {
		t.Error(`"G" did not scroll the details`)
	}
	if !strings.Contains(model.View(), "scrolls") {
		t.Errorf("no scroll indicator:\n%s", model.View())
	}

	model = press(t, model, 'g')

	if got := model.(Model).details.YOffset; got != 0 {
		t.Errorf(`"g" did not jump back to the top: %d`, got)
	}
}

func TestShowDeletedKeys(t *testing.T) {
	server := fakeS3(t)
	model := openBucket(t, server)

	for _, item := range model.(Model).objects.Items() {
		if item.(objectItem).key == "gone.txt" {
			t.Fatal("a deleted key showed up in the ordinary listing")
		}
	}

	model = openSection(t, openBucketMenu(t, server), menuDeleted)

	got := model.(Model)
	if !got.showDeleted {
		t.Fatal("the deleted listing was not chosen")
	}

	var names []string
	for _, item := range got.objects.Items() {
		names = append(names, item.(objectItem).key)
	}

	if !slices.Contains(names, "gone.txt") {
		t.Errorf("the deleted key is missing: %v", names)
	}
	if !slices.Contains(names, "file.txt") {
		t.Errorf("a current key got lost: %v", names)
	}
	if !slices.Contains(names, "logs/") {
		t.Errorf("prefix navigation broke: %v", names)
	}

	view := model.View()
	for _, want := range []string{"[with deleted]", "gone.txt", "[deleted]"} {
		if !strings.Contains(view, want) {
			t.Errorf("view misses %q:\n%s", want, view)
		}
	}

	// the switch survives walking into a prefix
	inner := got
	inner.objects.Select(slices.Index(names, "logs/"))
	model = enter(t, tea.Model(inner))

	got = model.(Model)
	if !got.showDeleted {
		t.Error("the switch was lost when descending")
	}

	names = names[:0]
	for _, item := range got.objects.Items() {
		names = append(names, item.(objectItem).key)
	}
	if !slices.Contains(names, "logs/vanished.log") {
		t.Errorf("the deleted key below the prefix is missing: %v", names)
	}

	// and back to the ordinary listing through the overview
	model = step(t, model, tea.KeyMsg{Type: tea.KeyEsc}) // out of logs/
	model = step(t, model, tea.KeyMsg{Type: tea.KeyEsc}) // to the overview
	model = openSection(t, model, menuObjects)

	if model.(Model).showDeleted {
		t.Error("the ordinary listing still includes deleted keys")
	}
}

func TestDeletedKeyRefusesActions(t *testing.T) {
	server := fakeS3(t)
	model := openSection(t, openBucketMenu(t, server), menuDeleted)

	names := []string{}
	for _, item := range model.(Model).objects.Items() {
		names = append(names, item.(objectItem).key)
	}

	inner := model.(Model)
	inner.objects.Select(slices.Index(names, "gone.txt"))
	model = tea.Model(inner)

	for _, key := range []rune{'l', 'c', 'm', 'd'} {
		after := press(t, model, key)

		got := after.(Model)
		if got.prompt != nil || got.pending != nil || got.view != viewObjects {
			t.Errorf("%q acted on a deleted key", key)
		}
		if !strings.Contains(got.status, "is deleted") {
			t.Errorf("%q: status = %q", key, got.status)
		}
	}

	// its versions are reachable, that is where the delete marker can go
	model = enter(t, model)
	if got := model.(Model).view; got != viewVersions {
		t.Errorf("view = %v, want the versions of the deleted key", got)
	}
}

func TestDeletedFallsBackWhenDenied(t *testing.T) {
	server := fakeS3(t)
	model := newTestModel(t, server.URL)

	inner := model.(Model)
	inner.buckets.Select(1) // bucket-b denies ListObjectVersions
	model = openSection(t, enter(t, tea.Model(inner)), menuDeleted)

	got := model.(Model)
	if got.showDeleted {
		t.Error("the switch stayed on although the listing was denied")
	}
	if len(got.objects.Items()) == 0 {
		t.Error("the bucket looks empty instead of falling back")
	}
	if !strings.Contains(got.status, "s3:ListBucketVersions") {
		t.Errorf("status = %q, want the missing permission", got.status)
	}
	if got.err != nil {
		t.Errorf("the fallback must not be an error: %v", got.err)
	}
}

func TestBucketOverviewIsTheWayIn(t *testing.T) {
	server := fakeS3(t)
	model := openBucketMenu(t, server)

	got := model.(Model)
	if got.view != viewBucketMenu {
		t.Fatalf("view = %v, opening a bucket should show its overview", got.view)
	}
	if want := 4 + len(configEntries(false)); len(got.menu.Items()) != want {
		t.Fatalf("got %d entries, want %d", len(got.menu.Items()), want)
	}
	if !strings.Contains(model.View(), "bucket-a") {
		t.Errorf("the breadcrumb does not name the bucket:\n%s", model.View())
	}

	// the keys that used to reach these views are gone
	for _, key := range []rune{'i', 'p', 'o'} {
		after := press(t, openBucket(t, server), key)

		if got := after.(Model).view; got != viewObjects {
			t.Errorf("%q still jumps somewhere: view = %v", key, got)
		}
	}

	// every entry leads somewhere
	for _, test := range []struct {
		kind menuKind
		want viewState
	}{
		{menuObjects, viewObjects},
		{menuDeleted, viewObjects},
		{menuMultipart, viewMultipart},
	} {
		opened := openSection(t, openBucketMenu(t, server), test.kind)

		if got := opened.(Model).view; got != test.want {
			t.Errorf("entry %d opened view %v, want %v", test.kind, got, test.want)
		}
		if test.kind == menuDeleted && !opened.(Model).showDeleted {
			t.Error("the deleted entry did not switch the listing")
		}

		// and back to the overview
		back := step(t, opened, tea.KeyMsg{Type: tea.KeyEsc})
		if got := back.(Model).view; got != viewBucketMenu {
			t.Errorf("esc from %v went to %v, want the overview", test.want, got)
		}
	}

	// and so does every configuration of the bucket
	for kind, spec := range configSpecs {
		if spec.object {
			continue
		}

		opened := openConfigSection(t, openBucketMenu(t, server), configKind(kind))

		got := opened.(Model)
		if got.view != viewConfig || got.configKind != configKind(kind) {
			t.Errorf("the %s entry opened %v/%v", configKind(kind).spec().entry, got.view, got.configKind)
		}

		back := step(t, opened, tea.KeyMsg{Type: tea.KeyEsc})
		if got := back.(Model).view; got != viewBucketMenu {
			t.Errorf("esc from %s went to %v, want the overview", configKind(kind).spec().entry, got)
		}
	}
}

func TestProfileListIsTheRoot(t *testing.T) {
	server := fakeS3(t)
	model := openBucket(t, server)

	for range 3 {
		model = step(t, model, tea.KeyMsg{Type: tea.KeyEsc}) // objects, overview, buckets
	}

	model = step(t, model, tea.KeyMsg{Type: tea.KeyEsc})
	if got := model.(Model).view; got != viewProfiles {
		t.Fatalf("view = %v, want the profile list", got)
	}

	// esc must not fall back into the profile that is already active
	for range 3 {
		model = step(t, model, tea.KeyMsg{Type: tea.KeyEsc})

		if got := model.(Model).view; got != viewProfiles {
			t.Fatalf("esc left the profile list towards %v", got)
		}
	}

	// picking one leads on
	model = enter(t, model)
	if got := model.(Model).view; got != viewBuckets {
		t.Errorf("view = %v, want buckets", got)
	}
}

func TestEnterWalksIntoTheDetails(t *testing.T) {
	server := fakeS3(t)
	model := openBucket(t, server)

	inner := model.(Model)
	inner.objects.Select(1) // file.txt
	model = enter(t, tea.Model(inner))

	if got := model.(Model).view; got != viewVersions {
		t.Fatalf("view = %v, want the versions", got)
	}

	// enter on a version shows exactly that version
	inner = model.(Model)
	inner.versions.Select(1) // v1
	model = openDetails(t, enter(t, tea.Model(inner)))

	got := model.(Model)
	if got.view != viewConfig || got.configKind != configObjectMetadata {
		t.Fatalf("view = %v, want the details", got.view)
	}
	if got.objectVersion != "v1" {
		t.Errorf("objectVersion = %q, want v1", got.objectVersion)
	}

	// esc walks back the same way
	for _, want := range []viewState{viewObjectMenu, viewVersions, viewObjects} {
		model = step(t, model, tea.KeyMsg{Type: tea.KeyEsc})

		if got := model.(Model).view; got != want {
			t.Fatalf("view = %v, want %v", got, want)
		}
	}
}

func TestDeleteMarkerHasNoDetails(t *testing.T) {
	server := fakeS3(t)
	model := openBucket(t, server)

	inner := model.(Model)
	inner.objects.Select(1) // file.txt
	model = enter(t, tea.Model(inner))

	inner = model.(Model)
	inner.versions.Select(2) // the delete marker
	model = enter(t, tea.Model(inner))

	got := model.(Model)
	if got.view != viewVersions {
		t.Errorf("view = %v, a delete marker has nothing to show", got.view)
	}
	if !strings.Contains(got.status, "delete marker has no metadata") {
		t.Errorf("status = %q", got.status)
	}
}

// configTargetOf names the store entry of a request: the bucket, or one
// object in it.
func configTargetOf(bucket, key string) string {
	if key == "" {
		return bucket
	}

	return bucket + "/" + key
}

// configResourceName picks the bucket or object level document a request
// addresses.
func configResourceName(query url.Values) string {
	for _, name := range []string{
		"policy", "lifecycle", "acl", "publicAccessBlock", "cors", "versioning",
		"object-lock", "legal-hold", "retention", "tagging",
	} {
		if query.Has(name) {
			return name
		}
	}

	return ""
}

// serveConfig answers a GET of one bucket or object level document.
func (f *fakeServer) serveConfig(w http.ResponseWriter, name, target string) {
	resource := f.resources[name]

	document, ok := f.config(name, target)
	if !ok {
		if resource.code != "" {
			w.WriteHeader(http.StatusNotFound)
			fmt.Fprintf(w, `<Error><Code>%s</Code><Message>not configured</Message></Error>`, resource.code)

			return
		}

		document = resource.fallback
	}

	if resource.json {
		w.Header().Set("Content-Type", "application/json")
	}

	fmt.Fprint(w, document)
}

// The documents bucket-a starts with.
const (
	defaultPolicy = `{"Version":"2012-10-17","Statement":[{"Sid":"PublicRead","Effect":"Allow",` +
		`"Principal":"*","Action":["s3:GetObject"],"Resource":"arn:aws:s3:::bucket-a/*"}]}`

	defaultACL = `<?xml version="1.0" encoding="UTF-8"?>
<AccessControlPolicy><Owner><ID>owner-id</ID><DisplayName>simon</DisplayName></Owner>
<AccessControlList><Grant>
<Grantee xmlns:xsi="http://www.w3.org/2001/XMLSchema-instance" xsi:type="CanonicalUser"><ID>owner-id</ID><DisplayName>simon</DisplayName></Grantee>
<Permission>FULL_CONTROL</Permission>
</Grant></AccessControlList></AccessControlPolicy>`

	defaultPublicAccess = `<?xml version="1.0" encoding="UTF-8"?>
<PublicAccessBlockConfiguration><BlockPublicAcls>true</BlockPublicAcls><IgnorePublicAcls>true</IgnorePublicAcls>
<BlockPublicPolicy>false</BlockPublicPolicy><RestrictPublicBuckets>false</RestrictPublicBuckets>
</PublicAccessBlockConfiguration>`

	defaultCORS = `<?xml version="1.0" encoding="UTF-8"?>
<CORSConfiguration><CORSRule><ID>web</ID>
<AllowedMethod>GET</AllowedMethod><AllowedMethod>HEAD</AllowedMethod>
<AllowedOrigin>https://example.com</AllowedOrigin><MaxAgeSeconds>3000</MaxAgeSeconds>
</CORSRule></CORSConfiguration>`

	defaultVersioning = `<?xml version="1.0" encoding="UTF-8"?>
<VersioningConfiguration><Status>Enabled</Status></VersioningConfiguration>`

	defaultTags = `<?xml version="1.0" encoding="UTF-8"?>
<Tagging><TagSet>
<Tag><Key>env</Key><Value>prod</Value></Tag>
<Tag><Key>owner</Key><Value>platform</Value></Tag>
</TagSet></Tagging>`

	defaultLegalHold = `<?xml version="1.0" encoding="UTF-8"?>
<LegalHold><Status>ON</Status></LegalHold>`

	defaultRetention = `<?xml version="1.0" encoding="UTF-8"?>
<Retention><Mode>GOVERNANCE</Mode><RetainUntilDate>2030-01-02T03:04:05Z</RetainUntilDate></Retention>`

	defaultObjectLock = `<?xml version="1.0" encoding="UTF-8"?>
<ObjectLockConfiguration><ObjectLockEnabled>Enabled</ObjectLockEnabled>
<Rule><DefaultRetention><Mode>GOVERNANCE</Mode><Days>30</Days></DefaultRetention></Rule>
</ObjectLockConfiguration>`
)

// defaultLifecycle is what bucket-a starts with.
const defaultLifecycle = `<?xml version="1.0" encoding="UTF-8"?>
<LifecycleConfiguration>
<Rule><ID>expire-logs</ID><Status>Enabled</Status>
<Filter><Prefix>logs/</Prefix></Filter>
<Expiration><Days>30</Days></Expiration>
<NoncurrentVersionExpiration><NoncurrentDays>7</NoncurrentDays><NewerNoncurrentVersions>3</NewerNoncurrentVersions></NoncurrentVersionExpiration>
<AbortIncompleteMultipartUpload><DaysAfterInitiation>7</DaysAfterInitiation></AbortIncompleteMultipartUpload>
</Rule>
<Rule><ID>archive-raw</ID><Status>Disabled</Status>
<Filter><And><Prefix>raw/</Prefix><Tag><Key>class</Key><Value>cold</Value></Tag></And></Filter>
<Transition><Days>90</Days><StorageClass>GLACIER</StorageClass></Transition>
</Rule>
</LifecycleConfiguration>`
