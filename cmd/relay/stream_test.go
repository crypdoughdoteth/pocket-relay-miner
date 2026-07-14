package relay

import (
	"bytes"
	"errors"
	"io"
	"testing"

	"github.com/stretchr/testify/require"
)

// streamDelimiterBytes is the on-wire batch separator, resolved once for tests.
var streamDelimiterBytes = []byte(streamDelimiter)

// errBoom is a non-EOF reader failure used to exercise the mid-stream error path.
var errBoom = errors.New("boom: connection dropped")

// errReadTooFar signals that a reader was consumed past the point the test
// allows, i.e. readStreamingBatches failed to stop at maxBatches.
var errReadTooFar = errors.New("reader read past the allowed batch count")

// makeBatches builds n distinct binary batches. Each batch embeds its index and
// deliberately includes a newline plus non-UTF8 bytes to prove the reader hands
// back raw protobuf payloads untouched (no trimming, no re-encoding).
func makeBatches(n int) [][]byte {
	batches := make([][]byte, n)
	for i := range batches {
		batches[i] = []byte{'B', byte(i), '\n', 0x00, 0xFF, 0xFE, byte(i*31 + 7)}
	}
	return batches
}

// batchChunks turns each batch into its own delimiter-suffixed chunk so a
// chunkedReader can deliver exactly one batch per Read call.
func batchChunks(batches [][]byte) [][]byte {
	chunks := make([][]byte, len(batches))
	for i, b := range batches {
		chunk := make([]byte, 0, len(b)+len(streamDelimiterBytes))
		chunk = append(chunk, b...)
		chunk = append(chunk, streamDelimiterBytes...)
		chunks[i] = chunk
	}
	return chunks
}

// withTrailingDelimiters concatenates batches each followed by the delimiter,
// matching the relayer's wire format (every signed batch is suffixed with the
// delimiter).
func withTrailingDelimiters(batches [][]byte) []byte {
	var out []byte
	for _, b := range batches {
		out = append(out, b...)
		out = append(out, streamDelimiterBytes...)
	}
	return out
}

// chunkedReader delivers each pre-built chunk in its own Read call, forcing
// bufio.Scanner to read incrementally instead of slurping the whole stream at
// once. It records how many Read calls happened; when failAfter > 0 it returns
// errReadTooFar once consumed past that many reads, letting a test assert that
// readStreamingBatches stopped early instead of draining the reader.
type chunkedReader struct {
	chunks    [][]byte
	idx       int
	reads     int
	failAfter int
}

func (r *chunkedReader) Read(p []byte) (int, error) {
	r.reads++
	if r.failAfter > 0 && r.reads > r.failAfter {
		return 0, errReadTooFar
	}
	if r.idx >= len(r.chunks) {
		return 0, io.EOF
	}
	chunk := r.chunks[r.idx]
	n := copy(p, chunk)
	if n < len(chunk) {
		// The scanner passes a 256KB buffer and chunks are tiny, so a short
		// copy would only happen if the test itself is misconfigured. Fail
		// loudly rather than desync chunk boundaries.
		return n, io.ErrShortBuffer
	}
	r.idx++
	return n, nil
}

// errReader yields data on the first Read(s) and then returns err on the next
// Read. With data set to a complete, delimiter-suffixed batch stream it models a
// server that sends some batches and then drops the connection (or the client
// timeout firing).
type errReader struct {
	data []byte
	err  error
}

func (r *errReader) Read(p []byte) (int, error) {
	if len(r.data) > 0 {
		n := copy(p, r.data)
		r.data = r.data[n:]
		return n, nil
	}
	return 0, r.err
}

// TestReadStreamingBatches_StopsAtMaxBatches proves termination by count: given a
// stream longer than maxBatches, the reader returns exactly maxBatches batches,
// byte-for-byte, and never drains the rest of the (infinite) stream.
func TestReadStreamingBatches_StopsAtMaxBatches(t *testing.T) {
	batches := makeBatches(10)
	const maxBatches = 3

	// failAfter == maxBatches: one Read per collected batch, so a 4th Read (which
	// would only happen if the count bound were ignored) trips errReadTooFar.
	reader := &chunkedReader{chunks: batchChunks(batches), failAfter: maxBatches}

	got, err := readStreamingBatches(reader, maxBatches)

	require.NoError(t, err)
	require.Len(t, got, maxBatches, "must stop at exactly maxBatches")
	require.Equal(t, maxBatches, reader.reads, "must not read past maxBatches chunks")
	for i := 0; i < maxBatches; i++ {
		require.Equal(t, batches[i], got[i], "batch %d must be returned byte-exact", i)
	}
}

// TestReadStreamingBatches_PartialOnReaderError proves that batches collected
// before a mid-stream reader error are returned (with nil error), not discarded —
// this is the exact bug: the client timeout fired and thousands of received bytes
// were thrown away.
func TestReadStreamingBatches_PartialOnReaderError(t *testing.T) {
	batches := makeBatches(2)
	// Deliver both complete, delimiter-terminated batches, then fail.
	reader := &errReader{data: withTrailingDelimiters(batches), err: errBoom}

	// maxBatches larger than what the stream provides, so termination happens via
	// the reader error, not the count bound.
	got, err := readStreamingBatches(reader, 5)

	require.NoError(t, err, "reader error after collecting batches must be swallowed")
	require.Len(t, got, 2)
	require.Equal(t, batches[0], got[0])
	require.Equal(t, batches[1], got[1])
}

// TestReadStreamingBatches_ErrorWithZeroBatches proves the error is surfaced only
// when nothing was collected. The reader errors before emitting any bytes: any
// leftover bytes at error would be emitted as a trailing-token batch, so a true
// zero-batch failure requires the reader to fail before the first byte.
func TestReadStreamingBatches_ErrorWithZeroBatches(t *testing.T) {
	reader := &errReader{data: nil, err: errBoom}

	got, err := readStreamingBatches(reader, 3)

	require.Error(t, err)
	require.ErrorIs(t, err, errBoom, "underlying reader error must be wrapped")
	require.Empty(t, got, "no batches must be returned on a pre-delimiter failure")
}

// TestReadStreamingBatches_EOFBeforeMax proves a clean EOF before maxBatches
// returns everything collected with nil error, and that the trailing token after
// the last delimiter counts as a batch (atEOF semantics).
func TestReadStreamingBatches_EOFBeforeMax(t *testing.T) {
	batches := makeBatches(2)
	// bytes.Join produces "b0 <delim> b1" (no trailing delimiter), so b1 is the
	// trailing token emitted at EOF.
	reader := bytes.NewReader(bytes.Join(batches, streamDelimiterBytes))

	got, err := readStreamingBatches(reader, 5)

	require.NoError(t, err)
	require.Len(t, got, 2, "both batches must survive a clean EOF short of maxBatches")
	require.Equal(t, batches[0], got[0])
	require.Equal(t, batches[1], got[1], "trailing token after the last delimiter must count")
}

// TestReadStreamingBatches_MinimumOne proves the defensive floor: a maxBatches of
// 0 is normalized to 1 inside readStreamingBatches, so a multi-batch stream still
// terminates after exactly one batch and one Read.
func TestReadStreamingBatches_MinimumOne(t *testing.T) {
	batches := makeBatches(3)
	// failAfter == 1: a second Read (which would happen only if the floor were
	// missing and the scan ran on) trips errReadTooFar.
	reader := &chunkedReader{chunks: batchChunks(batches), failAfter: 1}

	got, err := readStreamingBatches(reader, 0)

	require.NoError(t, err)
	require.Len(t, got, 1, "maxBatches=0 must normalize to a floor of 1")
	require.Equal(t, 1, reader.reads, "must read exactly one chunk")
	require.Equal(t, batches[0], got[0])
}
