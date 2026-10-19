package flockcli

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"github.com/mhadifilms/flock/src/croc"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"
)

func verificationFixture(t *testing.T, size int) (*receiveFileVerifier, *os.File, []byte, context.CancelFunc) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	t.Cleanup(cancel)
	data := make([]byte, size)
	for i := range data {
		data[i] = byte(i*73 + i/997)
	}
	digest := sha256.Sum256(data)
	name := filepath.Join(t.TempDir(), "destination.bin")
	file, err := os.Create(name)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { file.Close() })
	chunks := (int64(size)-1)/verifyChunkSize + 1
	v := &receiveFileVerifier{ctx: ctx, want: File{Path: name, Size: int64(size), SHA256: hex.EncodeToString(digest[:])}, words: (chunks-1)/64 + 1}
	t.Cleanup(func() {
		cancel()
		v.mu.Lock()
		done := v.done
		v.mu.Unlock()
		if done != nil {
			<-done
		}
	})
	return v, file, data, cancel
}
func TestReceiveVerifierConcurrentOutOfOrderReadback(t *testing.T) {
	v, file, data, _ := verificationFixture(t, (9<<20)+37)
	chunks := (len(data)-1)/int(verifyChunkSize) + 1
	var workers sync.WaitGroup
	// Reverse order leaves a hole at the beginning until the end, and exercises
	// parallel callbacks, multiple readback blocks, and a short final chunk.
	for worker := range 8 {
		workers.Go(func() {
			for index := chunks - 1 - worker; index >= 0; index -= 8 {
				offset := int64(index) * verifyChunkSize
				payload := data[offset:min(offset+verifyChunkSize, int64(len(data)))]
				if _, err := file.WriteAt(payload, offset); err != nil {
					t.Error(err)
					return
				}
				if err := v.written(offset, len(payload)); err != nil {
					t.Error(err)
					return
				}
			}
		})
	}
	workers.Wait()
	if err := v.finish(); err != nil {
		t.Fatal(err)
	}
	v.mu.Lock()
	complete := v.complete
	bits := v.seen
	v.mu.Unlock()
	if !complete || bits != nil {
		t.Fatal("verification did not complete and release coverage memory")
	}
	if err := v.written(0, int(verifyChunkSize)); err == nil {
		t.Fatal("accepted a write after verification")
	}
}
func TestReceiveVerifierRejectsMissingDuplicateAndCorruptData(t *testing.T) {
	for _, mode := range []string{"missing", "duplicate", "corrupt", "unaligned", "wrong-size", "replaced", "wrong-length"} {
		t.Run(mode, func(t *testing.T) {
			v, file, data, _ := verificationFixture(t, (8<<20)+37)
			if _, err := file.WriteAt(data, 0); err != nil {
				t.Fatal(err)
			}
			switch mode {
			case "duplicate":
				if err := v.written(0, int(verifyChunkSize)); err != nil {
					t.Fatal(err)
				}
				if err := v.written(0, int(verifyChunkSize)); err == nil {
					t.Fatal("accepted duplicate")
				}
				return
			case "unaligned":
				if err := v.written(1, int(verifyChunkSize)); err == nil {
					t.Fatal("accepted unaligned chunk")
				}
				return
			case "wrong-size":
				if err := v.written(0, 1); err == nil {
					t.Fatal("accepted short interior chunk")
				}
				return
			case "corrupt":
				if _, err := file.WriteAt([]byte("bad"), 0); err != nil {
					t.Fatal(err)
				}
			}
			end := len(data)
			if mode == "missing" {
				end = int(verifyChunkSize)
			}
			for offset := int64(0); offset < int64(end); offset += verifyChunkSize {
				if err := v.written(offset, int(min(verifyChunkSize, int64(end)-offset))); err != nil {
					t.Fatal(err)
				}
			}
			if mode == "replaced" {
				if err := os.Rename(v.want.Path, v.want.Path+"-original"); err != nil {
					// Windows prevents replacing an open destination. That also
					// satisfies this test's protection against identity replacement.
					if runtime.GOOS == "windows" && errors.Is(err, syscall.Errno(32)) {
						return
					}
					t.Fatal(err)
				}
				if err := os.WriteFile(v.want.Path, data, 0600); err != nil {
					t.Fatal(err)
				}
			}
			if mode == "wrong-length" {
				if err := file.Truncate(int64(len(data) + 1)); err != nil {
					t.Fatal(err)
				}
			}
			if err := v.finish(); err == nil {
				t.Fatal("invalid destination verified")
			}
		})
	}
}
func TestReceiveVerifierCancellationAndBounds(t *testing.T) {
	v, file, data, cancel := verificationFixture(t, 8<<20)
	if _, err := file.WriteAt(data[:verifyChunkSize], 0); err != nil {
		t.Fatal(err)
	}
	if err := v.written(0, int(verifyChunkSize)); err != nil {
		t.Fatal(err)
	}
	cancel()
	if err := v.finish(); err == nil {
		t.Fatal("cancelled verification succeeded")
	}
	large := newReceiveVerifier(context.Background(), []File{{Path: "enormous", Size: 1<<63 - 1}, {Path: "small", Size: 1}})
	defer large.close()
	if len(large.files) != 0 {
		t.Fatal("unbounded coverage allocation")
	}
	if !bytes.Equal(data[:3], []byte{0, 73, 146}) {
		t.Fatal("fixture changed")
	}
}

func TestReceiveVerifierWaitsForAuthenticatedSourceDigest(t *testing.T) {
	for _, mode := range []string{"valid", "wrong-digest", "wrong-inventory", "missing"} {
		t.Run(mode, func(t *testing.T) {
			state, file, data, _ := verificationFixture(t, (8<<20)+37)
			want := state.want
			want.Path = filepath.ToSlash(want.Path)
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			unsigned := want
			unsigned.SHA256 = ""
			verifier := newReceiveVerifier(ctx, []File{unsigned})
			defer verifier.close()
			info := croc.FileInfo{Name: filepath.Base(want.Path), FolderRemote: filepath.ToSlash(filepath.Dir(want.Path))}
			if _, err := file.WriteAt(data, 0); err != nil {
				t.Fatal(err)
			}
			for position := int64(0); position < int64(len(data)); position += verifyChunkSize {
				if err := verifier.written(info, position, int(min(verifyChunkSize, int64(len(data))-position))); err != nil {
					t.Fatal(err)
				}
			}
			finished := make(chan error, 1)
			go func() { finished <- verifier.finishFile(info) }()
			select {
			case err := <-finished:
				t.Fatalf("completed without source digest: %v", err)
			case <-time.After(10 * time.Millisecond):
			}
			if mode == "missing" {
				cancel()
				if err := <-finished; err == nil {
					t.Fatal("missing source digest succeeded")
				}
				return
			}
			announced := want
			if mode == "wrong-digest" {
				announced.SHA256 = strings.Repeat("0", 64)
			}
			if mode == "wrong-inventory" {
				announced.Size++
			}
			encoded, _ := json.Marshal([]File{announced})
			err := verifier.acceptManifest(encoded)
			if mode == "wrong-inventory" {
				if err == nil {
					t.Fatal("accepted changed inventory")
				}
				if err := <-finished; err == nil {
					t.Fatal("changed inventory succeeded")
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			err = <-finished
			if mode == "wrong-digest" {
				if err == nil {
					t.Fatal("unverified source digest succeeded")
				}
				return
			}
			if err != nil || !verifier.verified(want.Path) {
				t.Fatalf("valid digest failed: %v", err)
			}
			if err := verifier.acceptManifest(encoded); err == nil {
				t.Fatal("accepted duplicate source digest")
			}
		})
	}
}
