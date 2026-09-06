package ui

import (
	"context"
	"fmt"
	"path"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/s3"
	"github.com/aws/aws-sdk-go-v2/service/s3/types"
	"github.com/charmbracelet/bubbles/textinput"
	tea "github.com/charmbracelet/bubbletea"

	"github.com/sj14/s3tui/internal/awsclient"
)

// deleteBatchSize is the maximum number of keys per DeleteObjects request.
const deleteBatchSize = 1000

// startDelete removes an object, a single version, every version of a key or
// a whole prefix.
func startDelete(parent context.Context, client *s3.Client, ref copyRef) (*transfer, error) {
	if ref.key == "" {
		return nil, fmt.Errorf("nothing selected")
	}

	ctx, cancel := context.WithCancel(parent)

	transfer := &transfer{
		kind:   transferDelete,
		label:  ref.String(),
		events: make(chan tea.Msg, 16),
		cancel: cancel,
		total:  1,
	}

	go runDelete(ctx, client, transfer, ref)

	return transfer, nil
}

func runDelete(ctx context.Context, client *s3.Client, transfer *transfer, ref copyRef) {
	defer close(transfer.events)

	var (
		keys []types.ObjectIdentifier
		noun = "objects"
	)

	switch {
	case ref.allVersions:
		noun = "versions"

		versions, err := listAllVersions(ctx, client, ref.bucket, ref.key, ref.recursive)
		if err != nil {
			transfer.events <- transferDoneMsg{err: transferErr(ctx, err)}

			return
		}

		keys = versions

	case ref.recursive:
		objects, err := listAllObjects(ctx, client, ref.bucket, ref.key)
		if err != nil {
			transfer.events <- transferDoneMsg{err: transferErr(ctx, err)}

			return
		}

		for _, object := range objects {
			keys = append(keys, types.ObjectIdentifier{Key: object.Key})
		}

	default:
		identifier := types.ObjectIdentifier{Key: aws.String(ref.key)}
		if ref.versionID != "" {
			identifier.VersionId = aws.String(ref.versionID)
		}

		keys = append(keys, identifier)
	}

	if len(keys) == 0 {
		transfer.events <- transferDoneMsg{summary: "nothing to delete"}

		return
	}

	report := &reporter{events: transfer.events, ctx: ctx, total: len(keys)}

	// A single key does not need the batch API, which some endpoints lack.
	if len(keys) == 1 {
		report.startFile(aws.ToString(keys[0].Key), 0)

		_, err := client.DeleteObject(ctx, &s3.DeleteObjectInput{
			Bucket:    aws.String(ref.bucket),
			Key:       keys[0].Key,
			VersionId: keys[0].VersionId,
		})
		if err != nil {
			transfer.events <- transferDoneMsg{err: transferErr(ctx, err), summary: deleteSummary(0, 1, noun)}

			return
		}

		report.finishFile()

		transfer.events <- transferDoneMsg{err: transferErr(ctx, nil), summary: deleteSummary(1, 1, noun)}

		return
	}

	for start := 0; start < len(keys); start += deleteBatchSize {
		if ctx.Err() != nil {
			break
		}

		batch := keys[start:min(start+deleteBatchSize, len(keys))]

		report.startFile(aws.ToString(batch[0].Key), 0)

		resp, err := client.DeleteObjects(ctx, &s3.DeleteObjectsInput{
			Bucket: aws.String(ref.bucket),
			Delete: &types.Delete{Objects: batch, Quiet: aws.Bool(true)},
		})
		if err != nil {
			transfer.events <- transferDoneMsg{
				err:     transferErr(ctx, err),
				summary: deleteSummary(report.done, len(keys), noun),
			}

			return
		}

		if len(resp.Errors) > 0 {
			first := resp.Errors[0]

			transfer.events <- transferDoneMsg{
				err: fmt.Errorf("deleting %q: %s (%d of %d keys failed)",
					aws.ToString(first.Key), aws.ToString(first.Message), len(resp.Errors), len(batch)),
				summary: deleteSummary(report.done+len(batch)-len(resp.Errors), len(keys), noun),
			}

			return
		}

		report.finishFiles(len(batch))
	}

	transfer.events <- transferDoneMsg{
		err:     transferErr(ctx, nil),
		summary: deleteSummary(report.done, len(keys), noun),
	}
}

func deleteSummary(done, total int, noun string) string {
	return fmt.Sprintf("deleted %d/%d %s", done, total, noun)
}

// listAllVersions collects every version and every delete marker of one key,
// or of everything below a prefix. This is what makes a key really disappear:
// a delete without a version id only adds another delete marker on top.
func listAllVersions(ctx context.Context, client *s3.Client, bucket, key string, recursive bool) ([]types.ObjectIdentifier, error) {
	var versions []types.ObjectIdentifier

	paginator := s3.NewListObjectVersionsPaginator(client, &s3.ListObjectVersionsInput{
		Bucket:  aws.String(bucket),
		Prefix:  aws.String(key),
		MaxKeys: aws.Int32(pageSize),
	})

	for paginator.HasMorePages() {
		resp, err := paginator.NextPage(ctx)
		if err != nil {
			return nil, err
		}

		for _, version := range resp.Versions {
			if !recursive && aws.ToString(version.Key) != key {
				continue // a key the prefix only happens to start with
			}

			versions = append(versions, types.ObjectIdentifier{Key: version.Key, VersionId: version.VersionId})
		}

		for _, marker := range resp.DeleteMarkers {
			if !recursive && aws.ToString(marker.Key) != key {
				continue
			}

			versions = append(versions, types.ObjectIdentifier{Key: marker.Key, VersionId: marker.VersionId})
		}
	}

	return versions, nil
}

// abortMultipartCmd cancels an unfinished multipart upload and frees its parts.
func abortMultipartCmd(ctx context.Context, client *awsclient.Client, bucket, key, uploadID string) tea.Cmd {
	return func() tea.Msg {
		s3Client, err := client.ForBucket(ctx, bucket)
		if err != nil {
			return fail("resolving bucket region", err)
		}

		_, err = s3Client.AbortMultipartUpload(ctx, &s3.AbortMultipartUploadInput{
			Bucket:   aws.String(bucket),
			Key:      aws.String(key),
			UploadId: aws.String(uploadID),
		})
		if err != nil {
			return fail(fmt.Sprintf("aborting the multipart upload of %q", key), err)
		}

		return multipartAbortedMsg{bucket: bucket, key: key, uploadID: uploadID}
	}
}

type multipartAbortedMsg struct {
	bucket   string
	key      string
	uploadID string
}

// askDelete asks before removing the selection: an object, a prefix, a single
// version or an unfinished multipart upload.
func (m Model) askDelete() (tea.Model, tea.Cmd) {
	if m.profileCfg.ReadOnly {
		m.status = "profile is read-only, delete blocked"

		return m, nil
	}

	switch m.view {
	case viewObjects:
		selected, note, ok := m.selectedObject("delete")
		if !ok {
			m.status = note

			return m, nil
		}

		return m.askDeletePrefix(selected.key)

	case viewVersions:
		selected, ok := m.versions.SelectedItem().(versionItem)
		if !ok {
			return m, nil
		}

		ref := copyRef{bucket: m.bucket, key: m.key, versionID: selected.versionID}

		what := "version"
		if selected.deleteMarker {
			what = "delete marker"
		}

		m.pending = &pendingConfirm{
			question: fmt.Sprintf("delete %s %s of %s?", what, short(selected.versionID, 16), path.Base(m.key)),
			action:   func(model Model) (tea.Model, tea.Cmd) { return model.startDeleteOf(ref) },
		}

	case viewMultipart:
		selected, ok := m.multipart.SelectedItem().(multipartItem)
		if !ok {
			return m, nil
		}

		bucket, key, uploadID := m.bucket, selected.key, selected.uploadID

		m.pending = &pendingConfirm{
			question: fmt.Sprintf("abort multipart upload %s of %s?", short(uploadID, 16), key),
			action: func(model Model) (tea.Model, tea.Cmd) {
				model.inFlight++
				model.status = ""

				return model, abortMultipartCmd(model.ctx, model.client, bucket, key, uploadID)
			},
		}

	case viewConfig:
		target, kind := m.target, m.configKind

		spec := kind.spec()
		if spec.remove == nil {
			m.status = "the " + spec.name + " cannot be deleted, only changed"

			return m, nil
		}

		m.pending = &pendingConfirm{
			question: fmt.Sprintf("delete the %s of %s?", spec.name, target.name()),
			action: func(model Model) (tea.Model, tea.Cmd) {
				model.inFlight++
				model.status = ""

				return model, deleteConfigCmd(model.ctx, model.client, target, kind)
			},
		}

	default:
		m.status = "select an object, a version or a multipart upload to delete"
	}

	return m, nil
}

// askDeletePrefix asks for a prefix to delete recursively. The selected row
// pre-fills it, but it remains editable so a prefix need not be visible in the
// page currently loaded by the object list.
func (m Model) askDeletePrefix(prefix string) (tea.Model, tea.Cmd) {
	if m.profileCfg.ReadOnly {
		m.status = "profile is read-only, delete blocked"

		return m, nil
	}

	m.prompt = newPrompt(promptDeletePrefix,
		fmt.Sprintf("delete every object below, in s3://%s", m.bucket), prefix, m.width)

	return m, textinput.Blink
}

// askDeleteVersions asks before a hard delete: every version and every delete
// marker of a key, or of everything below a prefix. Unlike d this cannot be
// taken back – there is no delete marker left to remove afterwards.
func (m Model) askDeleteVersions() (tea.Model, tea.Cmd) {
	if m.profileCfg.ReadOnly {
		m.status = "profile is read-only, delete blocked"

		return m, nil
	}

	var (
		ref      copyRef
		question string
	)

	switch m.view {
	case viewObjects:
		selected, ok := m.objects.SelectedItem().(objectItem)
		if !ok {
			return m, nil
		}

		// A key whose newest version is a delete marker is refused everywhere
		// else, but it is exactly what this is for: the versions are still
		// there and this is the way to get rid of them.
		ref = copyRef{bucket: m.bucket, key: selected.key, recursive: selected.isPrefix, allVersions: true}

		question = "delete " + ref.String() + " and every version of it? (irreversible)"
		if selected.isPrefix {
			question = "delete every version below " + ref.String() + "? (recursive, irreversible)"
		}

	case viewVersions:
		ref = copyRef{bucket: m.bucket, key: m.key, allVersions: true}

		question = fmt.Sprintf("delete %s and all %d versions of it? (irreversible)",
			path.Base(m.key), len(m.versions.Items()))

	default:
		m.status = "select an object or a prefix to delete every version of"

		return m, nil
	}

	m.pending = &pendingConfirm{
		question: question,
		action:   func(model Model) (tea.Model, tea.Cmd) { return model.startDeleteOf(ref) },
	}

	return m, nil
}

// startDeleteOf starts the delete transfer of a confirmed selection, the way
// startUploadOf starts an upload.
func (m Model) startDeleteOf(ref copyRef) (tea.Model, tea.Cmd) {
	client, err := m.client.ForBucket(m.ctx, ref.bucket)
	if err != nil {
		m.err = fmt.Errorf("resolving bucket region: %w", err)

		return m, nil
	}

	transfer, err := startDelete(m.ctx, client, ref)
	if err != nil {
		m.err = fmt.Errorf("delete: %w", err)

		return m, nil
	}

	return m, m.track(transfer)
}
