package ui

import (
	"context"
	"encoding/binary"
	"fmt"
	"io"
	"math"
	"math/rand/v2"
	"strconv"
	"strings"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/feature/s3/manager"
	"github.com/aws/aws-sdk-go-v2/service/s3"
	"github.com/charmbracelet/bubbles/textinput"
	tea "github.com/charmbracelet/bubbletea"
	"github.com/dustin/go-humanize"
)

// Filling a bucket with objects that do not exist on disk is what a test of an
// endpoint starts with: how fast is it, what does the listing do with 10.000
// keys, does the lifecycle rule fire. This is the put-rand of sss.

// randomSpec is what the prompt asks for: how big and how many.
type randomSpec struct {
	size  int64
	count int
}

func (s randomSpec) String() string {
	return fmt.Sprintf("%d × %s", s.count, humanize.IBytes(uint64(max(0, s.size))))
}

// parseRandomSpec reads "10 MiB 100": the count is the last word, everything
// in front of it is the size. A size on its own means one object.
func parseRandomSpec(input string) (randomSpec, error) {
	fields := strings.Fields(input)
	if len(fields) == 0 {
		return randomSpec{}, fmt.Errorf("say how big and how many, e.g. %q", "1 MiB 10")
	}

	spec := randomSpec{count: 1}

	// "4 KiB" ends in a word, "4 KiB 10" in a number: only the number is a
	// count, the rest of the line is the size.
	if count, err := strconv.Atoi(fields[len(fields)-1]); err == nil && len(fields) > 1 {
		if count < 1 {
			return randomSpec{}, fmt.Errorf("a count of %d uploads nothing", count)
		}

		spec.count = count
		fields = fields[:len(fields)-1]
	}

	size := strings.Join(fields, " ")

	bytes, err := humanize.ParseBytes(size)
	if err != nil {
		return randomSpec{}, fmt.Errorf("%q is not a size", size)
	}

	if bytes > math.MaxInt64 {
		return randomSpec{}, fmt.Errorf("%q is not a size anybody has", size)
	}

	spec.size = int64(bytes)

	return spec, nil
}

// randomReader hands out size bytes of noise. Nothing is held in memory: the
// uploader reads it while it sends, so the size of an object is not the size
// of the process.
type randomReader struct {
	left int64
	src  *rand.Rand
}

func newRandomReader(size int64) *randomReader {
	return &randomReader{left: size, src: rand.New(rand.NewPCG(rand.Uint64(), rand.Uint64()))}
}

func (r *randomReader) Read(p []byte) (int, error) {
	if r.left <= 0 {
		return 0, io.EOF
	}

	if int64(len(p)) > r.left {
		p = p[:r.left]
	}

	var word [8]byte

	for i := 0; i < len(p); i += len(word) {
		binary.LittleEndian.PutUint64(word[:], r.src.Uint64())
		copy(p[i:], word[:])
	}

	r.left -= int64(len(p))

	return len(p), nil
}

// startRandomUpload puts count objects of random data into bucket/prefix.
func startRandomUpload(parent context.Context, client *s3.Client, bucket, prefix string, spec randomSpec) (*transfer, error) {
	if spec.count < 1 {
		return nil, fmt.Errorf("nothing to upload")
	}

	ctx, cancel := context.WithCancel(parent)

	transfer := &transfer{
		kind:   transferUpload,
		label:  fmt.Sprintf("%s of random data → s3://%s/%s", spec, bucket, prefix),
		events: make(chan tea.Msg, 16),
		cancel: cancel,
		total:  spec.count,
	}

	go runRandomUpload(ctx, client, transfer, bucket, prefix, spec)

	return transfer, nil
}

func runRandomUpload(ctx context.Context, client *s3.Client, transfer *transfer, bucket, prefix string, spec randomSpec) {
	defer close(transfer.events)

	report := &reporter{events: transfer.events, ctx: ctx, total: spec.count}
	uploader := manager.NewUploader(client)

	// one run, one set of keys: sorted the way they were written
	stamp := time.Now().Format("20060102-150405")
	digits := len(strconv.Itoa(spec.count))

	var uploaded int64

	for i := 1; i <= spec.count; i++ {
		if ctx.Err() != nil {
			break
		}

		key := fmt.Sprintf("%srand-%s-%0*d.bin", prefix, stamp, digits, i)

		report.startFile(key, spec.size)

		_, err := uploader.Upload(ctx, &s3.PutObjectInput{
			Bucket: aws.String(bucket),
			Key:    aws.String(key),
			Body:   &countingReader{reader: newRandomReader(spec.size), reporter: report},
		})
		if err != nil {
			transfer.events <- transferDoneMsg{
				err:     transferErr(ctx, err),
				summary: summary("uploaded", report.done, spec.count, uploaded),
			}

			return
		}

		uploaded += spec.size
		report.finishFile()
	}

	transfer.events <- transferDoneMsg{
		err:     transferErr(ctx, nil),
		summary: summary("uploaded", report.done, spec.count, uploaded),
	}
}

// askRandomUpload asks how much random data should go into the current prefix.
func (m Model) askRandomUpload() (tea.Model, tea.Cmd) {
	if m.profileCfg.ReadOnly {
		m.status = "profile is read-only, upload blocked"

		return m, nil
	}

	if m.bucket == "" {
		m.status = "open a bucket first to upload into it"

		return m, nil
	}

	m.prompt = newPrompt(promptRandom,
		fmt.Sprintf("random objects into s3://%s/%s, size and count", m.bucket, m.prefix),
		"1 MiB 10", m.width,
	)

	return m, textinput.Blink
}
