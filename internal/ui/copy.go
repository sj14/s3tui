package ui

import (
	"context"
	"fmt"
	"net/url"
	"strings"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/s3"
	"github.com/aws/aws-sdk-go-v2/service/s3/types"
	"github.com/charmbracelet/bubbles/textinput"
	tea "github.com/charmbracelet/bubbletea"
)

const (
	// CopyObject is limited to 5 GiB, bigger objects are copied part by part.
	maxCopyObjectSize = 5 * 1024 * 1024 * 1024
	copyPartSize      = 512 * 1024 * 1024
)

// copyRef points at an object, a version or a whole prefix.
type copyRef struct {
	bucket      string
	key         string
	versionID   string
	recursive   bool
	allVersions bool // delete: every version of the key or of the prefix
	size        int64
}

// parseTarget reads "s3://bucket/key" or a plain key of the current bucket.
func parseTarget(input, currentBucket string) (copyRef, error) {
	input = strings.TrimSpace(input)
	if input == "" {
		return copyRef{}, fmt.Errorf("no target given")
	}

	if !strings.HasPrefix(input, "s3://") {
		return copyRef{bucket: currentBucket, key: strings.TrimPrefix(input, "/")}, nil
	}

	rest := strings.TrimPrefix(input, "s3://")

	bucket, key, found := strings.Cut(rest, "/")
	if !found || bucket == "" {
		return copyRef{}, fmt.Errorf("target %q has no key", input)
	}

	return copyRef{bucket: bucket, key: key}, nil
}

// String renders the reference as an s3 URL.
func (r copyRef) String() string {
	return "s3://" + r.bucket + "/" + r.key
}

// copySource is the URL encoded value of the CopySource header.
func copySource(bucket, key, versionID string) string {
	source := (&url.URL{Path: "/" + bucket + "/" + key}).EscapedPath()

	if versionID != "" {
		source += "?versionId=" + url.QueryEscape(versionID)
	}

	return source
}

// startCopy copies or moves an object, a version or a prefix inside S3. The
// data never passes through the client.
func startCopy(parent context.Context, client *s3.Client, kind transferKind, src, dst copyRef) (*transfer, error) {
	if src.recursive {
		if !strings.HasSuffix(dst.key, "/") && dst.key != "" {
			dst.key += "/"
		}

		if dst.bucket == src.bucket && strings.HasPrefix(dst.key, src.key) {
			return nil, fmt.Errorf("the target is inside the source prefix")
		}
	}

	if dst.bucket == src.bucket && dst.key == src.key && src.versionID == "" {
		return nil, fmt.Errorf("source and target are identical")
	}

	if dst.key == "" || strings.HasSuffix(dst.key, "/") && !src.recursive {
		return nil, fmt.Errorf("the target needs an object name")
	}

	ctx, cancel := context.WithCancel(parent)

	transfer := &transfer{
		kind:   kind,
		label:  src.String() + " → " + dst.String(),
		events: make(chan tea.Msg, 16),
		cancel: cancel,
		total:  1,
	}

	go runCopy(ctx, client, transfer, kind, src, dst)

	return transfer, nil
}

type copyPair struct {
	srcKey string
	dstKey string
	size   int64
}

func runCopy(ctx context.Context, client *s3.Client, transfer *transfer, kind transferKind, src, dst copyRef) {
	defer close(transfer.events)

	var pairs []copyPair

	if src.recursive {
		objects, err := listAllObjects(ctx, client, src.bucket, src.key)
		if err != nil {
			transfer.events <- transferDoneMsg{err: transferErr(ctx, err)}

			return
		}

		for _, object := range objects {
			key := aws.ToString(object.Key)

			pairs = append(pairs, copyPair{
				srcKey: key,
				dstKey: dst.key + strings.TrimPrefix(key, src.key),
				size:   aws.ToInt64(object.Size),
			})
		}

		if len(pairs) == 0 {
			transfer.events <- transferDoneMsg{summary: "nothing to " + kind.String()}

			return
		}
	} else {
		pairs = append(pairs, copyPair{srcKey: src.key, dstKey: dst.key, size: src.size})
	}

	report := &reporter{events: transfer.events, ctx: ctx, total: len(pairs)}
	verb := "copied"
	if kind == transferMove {
		verb = "moved"
	}

	var done int64

	for _, pair := range pairs {
		if ctx.Err() != nil {
			break
		}

		report.startFile(pair.dstKey, pair.size)

		err := copyOne(ctx, client, src.bucket, pair.srcKey, src.versionID, dst.bucket, pair.dstKey, pair.size, report)
		if err != nil {
			transfer.events <- transferDoneMsg{
				err:     transferErr(ctx, err),
				summary: summary(verb, report.done, len(pairs), done),
			}

			return
		}

		if kind == transferMove {
			input := &s3.DeleteObjectInput{Bucket: aws.String(src.bucket), Key: aws.String(pair.srcKey)}
			if src.versionID != "" {
				input.VersionId = aws.String(src.versionID)
			}

			if _, err := client.DeleteObject(ctx, input); err != nil {
				transfer.events <- transferDoneMsg{
					err:     fmt.Errorf("deleting the source %q: %w", pair.srcKey, transferErr(ctx, err)),
					summary: summary(verb, report.done, len(pairs), done),
				}

				return
			}
		}

		done += pair.size
		report.finishFile()
	}

	transfer.events <- transferDoneMsg{
		err:     transferErr(ctx, nil),
		summary: summary(verb, report.done, len(pairs), done),
	}
}

// copyOne copies a single object, in parts when it is too big for CopyObject.
func copyOne(ctx context.Context, client *s3.Client, srcBucket, srcKey, versionID, dstBucket, dstKey string, size int64, report *reporter) error {
	source := copySource(srcBucket, srcKey, versionID)

	if size <= maxCopyObjectSize {
		_, err := client.CopyObject(ctx, &s3.CopyObjectInput{
			Bucket:     aws.String(dstBucket),
			Key:        aws.String(dstKey),
			CopySource: aws.String(source),
		})
		if err != nil {
			return fmt.Errorf("copying %q: %w", srcKey, err)
		}

		report.add(size)

		return nil
	}

	return copyMultipart(ctx, client, source, srcKey, dstBucket, dstKey, size, report)
}

// copyMultipart copies an object bigger than 5 GiB with UploadPartCopy.
func copyMultipart(ctx context.Context, client *s3.Client, source, srcKey, dstBucket, dstKey string, size int64, report *reporter) error {
	created, err := client.CreateMultipartUpload(ctx, &s3.CreateMultipartUploadInput{
		Bucket: aws.String(dstBucket),
		Key:    aws.String(dstKey),
	})
	if err != nil {
		return fmt.Errorf("starting the multipart copy of %q: %w", srcKey, err)
	}

	abort := func() {
		client.AbortMultipartUpload(context.WithoutCancel(ctx), &s3.AbortMultipartUploadInput{
			Bucket:   aws.String(dstBucket),
			Key:      aws.String(dstKey),
			UploadId: created.UploadId,
		})
	}

	var (
		parts  []types.CompletedPart
		number int32 = 1
	)

	for offset := int64(0); offset < size; offset += copyPartSize {
		end := min(offset+copyPartSize, size) - 1

		part, err := client.UploadPartCopy(ctx, &s3.UploadPartCopyInput{
			Bucket:          aws.String(dstBucket),
			Key:             aws.String(dstKey),
			UploadId:        created.UploadId,
			CopySource:      aws.String(source),
			CopySourceRange: aws.String(fmt.Sprintf("bytes=%d-%d", offset, end)),
			PartNumber:      aws.Int32(number),
		})
		if err != nil {
			abort()

			return fmt.Errorf("copying part %d of %q: %w", number, srcKey, err)
		}

		parts = append(parts, types.CompletedPart{
			ETag:       part.CopyPartResult.ETag,
			PartNumber: aws.Int32(number),
		})

		report.add(end - offset + 1)
		number++
	}

	_, err = client.CompleteMultipartUpload(ctx, &s3.CompleteMultipartUploadInput{
		Bucket:          aws.String(dstBucket),
		Key:             aws.String(dstKey),
		UploadId:        created.UploadId,
		MultipartUpload: &types.CompletedMultipartUpload{Parts: parts},
	})
	if err != nil {
		abort()

		return fmt.Errorf("completing the multipart copy of %q: %w", srcKey, err)
	}

	return nil
}

// askCopy opens the prompt for the target of a server side copy or move.
func (m Model) askCopy(kind transferKind) (tea.Model, tea.Cmd) {
	if m.profileCfg.ReadOnly {
		m.status = fmt.Sprintf("profile is read-only, %s blocked", kind)

		return m, nil
	}

	prompt := &promptState{bucket: m.bucket}
	if kind == transferMove {
		prompt.kind = promptMove
	} else {
		prompt.kind = promptCopy
	}

	switch m.view {
	case viewObjects:
		selected, note, ok := m.selectedObject(kind.String())
		if !ok {
			m.status = note

			return m, nil
		}

		prompt.key = selected.key
		prompt.recursive = selected.isPrefix
		prompt.size = max(0, selected.size)

	case viewVersions:
		if kind == transferMove {
			m.status = "a single version can be copied, but not moved"

			return m, nil
		}

		selected, ok := m.versions.SelectedItem().(versionItem)
		if !ok {
			return m, nil
		}

		if selected.deleteMarker {
			m.status = "a delete marker has no content to copy"

			return m, nil
		}

		prompt.key = m.key
		prompt.versionID = selected.versionID
		prompt.size = max(0, selected.size)

	default:
		m.status = fmt.Sprintf("select an object or a prefix to %s", kind)

		return m, nil
	}

	prompt.title = fmt.Sprintf("%s s3://%s/%s to", kind, m.bucket, prompt.key)
	m.prompt = m.fillPrompt(prompt, "s3://"+m.bucket+"/"+prompt.key)

	return m, textinput.Blink
}
