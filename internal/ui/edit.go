package ui

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path"
	"strings"

	tea "github.com/charmbracelet/bubbletea"

	"github.com/sj14/s3tui/internal/awsclient"
)

// editorHook starts the editor. In production bubbletea suspends the UI and
// hands the terminal over; the tests replace the hook and run their own.
var editorHook = tea.ExecProcess

// editorClosedMsg arrives once the editor has quit.
type editorClosedMsg struct {
	target   configTarget
	kind     configKind
	path     string
	original string
	err      error
}

// configSavedMsg reports what happened to a written or deleted document.
type configSavedMsg struct {
	target  configTarget
	kind    configKind
	note    string
	changed bool // the target now holds something else than before
	err     error
}

// editConfig opens the shown document in $EDITOR and saves it afterwards.
func (m Model) editConfig() (tea.Model, tea.Cmd) {
	spec := m.configKind.spec()
	if spec.put == nil {
		m.status = "the " + spec.name + " cannot be edited, S3 only reports it"

		return m, nil
	}

	if m.profileCfg.ReadOnly {
		m.status = "profile is read-only, editing blocked"

		return m, nil
	}

	if m.inFlight > 0 {
		m.status = "still loading, try again in a moment"

		return m, nil
	}

	editor, err := resolveEditor()
	if err != nil {
		m.err = err

		return m, nil
	}

	document := m.configDocument()

	path, err := writeTemp(m.target, m.configKind, document)
	if err != nil {
		m.err = fmt.Errorf("preparing the editor: %w", err)

		return m, nil
	}

	closed := editorClosedMsg{target: m.target, kind: m.configKind, path: path, original: document}
	m.err, m.status = nil, ""

	command := exec.CommandContext(m.ctx, editor[0], append(editor[1:], path)...)

	return m, editorHook(command, func(err error) tea.Msg {
		closed.err = err

		return closed
	})
}

// configDocument is what the editor starts with: the stored document, or a
// template when the bucket has none.
func (m Model) configDocument() string {
	if strings.TrimSpace(m.configText) == "" {
		return m.configKind.spec().template
	}

	return strings.TrimRight(m.configText, "\n") + "\n"
}

// resolveEditor picks the editor of the user, or a fallback which every
// system has.
func resolveEditor() ([]string, error) {
	for _, name := range []string{"VISUAL", "EDITOR"} {
		if value := strings.TrimSpace(os.Getenv(name)); value != "" {
			return strings.Fields(value), nil
		}
	}

	for _, fallback := range []string{"vi", "nano", "vim"} {
		if path, err := exec.LookPath(fallback); err == nil {
			return []string{path}, nil
		}
	}

	return nil, errors.New("no editor found, set $EDITOR")
}

// writeTemp puts the document into a file the editor can open. The name says
// what is edited, some editors pick their syntax mode from the suffix.
func writeTemp(target configTarget, kind configKind, document string) (string, error) {
	name := path.Base(target.name())

	file, err := os.CreateTemp("", fmt.Sprintf("s3tui-%s-%s-*.json", kind.spec().short, name))
	if err != nil {
		return "", err
	}

	if _, err := file.WriteString(document); err != nil {
		file.Close()
		os.Remove(file.Name())

		return "", err
	}

	if err := file.Close(); err != nil {
		os.Remove(file.Name())

		return "", err
	}

	return file.Name(), nil
}

// saveConfigCmd writes back what the editor left behind.
func saveConfigCmd(ctx context.Context, client *awsclient.Client, edit editorClosedMsg) tea.Cmd {
	return func() tea.Msg {
		spec := edit.kind.spec()
		result := configSavedMsg{target: edit.target, kind: edit.kind}

		defer os.Remove(edit.path)

		raw, err := os.ReadFile(edit.path)
		if err != nil {
			result.err = fmt.Errorf("reading back the editor file: %w", err)

			return result
		}

		document := strings.TrimSpace(string(raw))
		if document == strings.TrimSpace(edit.original) {
			result.note = "unchanged, nothing written"

			return result
		}

		if document == "" {
			result.err = emptyDocument(spec)

			return result
		}

		s3Client, err := client.ForBucket(ctx, edit.target.bucket)
		if err != nil {
			result.err = fmt.Errorf("resolving bucket region: %w", err)

			return result
		}

		if err := spec.put(ctx, s3Client, edit.target, document); err != nil {
			result.err = describeWrite(err, "saving the "+spec.name)

			return result
		}

		result.note, result.changed = spec.name+" saved", true

		return result
	}
}

// emptyDocument explains what an emptied file means: not a delete, that is a
// key of its own wherever the API has such a call.
func emptyDocument(spec configSpec) error {
	if spec.remove != nil {
		return invalid("the %s is empty, d deletes it", spec.name)
	}

	return invalid("the %s is empty, there is nothing to save", spec.name)
}

// describeWrite keeps a complaint about the document as it is – nothing was
// sent – and puts everything else below what was attempted.
func describeWrite(err error, what string) error {
	var typo documentError
	if errors.As(err, &typo) {
		return typo
	}

	return fmt.Errorf("%s: %w", what, err)
}

// deleteConfigCmd takes the configuration off the bucket.
func deleteConfigCmd(ctx context.Context, client *awsclient.Client, target configTarget, kind configKind) tea.Cmd {
	return func() tea.Msg {
		spec := kind.spec()
		result := configSavedMsg{target: target, kind: kind, changed: true}

		if spec.remove == nil {
			result.err = invalid("the %s cannot be deleted, only changed", spec.name)

			return result
		}

		s3Client, err := client.ForBucket(ctx, target.bucket)
		if err != nil {
			result.err = fmt.Errorf("resolving bucket region: %w", err)

			return result
		}

		if err := spec.remove(ctx, s3Client, target); err != nil {
			result.err = fmt.Errorf("deleting the %s: %w", spec.name, err)

			return result
		}

		result.note = spec.name + " deleted"

		return result
	}
}
