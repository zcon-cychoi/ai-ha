// Package compress provides a pluggable compression layer for backup archives.
//
// A Compressor wraps an io.Writer (for backup) or io.Reader (for restore) with
// a codec. Codecs register themselves via Register in their init function, so
// new codecs (e.g. gzip) can be added without touching the engine.
package compress

import (
	"fmt"
	"io"
	"slices"

	"github.com/cloud-barista/ai-ha/pkg/backup/model"
)

// Compressor is a compression codec usable for both backup and restore.
type Compressor interface {
	// Type returns the codec identifier.
	Type() model.CompressionType
	// NewWriter returns a WriteCloser that compresses into w. Callers must
	// Close it to flush before closing the underlying writer.
	NewWriter(w io.Writer) (io.WriteCloser, error)
	// NewReader returns a ReadCloser that decompresses from r.
	NewReader(r io.Reader) (io.ReadCloser, error)
}

var registry = map[model.CompressionType]Compressor{}

// Register makes a codec available to Get. It is intended to be called from a
// codec's init function.
func Register(c Compressor) {
	registry[c.Type()] = c
}

// Get returns the registered codec for t, or an error if unsupported.
func Get(t model.CompressionType) (Compressor, error) {
	if t == "" {
		t = model.DefaultCompression
	}
	c, ok := registry[t]
	if !ok {
		return nil, fmt.Errorf("unsupported compression type %q", t)
	}
	return c, nil
}

// Available returns the sorted list of registered codec identifiers.
func Available() []model.CompressionType {
	out := make([]model.CompressionType, 0, len(registry))
	for t := range registry {
		out = append(out, t)
	}
	slices.Sort(out)
	return out
}
