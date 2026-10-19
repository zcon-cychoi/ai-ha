package compress

import (
	"io"

	"github.com/cloud-barista/ai-ha/pkg/backup/model"
	"github.com/klauspost/compress/zstd"
)

// zstdCompressor is the default codec.
type zstdCompressor struct{}

func (zstdCompressor) Type() model.CompressionType { return model.CompressionZstd }

func (zstdCompressor) NewWriter(w io.Writer) (io.WriteCloser, error) {
	// *zstd.Encoder satisfies io.WriteCloser (Write + Close flushes and finishes).
	return zstd.NewWriter(w)
}

func (zstdCompressor) NewReader(r io.Reader) (io.ReadCloser, error) {
	d, err := zstd.NewReader(r)
	if err != nil {
		return nil, err
	}
	return d.IOReadCloser(), nil
}

func init() {
	Register(zstdCompressor{})
}
