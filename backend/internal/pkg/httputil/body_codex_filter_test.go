package httputil

import (
	"bytes"
	"compress/gzip"
	"compress/zlib"
	"io"
	"net/http"
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
