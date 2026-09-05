# AGENTS.md

Notes for agents (and humans) working on s3tui.

## What this is

A terminal UI for S3 built on the AWS SDK for Go v2 and the Bubble Tea
ecosystem. The config format is deliberately identical to
[sss](https://github.com/sj14/sss) so an existing sss config works unchanged.

## Layout

| Path                 | Contains                                                  |
| -------------------- | --------------------------------------------------------- |
| `main.go`            | flags, config loading, program start                       |
| `internal/config`    | the TOML config (sss format) and the env overrides         |
| `internal/awsclient` | builds the `*s3.Client` per profile, region routing        |
| `internal/ui`        | the whole bubbletea application                            |

Inside `internal/ui`:

| File          | Contains                                                    |
| ------------- | ----------------------------------------------------------- |
| `model.go`    | the root `Model`, key handling, views, layout, rendering     |
| `items.go`    | the list items, the row renderer and the bucket overview      |
| `commands.go` | the read-only `tea.Cmd`s (listings) and their messages       |
| `keys.go`     | the key bindings and the per view help                       |
| `transfer.go` | upload and download, the file browser keys, the transfer machinery |
| `copy.go`     | server-side copy and move, and the prompt that asks for a target |
| `delete.go`   | delete, batch delete, abort multipart, and their questions   |
| `stats.go`    | the bucket scan, what it adds up and how it is rendered      |
| `transfers.go` | the list of running and finished transfers and its view     |
| `random.go`   | put-rand: objects of random data, without a file on disk     |
| `config.go`   | the table of bucket and object configurations, its target and the JSON helpers |
| `edit.go`     | editing a configuration in `$EDITOR` and writing it back      |
| `popup.go`    | the error box and the overlay that draws it over the view    |
| `picker.go`   | the local file browser used to pick an upload                |

## Conventions

* All state lives in `Model`; every bit of I/O happens in a `tea.Cmd` that
  returns a message. Never block in `Update`.
* Long running work (transfers) runs in a goroutine, reports through a channel
  of `tea.Msg` and is pumped by `waitForTransfer`.
* Listings compare bucket/prefix/key of the response against the current state
  before applying it, so a stale response of a view we already left is dropped.
* The model runs without a client until a profile is picked; guard `m.client`
  before using it.
* The upload browser in `picker.go` is a plain `list.Model` over `os.ReadDir`,
  not `bubbles/filepicker`: that component renders its own rows from an
  unexported slice, so a selectable ".." entry is not possible with it. There
  is a discussion about a filepicker with a folder-up entry at
  https://github.com/charmbracelet/bubbles/discussions/739 – worth a look if
  that ever lands upstream, it would make `picker.go` redundant.
* Destructive actions confirm through `pendingConfirm` and are refused when the
  profile has `read_only = true`. Editing a bucket configuration is refused
  there too.
* Every action key lives in `keys.go` and is handled in `handleKey` before the
  event reaches the list: `d` deletes, `l` downloads (load), `e` edits, `u`
  uploads, `c`/`m` copy and move. `d`, `l` and `u` shadow the paging bindings
  `bubbles/list` brings along; paging stays on `pgup`/`pgdn`. `e` only does
  something in a configuration view, elsewhere it falls through.
* Run `gofmt -l -w .`, `go vet ./...` and `go test ./...` before finishing.

## Tests

`internal/ui/ui_test.go` starts a `fakeServer`, a small httptest based S3
endpoint that speaks the operations the UI uses (listings, ranged GET, PUT,
copy, delete, multipart, and every bucket configuration). The tests drive the
real `Model` through `step`/`runCmd`, which execute the returned commands
synchronously.

`editorHook` is the seam for the editor: production hands the terminal to
`tea.ExecProcess`, the tests replace the hook with a function that edits the
file in place (`withEditor` in `config_test.go`) and never starts a
process. The `fakeServer` keeps every bucket level document per bucket in
`resources`, so a write can be read back; a resource either answers with an
error code when the bucket has none (`NoSuchBucketPolicy`) or with a fallback
where S3 always has an answer (an ACL, the versioning state).

Two harness quirks worth knowing:

* messages from `cursor.` (the prompt cursor blink) would loop forever, so
  `runCmd` drops them and prompts are opened with `pressNoRun`,
* several keystrokes sent in one write arrive as a single multi rune `KeyMsg`,
  which matches no binding. Send keys one at a time.

Colors are off in tests (no TTY), so `View()` contains plain text. In a real
terminal the same text is interleaved with escape sequences — do not assert
across a styled boundary such as `"x delete"`.

## TODO

Roughly in the order they seem worth doing.

### Presigned URL

A key on an object that creates a time limited link through
`s3.NewPresignClient`. Probably the most common reason to open an S3 tool at
all. Open question is the expiry: a prompt with a default of one hour.

## Design notes

### Why the error box is built while rendering

`m.err` is set in about twenty places. Instead of teaching each of them about
the box, `View` builds it from `m.err` whenever that is not nil, and
`preparePopup` sizes the viewport and fills it. Only the scroll offset lives in
the model, so the box needs no opening and no closing state.

`overlay` splices the box into the rendered screen line by line with
`ansi.Truncate`/`ansi.TruncateLeft`, which keeps the escape sequences of the
covered lines intact. Note that `lipgloss.Style.Width` counts the padding: the
box asks for `text width + 2`, otherwise the already wrapped text is wrapped a
second time and the box grows past the screen.

### Why the configurations are one table

`config.go` holds a `configSpec` per document – name, template, and a `get`,
`put` and `remove` closure – and everything else is generic: one `viewConfig`,
one `configCmd`, one `renderConfig`, one editor path, one delete path. Both
overviews are built from the same table (`configEntries` splits it by the
`object` flag), so a new configuration is a new entry there and nothing else.
A `nil` remove is the honest way to say that the API has no delete call: the
footer drops `d` and `askDelete` says so.

What a document belongs to is a `configTarget` – a bucket, or one object or
version in it. That is why the closures take a target and not a bucket name,
and why nothing about an object needs machinery of its own: its metadata, its
tags, its ACL and its legal hold are four more rows.

A `nil` put is the counterpart of a `nil` remove: `HeadObject` has no way back
in, so the metadata row has no put, `e` says so and the footer drops it.

The documents are the SDK structs marshalled to JSON, with two exceptions,
both because the SDK shape would be worse to read and to edit than what S3
actually means: the policy is already JSON at S3 and is passed through, and a
tag set is rendered as a flat object instead of a list of Key/Value pairs.

The documents are the SDK structs marshalled to JSON, and are parsed back into
the very same struct with `DisallowUnknownFields`. That is what makes the
round trip trustworthy – what the view shows is exactly what the editor gets
and what `Put…` receives. The policy is the exception: at S3 it already is a
JSON document, so it is passed through and only checked with `json.Valid`.

### Why the editor is external

Policy and lifecycle are edited by writing the document to a temp file and
handing the terminal to `$VISUAL`/`$EDITOR`. A `bubbles/textarea` was the
alternative and was rejected: it has no syntax highlighting, no bracket
matching and no indent handling, which is most of what editing JSON needs, and
it would have to fight the list bindings for every key. The external editor is
a few dozen lines and gives the user the tooling they already have.

Only two things happen locally before the write: the document is compared with
what the editor started from (unchanged means nothing is sent), and it is
parsed. The policy is only checked for being JSON – which statements are valid
is S3's judgement, not ours. The lifecycle has to be parsed anyway, since the
API takes rules and not a document, and `DisallowUnknownFields` turns a typo
into an error instead of a silently dropped rule.

An emptied document is refused rather than treated as a delete: deleting is on
`d`, like everywhere else, and asks first.

### Why an object has an overview

`enter` on a version used to open one page with the metadata and the tags on
it. There are four documents now, each with its own call, so it opens the same
kind of menu a bucket does and every document is one entry of it – there is no
object view left, only `viewConfig`. `back` from an object `viewConfig` always
lands on `viewObjectMenu`, and only the menu itself remembers where it came
from (`objectFrom`, which is `viewObjects` on an unversioned bucket and
`viewVersions` otherwise).

### Why every transfer runs on its own

There used to be one `Model.transfer` and a `transferBusy` guard which refused
every upload, download, copy, move, delete and even an editor while it was set.
That is gone: `Model.transfers` is a slice, everything can run at the same
time, and nothing refuses anything because something else is busy.

What that costs is an id. All transfers share one update loop, so
`waitForTransfer` wraps each event in a `transferMsg{id, msg}` and `Update`
looks the transfer up by it – the producers in `transfer.go`, `copy.go` and
`delete.go` did not have to change for that. The overwrite question became a
queue (`Model.confirms`) for the same reason: two downloads can ask at once, so
they are answered oldest first and the answer goes back to the channel of the
transfer which asked.

Quitting had to learn about them too. `q` is gone – `ctrl+c` is the only way
out. It is not in the footer, only in the full help, where it is the first
column so that a narrow terminal cannot truncate the one key nobody may miss.
It is handled at the top of `handleKey` so it works from inside a
prompt, a question or the error box. With transfers running the first press
only arms `Model.quitArmed` and warns; a second one within `quitWindow` leaves,
and a `tea.Tick` clears the window afterwards. The warning lives in
`contextLine` rather than in `m.status`, so it cannot eat the summary of a
transfer that finishes in the meantime.

A transfer that succeeded takes itself off the list in `finishTransfer`; its
summary is in the status line and there is nothing left to look at. What failed
or was canceled stays, because that is the part somebody still has to read.

`esc` no longer cancels. It was the only key with two meanings depending on
state, and with several transfers it could not have picked one anyway. `x` in
the transfer view cancels the selected transfer instead, and clears the row of
a failed or canceled one – the same gesture: get this off my list. It is not `d`,
although aborting a multipart upload one view over is the same kind of thing:
`d` deletes in S3, and a footer reading "delete" over a running download invites
exactly the wrong guess about what it deletes. Canceling asks first, since the
transfer starts over from the beginning; clearing a finished row does not.

The rows hold a `*transfer`, not a copy of its numbers, so progress needs no
`SetItems`: only starting, finishing and clearing rebuild the list.

Two things decide whether a download bar moves at all. `ratio()` only counts
bytes when `curTotal` is set, so `runDownload` asks `HeadObject` for the size of
a single file – the recursive case gets it from the listing, and without it the
bar sat at zero until the last byte. And the manager writes a whole part in one
`WriteAt`, which is the only moment the counter can advance: `downloadPartSize`
is therefore 1 MiB rather than the SDK default of 5, with the concurrency
raised to keep a comparable amount of data in flight.

### Why the random upload sits in the file browser

`put-rand` fills a bucket with objects that never existed on disk, which is how
a test of an endpoint starts. It is reached with `r` from the file browser
because that is already the "where does an upload come from" screen – only here
the answer is "from nowhere", and the browser listing is beside the point, so
the prompt takes over and the view goes back to the objects.

Size and count are one prompt, not two: the line is parsed from the right, so
the last word is the count when it is a number ("1 MiB 10") and the whole line
is the size when it is not ("4 KiB"). `randomReader` streams the noise into the
uploader instead of building it, so a run of 10 × 5 GiB costs no memory.

### Why the hard delete is a second key

`d` on a versioned bucket writes a delete marker, which is a *creation*: it
hides the key and keeps everything. Deleting for real means listing the key's
versions and deleting each one by id. Those are two different operations with
two different outcomes, so they are two keys – `d` and `D` – instead of one key
with a three-way question. A key never changes its meaning here, and the one
that cannot be undone should not be the one people press by reflex.

Both end in the same `startDelete`; `copyRef.allVersions` selects
`listAllVersions` (`ListObjectVersions` over the key, or over the prefix when
`recursive` is set) instead of a single identifier, and the batch delete is the
same one the recursive delete uses. `D` is also the only action which accepts
an object item with `deleted` set: `selectedObject` refuses those everywhere
else because there is no current version to work on, which is exactly the state
`D` is meant to clean up.

### Why the statistics are a paged scan

There is no API that reports what a bucket holds, so the *statistics* entry is
the listing itself, added up. That could have been one command that loops until
the bucket ends, but such a command is invisible while it runs and cannot be
stopped: a bucket with a million versions would sit behind a spinner for
minutes.

Instead every page is one `tea.Cmd` and one `statsPageMsg` which carries the
numbers of *that page only*. `Update` merges it into `Model.stats` and starts
the next page, so the screen grows with every request, the scan lives entirely
in the normal message loop (no goroutine, no channel), and `esc` stops it by
clearing `running` – the page which is already on its way is merged nowhere,
its message is dropped by the `run` counter of the scan.

A page carries a fresh `bucketStats` rather than the accumulated one on
purpose: the model owns its maps and slices, and nothing a command returns is
ever mutated by two goroutines. `run` is incremented per scan so that the pages
of a scan the user left cannot land in the numbers of the next one.

The scan reads `ListObjectVersions` without a delimiter, which is the only way
to see non-current versions and delete markers at all, and falls back to
`ListObjectsV2` with a note where that is denied – the same fallback the
deleted-keys switch uses. The rows that only the version listing can fill are
then left out instead of being shown as zero.

### Why deleted keys are a switch and not the default

The object listing normally comes from `ListObjectsV2`, which hides keys whose
newest version is a delete marker. The *objects with delete markers* entry of
the bucket overview rebuilds the same listing from `ListObjectVersions`,
reduced to `IsLatest` per key. That is a choice rather than the default because
it costs:

1. **another permission** – `ListObjectsV2` needs `s3:ListBucket`,
   `ListObjectVersions` needs `s3:ListBucketVersions`. Setups that browse fine
   today would get AccessDenied, which is why a denied versions listing falls
   back to `ListObjectsV2` with a note instead of showing an empty bucket,
2. **many more requests** – a page holds 1000 *versions*, not 1000 keys. With
   20 versions per object, enumerating 10.000 keys takes ~200 requests instead
   of 10,
3. **compatibility** – S3 compatible gateways implement `ListObjectsV2`
   universally, `ListObjectVersions` less so.

A key's versions can also span a page boundary, so the incremental loading
merges by key (`mergeObjects`) instead of appending.

## Deliberately not planned

* **A limit on how many transfers run at once.** They are started by hand, one
  key at a time; a queue with a worker count would be machinery for a problem
  nobody has hit.
* **Sorting the object list by size or date.** It could only sort the pages
  that happen to be loaded, which is the same half truth the list filter had
  before `/` started asking the server.
* **A bucket size in the bucket list.** The *statistics* entry counts what a
  bucket holds when it is asked to; doing that for every bucket of a listing
  would be millions of requests for a number CloudWatch already has.
