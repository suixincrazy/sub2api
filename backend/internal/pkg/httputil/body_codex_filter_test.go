package httputil

import (
	"bytes"
	"compress/gzip"
	"compress/zlib"
	"context"
	"io"
	"net/http"
	"strconv"
	"testing"

	"github.com/andybalholm/brotli"
	"github.com/klauspost/compress/zstd"
	"github.com/stretchr/testify/require"
)

func TestCompressedBodiesPreserveContentAndEnforceDecodedLimit(t *testing.T) {
	payload := []byte(samplePayload)
	for _, encoding := range []string{"gzip", "deflate", "br", "zstd"} {
		t.Run(encoding, func(t *testing.T) {
			var compressed bytes.Buffer
			var encoder io.WriteCloser
			switch encoding {
			case "gzip":
				encoder = gzip.NewWriter(&compressed)
			case "deflate":
				encoder = zlib.NewWriter(&compressed)
			case "br":
				encoder = brotli.NewWriter(&compressed)
			case "zstd":
				var err error
				encoder, err = zstd.NewWriter(&compressed)
				require.NoError(t, err)
			}
			_, err := encoder.Write(payload)
			require.NoError(t, err)
			require.NoError(t, encoder.Close())
			request := newRequestWithBody(t, compressed.Bytes(), encoding)
			actual, err := ReadRequestBodyWithPrealloc(request)
			require.NoError(t, err)
			require.Equal(t, payload, actual)
			require.Empty(t, request.Header.Get("Content-Encoding"))
			require.EqualValues(t, len(payload), request.ContentLength)
			actual, err = decompressRequestBodyLimit(encoding, compressed.Bytes(), int64(len(payload)))
			require.NoError(t, err)
			require.Equal(t, payload, actual)
			actual, err = decompressRequestBodyLimit(encoding, compressed.Bytes(), int64(len(payload)-1))
			var tooLarge *http.MaxBytesError
			require.ErrorAs(t, err, &tooLarge)
			require.Nil(t, actual, "never return a silently truncated body")
		})
	}
}

func TestZstdRequestPreservesLargeWindowCompatibility(t *testing.T) {
	payload := []byte(samplePayload)
	var compressed bytes.Buffer
	encoder, err := zstd.NewWriter(&compressed, zstd.WithWindowSize(128<<20))
	require.NoError(t, err)
	_, err = encoder.Write(payload)
	require.NoError(t, err)
	require.NoError(t, encoder.Close())
	frame := compressed.Bytes()
	var header zstd.Header
	require.NoError(t, header.Decode(frame))
	header.SingleSegment = false
	header.WindowSize = 128 << 20
	wideHeader, err := header.AppendTo(nil)
	require.NoError(t, err)
	frame = append(wideHeader, frame[header.HeaderSize:]...)
	require.NoError(t, header.Decode(frame))
	require.EqualValues(t, 128<<20, header.WindowSize)
	actual, err := ReadRequestBodyWithPrealloc(newRequestWithBody(t, frame, "zstd"))
	require.NoError(t, err, "small requests remain valid with the existing zstd window limit")
	require.Equal(t, payload, actual)
}

func TestZstdDeclaredWindowSharesBudgetAndReleasesDecoderMemory(t *testing.T) {
	for _, exponent := range []byte{24, 27} {
		t.Run(strconv.Itoa(int(exponent)), func(t *testing.T) {
			window := int64(1) << exponent
			budget := NewBodyMemoryBudget(2*window + 1<<20)
			heldCtx, releaseHeld := WithRequestBodyMemoryBudget(context.Background(), budget)
			defer releaseHeld()
			// The window alone fits; decoder workspace must also be admitted.
			require.NoError(t, ReserveRequestBodyMemory(heldCtx, budget.limit-window-(7<<20)))
			ctx, release := WithRequestBodyMemoryBudget(context.Background(), budget)
			defer release()
			// A valid frame with one literal byte and a declared 16/128 MiB window.
			raw := []byte{0x28, 0xb5, 0x2f, 0xfd, 0, (exponent - 10) << 3, 9, 0, 0, 'x'}
			before := requestBodyBudgetUsed(budget)
			actual, err := ReadRequestBodyWithPrealloc(newRequestWithBody(t, raw, "zstd").WithContext(ctx))
			require.ErrorIs(t, err, ErrRequestBodyMemoryBudget)
			require.Nil(t, actual)
			require.Equal(t, before, requestBodyBudgetUsed(budget), "failed decode must preserve another request's reservation")

			releaseHeld()
			actual, err = ReadRequestBodyWithPrealloc(newRequestWithBody(t, raw, "zstd").WithContext(ctx))
			require.NoError(t, err)
			require.Equal(t, "x", string(actual))
			require.EqualValues(t, cap(actual), requestBodyBudgetUsed(budget), "only the decoded body remains live")
			release()
			require.Zero(t, requestBodyBudgetUsed(budget))
		})
	}
}

func TestZstdRequestPreservesConcatenatedFrameWindows(t *testing.T) {
	small := []byte{0x28, 0xb5, 0x2f, 0xfd, 0, (20 - 10) << 3, 9, 0, 0, 'x'}
	large := []byte{0x28, 0xb5, 0x2f, 0xfd, 0, (27 - 10) << 3, 9, 0, 0, 'y'}
	skippable := []byte{0x50, 0x2a, 0x4d, 0x18, 3, 0, 0, 0, 's', 'k', 'p'}
	rle := []byte{0x28, 0xb5, 0x2f, 0xfd, 0, (20 - 10) << 3, 43, 0, 0, 'z'}
	multiBlock := []byte{0x28, 0xb5, 0x2f, 0xfd, 0, (20 - 10) << 3, 8, 0, 0, 'a', 9, 0, 0, 'b'}
	encoder, err := zstd.NewWriter(nil, zstd.WithEncoderConcurrency(1), zstd.WithEncoderCRC(true))
	require.NoError(t, err)
	checked := encoder.EncodeAll([]byte("checksum"), nil)
	require.NoError(t, encoder.Close())
	for _, tc := range []struct {
		name string
		raw  []byte
		want string
	}{
		{name: "small_then_large", raw: append(append([]byte(nil), small...), large...), want: "xy"},
		{name: "skippable_then_large", raw: append(append([]byte(nil), skippable...), large...), want: "y"},
		{name: "rle_then_large", raw: append(append([]byte(nil), rle...), large...), want: "zzzzzy"},
		{name: "multiple_blocks_then_large", raw: append(append([]byte(nil), multiBlock...), large...), want: "aby"},
		{name: "checksum_then_large", raw: append(append([]byte(nil), checked...), large...), want: "checksumy"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			budget := NewBodyMemoryBudget(256 << 20)
			ctx, release := WithRequestBodyMemoryBudget(context.Background(), budget)
			defer release()
			actual, err := ReadRequestBodyWithPrealloc(newRequestWithBody(t, tc.raw, "zstd").WithContext(ctx))
			require.NoError(t, err, "each frame stays within the supported window limit")
			require.Equal(t, tc.want, string(actual))
			require.EqualValues(t, cap(actual), requestBodyBudgetUsed(budget))
		})
	}
}

func TestZstdInvalidLaterFrameReleasesBudget(t *testing.T) {
	first := []byte{0x28, 0xb5, 0x2f, 0xfd, 0, (20 - 10) << 3, 9, 0, 0, 'x'}
	encoder, err := zstd.NewWriter(nil, zstd.WithEncoderConcurrency(1), zstd.WithEncoderCRC(true))
	require.NoError(t, err)
	checked := encoder.EncodeAll([]byte("checksum"), nil)
	require.NoError(t, encoder.Close())
	badChecksum := append([]byte(nil), checked...)
	badChecksum[len(badChecksum)-1] ^= 1
	for _, tc := range []struct {
		name string
		last []byte
	}{
		{name: "excessive_window", last: []byte{0x28, 0xb5, 0x2f, 0xfd, 0, (29 - 10) << 3, 9, 0, 0, 'y'}},
		{name: "truncated_header", last: first[:5]},
		{name: "truncated_block", last: first[:len(first)-1]},
		{name: "truncated_skippable", last: []byte{0x50, 0x2a, 0x4d, 0x18, 3, 0, 0, 0, 's'}},
		{name: "truncated_checksum", last: checked[:len(checked)-1]},
		{name: "invalid_checksum", last: badChecksum},
		{name: "reserved_block", last: []byte{0x28, 0xb5, 0x2f, 0xfd, 0, (20 - 10) << 3, 7, 0, 0}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			budget := NewBodyMemoryBudget(256 << 20)
			ctx, release := WithRequestBodyMemoryBudget(context.Background(), budget)
			defer release()
			require.NoError(t, ReserveRequestBodyMemory(ctx, 100))
			raw := append(append([]byte(nil), first...), tc.last...)
			actual, err := ReadRequestBodyWithPrealloc(newRequestWithBody(t, raw, "zstd").WithContext(ctx))
			require.Error(t, err)
			require.Nil(t, actual, "do not return a silently truncated first frame")
			require.EqualValues(t, 100, requestBodyBudgetUsed(budget), "failed decoding must release only its own buffers")
		})
	}
}
