package compress

import (
	"io"

	"github.com/cloud-barista/ai-ha/pkg/backup/model"
)

// noneCompressor performs no compression; it is a pass-through codec that also
// serves as the reference implementation for adding new codecs (e.g. gzip).
type noneCompressor struct{}

func (noneCompressor) Type() model.CompressionType { return model.CompressionNone }

func (noneCompressor) NewWriter(w io.Writer) (io.WriteCloser, error) {
	return nopWriteCloser{w}, nil
}

func (noneCompressor) NewReader(r io.Reader) (io.ReadCloser, error) {
	return io.NopCloser(r), nil
}

// nopWriteCloser adds a no-op Close to a writer that does not need flushing.
type nopWriteCloser struct{ io.Writer }

func (nopWriteCloser) Close() error { return nil }

func init() {
	Register(noneCompressor{})
}
