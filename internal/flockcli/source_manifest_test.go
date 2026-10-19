package flockcli

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/mhadifilms/flock/src/croc"
)

func TestFlockDeferredSourceManifestRejectsChangedSource(t *testing.T) {
	name := filepath.Join(t.TempDir(), "source.bin")
	if err := os.WriteFile(name, []byte("original"), 0600); err != nil {
		t.Fatal(err)
	}
	info, err := os.Stat(name)
	if err != nil {
		t.Fatal(err)
	}
	source := croc.FileInfo{Name: filepath.Base(name), FolderSource: filepath.Dir(name), Size: info.Size(), Mode: info.Mode(), ModTime: info.ModTime()}
	if err := os.WriteFile(name, []byte("modified"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.Chtimes(name, info.ModTime().Add(time.Second), info.ModTime().Add(time.Second)); err != nil {
		t.Fatal(err)
	}
	manifest := &sourceManifest{ready: make(chan struct{})}
	manifest.start(context.Background(), []croc.FileInfo{source}, []File{{Path: "source.bin", Size: info.Size()}})
	if _, err := manifest.get(context.Background()); err == nil {
		t.Fatal("changed source was committed")
	}
}
