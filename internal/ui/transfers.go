package ui

import (
	"fmt"
	"strings"

	"github.com/charmbracelet/bubbles/list"
	tea "github.com/charmbracelet/bubbletea"
)

// Every transfer runs on its own, so the one line in the footer can only ever
// be a summary. This view is where they are all visible, and where one of them
// is stopped.

const (
	transferKindWidth = 11 // "↓ download " and its friends
	transferBarWidth  = 10
	transferStatWidth = 30
)

// arrow marks what a transfer does to the data.
func (k transferKind) arrow() string {
	switch k {
	case transferDownload:
		return "↓"
	case transferUpload:
		return "↑"
	case transferDelete:
		return "✕"
	default:
		return "»"
	}
}

// transferItem is one row of the transfer view. It holds the transfer itself,
// so a row renders the current numbers without rebuilding the list.
type transferItem struct {
	entry *transfer
}

func (i transferItem) FilterValue() string { return i.entry.label }

func (i transferItem) row(width int, selected bool) string {
	entry := i.entry

	name := cell(entry.kind.arrow()+" "+entry.kind.String(), transferKindWidth, false) + entry.label

	return renderRow(width, selected, false, name, "", cell(entry.status(), transferStatWidth, false))
}

// describe names the transfer for the status line: the label alone says what
// is moved, not what is done to it.
func (t *transfer) describe() string { return t.kind.String() + " " + t.label }

// status is the right hand side of a row: the progress while it runs, what
// became of it once it ended.
func (t *transfer) status() string {
	switch {
	case t.canceled:
		return "canceled · " + t.summary
	case t.err != nil:
		return "failed · " + t.err.Error()
	case t.finished:
		if t.summary == "" {
			return "done"
		}

		return t.summary
	}

	return fmt.Sprintf("%s %s %s",
		textBar(t.ratio(), transferBarWidth),
		cell(fmt.Sprintf("%.0f%%", t.ratio()*100), 4, true),
		cell(fmt.Sprintf("%d/%d", t.done, t.total), 9, true),
	)
}

// textBar draws a progress bar out of block characters, so a row does not need
// the progress model of the footer.
func textBar(ratio float64, width int) string {
	filled := int(min(1, max(0, ratio)) * float64(width))

	return strings.Repeat("█", filled) + strings.Repeat("░", width-filled)
}

// transferItems lists the transfers, the newest one first: that is the one
// somebody just started and wants to see.
func transferItems(transfers []*transfer) []list.Item {
	items := make([]list.Item, 0, len(transfers))

	for i := len(transfers) - 1; i >= 0; i-- {
		items = append(items, transferItem{entry: transfers[i]})
	}

	return items
}

// running are the transfers which are still going.
func (m Model) running() []*transfer {
	var running []*transfer

	for _, entry := range m.transfers {
		if !entry.finished {
			running = append(running, entry)
		}
	}

	return running
}

func (m Model) transferByID(id int) *transfer {
	for _, entry := range m.transfers {
		if entry.id == id {
			return entry
		}
	}

	return nil
}

// track hands the started transfer an id, adds it to the list and starts
// waiting for its events.
func (m *Model) track(entry *transfer) tea.Cmd {
	m.transferID++
	entry.id = m.transferID
	m.transfers = append(m.transfers, entry)
	m.transferList.SetItems(transferItems(m.transfers))
	m.err = nil
	m.status = "started " + entry.describe() + " · t shows the transfers"

	return waitForTransfer(entry)
}

// openTransfers shows every transfer of this session, running or ended.
func (m Model) openTransfers() (tea.Model, tea.Cmd) {
	if m.view == viewTransfers {
		return m, nil
	}

	m.transferFrom = m.view
	m.view = viewTransfers
	m.err, m.status = nil, ""
	m.transferList.SetItems(transferItems(m.transfers))
	m.transferList.ResetFilter()

	return m, nil
}

// cancelTransfer asks before stopping the selected transfer, or drops it from
// the list when it has already ended.
func (m Model) cancelTransfer() (tea.Model, tea.Cmd) {
	item, ok := m.transferList.SelectedItem().(transferItem)
	if !ok {
		return m, nil
	}

	entry := item.entry

	if entry.finished {
		// nothing is interrupted here, the row is only taken off the list
		m.transfers = withoutTransfer(m.transfers, entry.id)
		m.transferList.SetItems(transferItems(m.transfers))
		m.status = "cleared " + entry.describe()

		return m, nil
	}

	// there is no undo for this, an interrupted transfer starts over
	m.pending = &pendingConfirm{
		question: "cancel the " + entry.describe() + "?",
		action: func(model Model) (tea.Model, tea.Cmd) {
			entry.cancel()
			model.status = "canceling " + entry.describe() + "…"

			return model, nil
		},
	}

	return m, nil
}

func withoutTransfer(transfers []*transfer, id int) []*transfer {
	kept := make([]*transfer, 0, len(transfers))

	for _, entry := range transfers {
		if entry.id != id {
			kept = append(kept, entry)
		}
	}

	return kept
}

// handleTransfer applies one event of one transfer.
func (m Model) handleTransfer(msg transferMsg) (tea.Model, tea.Cmd) {
	entry := m.transferByID(msg.id)
	if entry == nil || entry.finished {
		return m, nil
	}

	switch event := msg.msg.(type) {
	case transferProgressMsg:
		entry.done, entry.total = event.done, event.total
		entry.current = event.current
		entry.curDone, entry.curTotal = event.curDone, event.curTotal

		return m, waitForTransfer(entry)

	case transferAskMsg:
		// the wait is resumed once the question is answered
		m.confirms = append(m.confirms, transferAsk{id: msg.id, path: event.path, reply: event.reply})

		return m, nil

	case transferDoneMsg:
		return m.finishTransfer(entry, event)
	}

	return m, nil
}
