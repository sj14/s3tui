package ui

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"strings"

	"github.com/charmbracelet/bubbles/list"
	tea "github.com/charmbracelet/bubbletea"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/s3"
	"github.com/aws/smithy-go"
	"github.com/sj14/s3tui/internal/awsclient"
)

const pageSize = 1000

type errMsg struct {
	what string
	err  error
}

type bucketsMsg struct {
	items []list.Item
}

type objectsMsg struct {
	bucket string
	prefix string
	search string
	items  []list.Item
	next   *pageToken
	more   bool   // appended to the existing page
	note   string // why the listing is not the one that was asked for
}

// pageToken continues a listing. ListObjectsV2 and ListObjectVersions page
// differently, so it carries whichever markers belong to the current mode.
type pageToken struct {
	continuation  *string
	keyMarker     *string
	versionMarker *string
}

type versionsMsg struct {
	bucket string
	key    string
	items  []list.Item
}

type versioningMsg struct {
	bucket string
	status string
}

func fail(what string, err error) tea.Msg {
	return errMsg{what: what, err: err}
}

func listBucketsCmd(ctx context.Context, client *awsclient.Client) tea.Cmd {
	return func() tea.Msg {
		var (
			items []list.Item
			token *string
		)

		for {
			resp, err := client.Base().ListBuckets(ctx, &s3.ListBucketsInput{ContinuationToken: token})
			if err != nil {
				return fail("listing buckets", err)
			}

			for _, bucket := range resp.Buckets {
				item := bucketItem{
					name:   aws.ToString(bucket.Name),
					region: aws.ToString(bucket.BucketRegion),
				}
				if bucket.CreationDate != nil {
					item.created = *bucket.CreationDate
				}

				client.SetBucketRegion(item.name, item.region)
				items = append(items, item)
			}

			token = resp.ContinuationToken
			if aws.ToString(token) == "" {
				break
			}
		}

		return bucketsMsg{items: items}
	}
}

// listObjectsCmd lists one page of bucket/prefix. search narrows the listing
// on the server: only keys starting with prefix+search come back, while the
// names are still shown relative to prefix. With deleted set, the page comes
// from ListObjectVersions so that keys whose newest version is a delete marker
// show up at all.
func listObjectsCmd(ctx context.Context, client *awsclient.Client, bucket, prefix, search string, deleted bool, token *pageToken) tea.Cmd {
	return func() tea.Msg {
		s3Client, err := client.ForBucket(ctx, bucket)
		if err != nil {
			return fail("resolving bucket region", err)
		}

		if deleted {
			msg, err := listWithDeleted(ctx, s3Client, bucket, prefix, search, token)
			if err == nil {
				return msg
			}

			// Not every setup may list versions, do not leave the user with an
			// empty bucket in that case.
			note := describeMissing(err, "no versions")
			if note == "no versions" {
				note = err.Error()
			}

			msg, listErr := listCurrent(ctx, s3Client, bucket, prefix, search, nil)
			if listErr != nil {
				return fail(fmt.Sprintf("listing objects of %q", bucket), listErr)
			}

			msg.note = "deleted keys need s3:ListBucketVersions: " + note

			return msg
		}

		msg, err := listCurrent(ctx, s3Client, bucket, prefix, search, token)
		if err != nil {
			return fail(fmt.Sprintf("listing objects of %q", bucket), err)
		}

		return msg
	}
}

// listCurrent lists the objects that are currently there, the ordinary way.
func listCurrent(ctx context.Context, client *s3.Client, bucket, prefix, search string, token *pageToken) (objectsMsg, error) {
	input := &s3.ListObjectsV2Input{
		Bucket:    aws.String(bucket),
		Prefix:    aws.String(prefix + search),
		Delimiter: aws.String("/"),
		MaxKeys:   aws.Int32(pageSize),
	}

	if token != nil {
		input.ContinuationToken = token.continuation
	}

	resp, err := client.ListObjectsV2(ctx, input)
	if err != nil {
		return objectsMsg{}, err
	}

	items := make([]list.Item, 0, len(resp.CommonPrefixes)+len(resp.Contents))

	for _, commonPrefix := range resp.CommonPrefixes {
		key := aws.ToString(commonPrefix.Prefix)
		items = append(items, objectItem{
			key:      key,
			name:     trimPrefix(key, prefix),
			isPrefix: true,
			size:     -1,
		})
	}

	for _, object := range resp.Contents {
		key := aws.ToString(object.Key)
		if key == prefix {
			continue // the "folder" placeholder object itself
		}

		item := objectItem{
			key:   key,
			name:  trimPrefix(key, prefix),
			size:  aws.ToInt64(object.Size),
			class: string(object.StorageClass),
		}
		if object.LastModified != nil {
			item.modified = *object.LastModified
		}

		items = append(items, item)
	}

	msg := objectsMsg{bucket: bucket, prefix: prefix, search: search, items: items, more: token != nil}

	if resp.NextContinuationToken != nil {
		msg.next = &pageToken{continuation: resp.NextContinuationToken}
	}

	return msg, nil
}

// listWithDeleted builds the same listing from ListObjectVersions, reduced to
// the newest version of every key, so that deleted keys are visible too.
func listWithDeleted(ctx context.Context, client *s3.Client, bucket, prefix, search string, token *pageToken) (objectsMsg, error) {
	input := &s3.ListObjectVersionsInput{
		Bucket:    aws.String(bucket),
		Prefix:    aws.String(prefix + search),
		Delimiter: aws.String("/"),
		MaxKeys:   aws.Int32(pageSize),
	}

	if token != nil {
		input.KeyMarker, input.VersionIdMarker = token.keyMarker, token.versionMarker
	}

	resp, err := client.ListObjectVersions(ctx, input)
	if err != nil {
		return objectsMsg{}, err
	}

	items := make([]list.Item, 0, len(resp.CommonPrefixes)+len(resp.Versions))

	for _, commonPrefix := range resp.CommonPrefixes {
		key := aws.ToString(commonPrefix.Prefix)
		items = append(items, objectItem{
			key:      key,
			name:     trimPrefix(key, prefix),
			isPrefix: true,
			size:     -1,
		})
	}

	for _, version := range resp.Versions {
		if !aws.ToBool(version.IsLatest) {
			continue // only the newest version of a key is a listing entry
		}

		key := aws.ToString(version.Key)
		if key == prefix {
			continue
		}

		item := objectItem{
			key:   key,
			name:  trimPrefix(key, prefix),
			size:  aws.ToInt64(version.Size),
			class: string(version.StorageClass),
		}
		if version.LastModified != nil {
			item.modified = *version.LastModified
		}

		items = append(items, item)
	}

	for _, marker := range resp.DeleteMarkers {
		if !aws.ToBool(marker.IsLatest) {
			continue
		}

		key := aws.ToString(marker.Key)
		if key == prefix {
			continue
		}

		item := objectItem{
			key:     key,
			name:    trimPrefix(key, prefix),
			deleted: true,
			size:    -1,
		}
		if marker.LastModified != nil {
			item.modified = *marker.LastModified
		}

		items = append(items, item)
	}

	sortObjectItems(items)

	msg := objectsMsg{bucket: bucket, prefix: prefix, search: search, items: items, more: token != nil}

	if aws.ToBool(resp.IsTruncated) {
		msg.next = &pageToken{keyMarker: resp.NextKeyMarker, versionMarker: resp.NextVersionIdMarker}
	}

	return msg, nil
}

// sortObjectItems restores the order S3 lists in: prefixes first, then keys.
func sortObjectItems(items []list.Item) {
	sort.SliceStable(items, func(a, b int) bool {
		first, second := items[a].(objectItem), items[b].(objectItem)

		if first.isPrefix != second.isPrefix {
			return first.isPrefix
		}

		return first.key < second.key
	})
}

func listVersionsCmd(ctx context.Context, client *awsclient.Client, bucket, key string) tea.Cmd {
	return func() tea.Msg {
		s3Client, err := client.ForBucket(ctx, bucket)
		if err != nil {
			return fail("resolving bucket region", err)
		}

		var (
			items     []list.Item
			passedKey bool
		)

		// The prefix is the key itself, so the listing starts at it and the
		// first entry of another key ends the walk.
		paginator := s3.NewListObjectVersionsPaginator(s3Client, &s3.ListObjectVersionsInput{
			Bucket:  aws.String(bucket),
			Prefix:  aws.String(key),
			MaxKeys: aws.Int32(pageSize),
		})

		for paginator.HasMorePages() && !passedKey {
			resp, err := paginator.NextPage(ctx)
			if err != nil {
				return fail(fmt.Sprintf("listing versions of %q", key), err)
			}

			for _, version := range resp.Versions {
				if aws.ToString(version.Key) != key {
					passedKey = true
					continue
				}

				item := versionItem{
					versionID: aws.ToString(version.VersionId),
					latest:    aws.ToBool(version.IsLatest),
					size:      aws.ToInt64(version.Size),
					class:     string(version.StorageClass),
				}
				if version.LastModified != nil {
					item.modified = *version.LastModified
				}

				items = append(items, item)
			}

			for _, marker := range resp.DeleteMarkers {
				if aws.ToString(marker.Key) != key {
					passedKey = true
					continue
				}

				item := versionItem{
					versionID:    aws.ToString(marker.VersionId),
					latest:       aws.ToBool(marker.IsLatest),
					deleteMarker: true,
					size:         -1,
				}
				if marker.LastModified != nil {
					item.modified = *marker.LastModified
				}

				items = append(items, item)
			}
		}

		return versionsMsg{bucket: bucket, key: key, items: items}
	}
}

func bucketVersioningCmd(ctx context.Context, client *awsclient.Client, bucket string) tea.Cmd {
	return func() tea.Msg {
		s3Client, err := client.ForBucket(ctx, bucket)
		if err != nil {
			return fail("resolving bucket region", err)
		}

		resp, err := s3Client.GetBucketVersioning(ctx, &s3.GetBucketVersioningInput{Bucket: aws.String(bucket)})
		if err != nil {
			// Not every endpoint or policy allows this, don't treat it as fatal.
			return versioningMsg{bucket: bucket, status: versioningUnknown}
		}

		status := string(resp.Status) // "Enabled", "Suspended" or empty
		if status == "" {
			status = versioningDisabled
		}

		return versioningMsg{bucket: bucket, status: status}
	}
}

func trimPrefix(key, prefix string) string {
	if len(key) > len(prefix) && key[:len(prefix)] == prefix {
		return key[len(prefix):]
	}

	return key
}

type multipartMsg struct {
	bucket string
	prefix string
	items  []list.Item
}

type partsMsg struct {
	bucket   string
	key      string
	uploadID string
	items    []list.Item
}

// listMultipartCmd lists the multipart uploads which were started but neither
// completed nor aborted.
func listMultipartCmd(ctx context.Context, client *awsclient.Client, bucket, prefix string) tea.Cmd {
	return func() tea.Msg {
		s3Client, err := client.ForBucket(ctx, bucket)
		if err != nil {
			return fail("resolving bucket region", err)
		}

		var items []list.Item

		paginator := s3.NewListMultipartUploadsPaginator(s3Client, &s3.ListMultipartUploadsInput{
			Bucket:     aws.String(bucket),
			Prefix:     aws.String(prefix),
			MaxUploads: aws.Int32(pageSize),
		})

		for paginator.HasMorePages() {
			resp, err := paginator.NextPage(ctx)
			if err != nil {
				return fail(fmt.Sprintf("listing multipart uploads of %q", bucket), err)
			}

			for _, upload := range resp.Uploads {
				item := multipartItem{
					key:      aws.ToString(upload.Key),
					uploadID: aws.ToString(upload.UploadId),
					class:    string(upload.StorageClass),
				}
				if upload.Initiated != nil {
					item.initiated = *upload.Initiated
				}

				items = append(items, item)
			}
		}

		return multipartMsg{bucket: bucket, prefix: prefix, items: items}
	}
}

// listPartsCmd lists the parts which were already uploaded.
func listPartsCmd(ctx context.Context, client *awsclient.Client, bucket, key, uploadID string) tea.Cmd {
	return func() tea.Msg {
		s3Client, err := client.ForBucket(ctx, bucket)
		if err != nil {
			return fail("resolving bucket region", err)
		}

		var items []list.Item

		paginator := s3.NewListPartsPaginator(s3Client, &s3.ListPartsInput{
			Bucket:   aws.String(bucket),
			Key:      aws.String(key),
			UploadId: aws.String(uploadID),
			MaxParts: aws.Int32(pageSize),
		})

		for paginator.HasMorePages() {
			resp, err := paginator.NextPage(ctx)
			if err != nil {
				return fail(fmt.Sprintf("listing parts of %q", key), err)
			}

			for _, part := range resp.Parts {
				item := partItem{
					number: aws.ToInt32(part.PartNumber),
					etag:   aws.ToString(part.ETag),
					size:   aws.ToInt64(part.Size),
				}
				if part.LastModified != nil {
					item.modified = *part.LastModified
				}

				items = append(items, item)
			}
		}

		return partsMsg{bucket: bucket, key: key, uploadID: uploadID, items: items}
	}
}

type configMsg struct {
	target   configTarget
	kind     configKind
	document string
	note     string
}

// configCmd fetches one of the documents a bucket or an object is configured
// with.
func configCmd(ctx context.Context, client *awsclient.Client, target configTarget, kind configKind) tea.Cmd {
	return func() tea.Msg {
		spec := kind.spec()

		s3Client, err := client.ForBucket(ctx, target.bucket)
		if err != nil {
			return fail("resolving bucket region", err)
		}

		document, err := spec.get(ctx, s3Client, target)
		if err != nil {
			return configMsg{target: target, kind: kind, note: describeMissing(err, spec.missing)}
		}

		if strings.TrimSpace(document) == "" {
			return configMsg{target: target, kind: kind, note: spec.missing}
		}

		return configMsg{target: target, kind: kind, document: document}
	}
}

// describeMissing turns "nothing configured" into a note and keeps every other
// error readable, so one missing setting does not break the whole view.
func describeMissing(err error, missing string) string {
	if apiErr, ok := errors.AsType[smithy.APIError](err); ok {
		switch apiErr.ErrorCode() {
		case "NoSuchBucketPolicy", "NoSuchLifecycleConfiguration", "NoSuchConfiguration",
			"NoSuchCORSConfiguration", "NoSuchPublicAccessBlockConfiguration",
			"ObjectLockConfigurationNotFoundError", "NoSuchObjectLockConfiguration":
			return missing
		case "AccessDenied":
			return "access denied"
		case "NotImplemented", "MethodNotAllowed":
			return "not supported by this endpoint"
		}

		return apiErr.ErrorCode()
	}

	return err.Error()
}
