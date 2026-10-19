package flockcli

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"os"
	"path"
	"path/filepath"
	"sync"

	"github.com/mhadifilms/flock/src/croc"
	"github.com/mhadifilms/flock/src/models"
)

const verifyReadSize = 1 << 20
const verifyChunkSize = int64(models.TCP_BUFFER_SIZE / 2)

// Large destinations are read back in order while later chunks arrive. A bitset
// prevents hashing holes and rejects repeated writes; no network payload is used
// as a substitute for reading the destination file. Small files retain the
// ordinary post-transfer path. Memory is bounded even for hostile file sizes.
type receiveVerifier struct {
	deferred      bool
	manifestReady chan struct{}
	manifestMu    sync.Mutex
	manifestSeen  bool
	expectedFiles []File
	ctx           context.Context
	cancel        context.CancelFunc
	files         map[string]*receiveFileVerifier
}

func newReceiveVerifier(ctx context.Context, files []File) *receiveVerifier {
	ctx, cancel := context.WithCancel(ctx)
	v := &receiveVerifier{ctx: ctx, cancel: cancel, files: make(map[string]*receiveFileVerifier)}
	v.deferred = len(files) == 1 && files[0].SHA256 == ""
	v.manifestReady = make(chan struct{})
	v.expectedFiles = append([]File(nil), files...)
	if !v.deferred {
		close(v.manifestReady)
	}
	for _, file := range files {
		if file.Size < 8<<20 {
			continue
		}
		chunks := (file.Size-1)/verifyChunkSize + 1
		words := (chunks-1)/64 + 1
		if words > 1<<20 {
			continue
		} // At most 8 MiB of coverage bits per active file.
		v.files[file.Path] = &receiveFileVerifier{ctx: ctx, want: file, words: words}
	}
	return v
}
func (v *receiveVerifier) file(info croc.FileInfo) *receiveFileVerifier {
	return v.files[path.Join(info.FolderRemote, info.Name)]
}
func (v *receiveVerifier) written(info croc.FileInfo, position int64, length int) error {
	if file := v.file(info); file != nil {
		return file.written(position, length)
	}
	return nil
}
func (v *receiveVerifier) finishFile(info croc.FileInfo) error {
	if err := v.waitManifest(v.ctx); err != nil {
		return err
	}
	if file := v.file(info); file != nil {
		return file.finish()
	}
	return nil
}
func (v *receiveVerifier) verified(name string) bool {
	file := v.files[name]
	if file == nil {
		return false
	}
	file.mu.Lock()
	defer file.mu.Unlock()
	return file.complete && file.err == nil
}
func (v *receiveVerifier) close() {
	v.cancel()
	for _, file := range v.files {
		file.mu.Lock()
		done := file.done
		file.mu.Unlock()
		if done != nil {
			<-done
		}
	}
}

type receiveFileVerifier struct {
	ctx        context.Context
	want       File
	words      int64
	mu         sync.Mutex
	seen       []uint64
	contiguous int64
	ready      int64
	finishing  bool
	complete   bool
	err        error
	wake       chan struct{}
	done       chan struct{}
}

func (v *receiveFileVerifier) written(position int64, length int) error {
	v.mu.Lock()
	defer v.mu.Unlock()
	if v.err != nil {
		return v.err
	}
	if position < 0 || position >= v.want.Size || position%verifyChunkSize != 0 || int64(length) != min(verifyChunkSize, v.want.Size-position) {
		return errors.New("invalid SHA-256 verification chunk")
	}
	if v.done == nil {
		file, err := os.Open(filepath.FromSlash(v.want.Path))
		if err != nil {
			return err
		}
		v.seen = make([]uint64, v.words)
		v.wake = make(chan struct{}, 1)
		v.done = make(chan struct{})
		go v.readBack(file)
	}
	index := position / verifyChunkSize
	mask := uint64(1) << uint(index%64)
	if v.finishing || v.seen[index/64]&mask != 0 {
		return errors.New("duplicate SHA-256 verification chunk")
	}
	v.seen[index/64] |= mask
	previous := v.ready
	chunks := (v.want.Size-1)/verifyChunkSize + 1
	for v.contiguous < chunks && v.seen[v.contiguous/64]&(uint64(1)<<uint(v.contiguous%64)) != 0 {
		v.contiguous++
	}
	v.ready = min(v.contiguous*verifyChunkSize, v.want.Size)
	if v.ready/verifyReadSize > previous/verifyReadSize {
		v.notify()
	}
	return nil
}
func (v *receiveFileVerifier) notify() {
	select {
	case v.wake <- struct{}{}:
	default:
	}
}
func (v *receiveFileVerifier) finish() error {
	v.mu.Lock()
	if v.done == nil {
		v.mu.Unlock()
		return errors.New("received file has no verified chunks")
	}
	v.finishing = true
	v.notify()
	done := v.done
	v.mu.Unlock()
	select {
	case <-done:
	case <-v.ctx.Done():
		return v.ctx.Err()
	}
	v.mu.Lock()
	defer v.mu.Unlock()
	return v.err
}
func (v *receiveFileVerifier) readBack(file *os.File) {
	var result error
	defer func() {
		closeErr := file.Close()
		v.mu.Lock()
		v.err = errors.Join(result, closeErr)
		v.complete = v.err == nil
		v.seen = nil
		v.mu.Unlock()
		close(v.done)
	}()
	before, err := file.Stat()
	if err != nil {
		result = err
		return
	}
	if !before.Mode().IsRegular() {
		result = errors.New("receive destination is not a regular file")
		return
	}
	digest := sha256.New()
	buffer := make([]byte, verifyReadSize)
	var hashed int64
	for {
		select {
		case <-v.ctx.Done():
			result = v.ctx.Err()
			return
		case <-v.wake:
		}
		v.mu.Lock()
		ready, finishing := v.ready, v.finishing
		v.mu.Unlock()
		for ready-hashed >= verifyReadSize || (finishing && hashed < ready) {
			if err := v.ctx.Err(); err != nil {
				result = err
				return
			}
			size := min(int64(len(buffer)), ready-hashed)
			n, err := file.ReadAt(buffer[:size], hashed)
			if err != nil || int64(n) != size {
				result = err
				if result == nil {
					result = io.ErrUnexpectedEOF
				}
				return
			}
			digest.Write(buffer[:n])
			hashed += int64(n)
		}
		if !finishing {
			continue
		}
		if hashed != v.want.Size {
			result = errors.New("received file SHA-256 coverage incomplete")
			return
		}
		current, err := os.Lstat(filepath.FromSlash(v.want.Path))
		if err != nil {
			result = err
			return
		}
		if !current.Mode().IsRegular() || current.Size() != v.want.Size || !os.SameFile(before, current) || hex.EncodeToString(digest.Sum(nil)) != v.want.SHA256 {
			result = errors.New("received file SHA-256 or size mismatch")
			return
		}
		return
	}
}

// A version-2 invitation commits to names and sizes immediately; the complete
// digest arrives later over the authenticated control channel. It cannot change
// that inventory, and success cannot precede this message and full readback.
func (v *receiveVerifier) acceptManifest(data []byte) (err error) {
	defer func() {
		if err != nil {
			v.cancel()
		}
	}()
	if !v.deferred || len(data) > maxInvite {
		return errors.New("unexpected source SHA-256 manifest")
	}
	v.manifestMu.Lock()
	defer v.manifestMu.Unlock()
	if v.manifestSeen {
		return errors.New("duplicate source SHA-256 manifest")
	}
	var files []File
	if json.Unmarshal(data, &files) != nil || len(files) != len(v.expectedFiles) {
		return errors.New("invalid source SHA-256 manifest")
	}
	for i, file := range files {
		expected := v.expectedFiles[i]
		digest, err := hex.DecodeString(file.SHA256)
		if file.Path != expected.Path || file.Size != expected.Size || err != nil || len(digest) != 32 {
			return errors.New("source SHA-256 manifest differs from invitation")
		}
	}
	v.expectedFiles = files
	for _, file := range files {
		if state := v.files[file.Path]; state != nil {
			state.mu.Lock()
			state.want.SHA256 = file.SHA256
			state.mu.Unlock()
		}
	}
	v.manifestSeen = true
	close(v.manifestReady)
	return nil
}
func (v *receiveVerifier) waitManifest(ctx context.Context) error {
	select {
	case <-v.manifestReady:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}
func (v *receiveVerifier) manifest(ctx context.Context) ([]File, error) {
	if err := v.waitManifest(ctx); err != nil {
		return nil, err
	}
	v.manifestMu.Lock()
	defer v.manifestMu.Unlock()
	return append([]File(nil), v.expectedFiles...), nil
}
