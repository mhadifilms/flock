package flockcli

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"

	"github.com/mhadifilms/flock/src/croc"
)

type sourceManifest struct {
	ready chan struct{}
	files []File
	err   error
}

func (m *sourceManifest) start(ctx context.Context, sources []croc.FileInfo, expected []File) {
	go func() {
		defer close(m.ready)
		for i, source := range sources {
			name := filepath.Join(source.FolderSource, source.Name)
			digest, size, err := hashFile(ctx, name)
			if err != nil {
				m.err = err
				return
			}
			info, err := os.Lstat(name)
			if err != nil {
				m.err = err
				return
			}
			if info.Size() != source.Size || !info.ModTime().Equal(source.ModTime) || info.Mode() != source.Mode {
				m.err = errors.New("source changed while computing deferred SHA-256")
				return
			}
			m.files = append(m.files, File{Path: expected[i].Path, Size: size, SHA256: digest})
		}
	}()
}
func (m *sourceManifest) get(ctx context.Context) ([]File, error) {
	select {
	case <-m.ready:
		return m.files, m.err
	case <-ctx.Done():
		return nil, ctx.Err()
	}
}
func (m *sourceManifest) encoded(ctx context.Context) ([]byte, error) {
	files, err := m.get(ctx)
	if err != nil {
		return nil, err
	}
	return json.Marshal(files)
}
