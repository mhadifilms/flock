// Package flockcli implements the versioned, non-interactive Flock interface.
package flockcli

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"net"
	"os"
	"path"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/schollz/croc/v11/src/codephrase"
	"github.com/schollz/croc/v11/src/croc"
	log "github.com/schollz/croc/v11/src/logger"
	"github.com/schollz/croc/v11/src/models"
	"github.com/schollz/croc/v11/src/redact"
	"github.com/schollz/croc/v11/src/tcp"
)

const maxInvite = 4 << 20

// File identifies the exact bytes the recipient must verify before completion.
type File struct {
	Path   string `json:"path"`
	Size   int64  `json:"bytes"`
	SHA256 string `json:"sha256"`
}

// Invitation is an expiring bearer capability, intended for trusted agent handoff.
type Invitation struct {
	Version     int      `json:"version"`
	Transport   string   `json:"transport"`
	Capability  string   `json:"capability"`
	Relay       string   `json:"relay"`
	Expires     int64    `json:"expires"`
	Files       []File   `json:"files"`
	Directories []string `json:"directories"`
}

type emitter struct {
	mu     sync.Mutex
	enc    *json.Encoder
	err    error
	cancel context.CancelFunc
	bytes  int64
	last   time.Time
}

func (e *emitter) emitLocked(event string, fields map[string]any) {
	if e.err != nil {
		return
	}
	if fields == nil {
		fields = map[string]any{}
	}
	fields["version"] = 1
	fields["event"] = event
	e.err = e.enc.Encode(fields)
	if e.err != nil {
		e.cancel()
	}
}
func (e *emitter) emit(event string, fields map[string]any) {
	e.mu.Lock()
	defer e.mu.Unlock()
	e.emitLocked(event, fields)
}
func (e *emitter) progress(delta int64) {
	e.mu.Lock()
	defer e.mu.Unlock()
	e.bytes += delta
	if time.Since(e.last) >= 100*time.Millisecond {
		e.last = time.Now()
		e.emitLocked("progress", map[string]any{"bytes_transferred": e.bytes, "transport": "quic/udp"})
	}
}

// Run never prompts and writes only JSON Lines to stdout. Stderr is diagnostics.
func Run(parent context.Context, args []string, input io.Reader, output, diagnostics io.Writer) int {
	ctx, cancel := context.WithCancel(parent)
	defer cancel()
	events := &emitter{enc: json.NewEncoder(output), cancel: cancel}
	log.SetOutput(diagnostics)
	croc.Debug(false)
	err := run(ctx, args, input, events)
	if err != nil {
		code, status := "transfer_failed", 1
		if errors.Is(err, context.DeadlineExceeded) {
			code, status = "timeout", 124
		} else if errors.Is(err, context.Canceled) {
			code, status = "cancelled", 130
		}
		events.emit("error", map[string]any{"code": code, "message": err.Error()})
		return status
	}
	events.mu.Lock()
	defer events.mu.Unlock()
	if events.err != nil {
		return 1
	}
	return 0
}
func run(ctx context.Context, args []string, input io.Reader, e *emitter) error {
	if len(args) == 0 || args[0] == "schema" || args[0] == "--help" || args[0] == "--version" {
		e.emit("schema", map[string]any{"name": "flock", "protocol": "flock-v1", "transport": "quic/udp", "commands": []string{"send --advertise IPV4 [--relay HOST:PORT] FILE_OR_DIRECTORY...", "receive --out NEW_DIRECTORY [--invite-file FILE]", "relay --listen IP:PORT", "schema"}, "events": []string{"schema", "preparing", "invitation", "connecting", "transport", "progress", "completed", "error", "ready"}, "invitation": "generated 256-bit capability; pass the invitation object or invitation event on stdin", "exit_codes": map[string]int{"success": 0, "failure": 1, "timeout": 124, "cancelled": 130}})
		return nil
	}
	command := args[0]
	if command != "send" && command != "receive" && command != "relay" {
		return errors.New("unknown command; run flock schema")
	}
	flags := flag.NewFlagSet(command, flag.ContinueOnError)
	flags.SetOutput(io.Discard)
	relay := flags.String("relay", "", "Croc control relay HOST:PORT")
	advertise := flags.String("advertise", "", "reachable sender IPv4 address")
	listen := flags.String("listen", "127.0.0.1:9009", "control relay bind address")
	controlListen := flags.String("control-listen", "0.0.0.0:0", "embedded control relay bind address")
	udpListen := flags.String("udp-listen", "0.0.0.0:0", "sender UDP bind address")
	out := flags.String("out", "", "new receive directory")
	inviteFile := flags.String("invite-file", "", "invitation JSON file; default stdin")
	timeout := flags.Duration("timeout", 30*time.Minute, "overall deadline")
	streams := flags.Int("streams", 8, "parallel QUIC streams, 1 to 16")
	flags.Bool("json", true, "JSON Lines is always enabled")
	if err := flags.Parse(args[1:]); err != nil {
		return errors.New("invalid arguments; run flock schema")
	}
	if *timeout <= 0 || *timeout > 24*time.Hour {
		return errors.New("timeout must be positive and at most 24 hours")
	}
	ctx, stop := context.WithTimeout(ctx, *timeout)
	defer stop()
	password := os.Getenv("FLOCK_RELAY_PASSWORD")
	if password == "" {
		password = models.DEFAULT_PASSPHRASE
	}
	if command == "relay" {
		if flags.NArg() != 0 {
			return errors.New("relay takes no positional arguments")
		}
		listener, err := net.Listen("tcp", *listen)
		if err != nil {
			return err
		}
		e.emit("ready", map[string]any{"address": listener.Addr().String(), "role": "control_relay", "file_data": "quic/udp"})
		return tcp.RunWithListener(listener, password, tcp.WithCtx(ctx), tcp.WithLogLevel("error"), tcp.WithBanner("1"))
	}
	var invite Invitation
	var files, folders []croc.FileInfo
	var folderCount int
	if command == "send" {
		if flags.NArg() == 0 {
			return errors.New("send requires --advertise and at least one path")
		}
		if ip := net.ParseIP(*advertise); ip == nil || ip.To4() == nil || ip.IsUnspecified() || ip.IsMulticast() {
			return errors.New("advertise must be a reachable IPv4 address")
		}
		if *relay == "" {
			listener, err := net.Listen("tcp4", *controlListen)
			if err != nil {
				return err
			}
			defer listener.Close()
			*relay = net.JoinHostPort(*advertise, fmt.Sprint(listener.Addr().(*net.TCPAddr).Port))
			go func() {
				_ = tcp.RunWithListener(listener, password, tcp.WithCtx(ctx), tcp.WithLogLevel("error"), tcp.WithBanner("1"))
			}()
		}
		e.emit("preparing", nil)
		var err error
		files, folders, folderCount, err = croc.GetFilesInfoContext(ctx, flags.Args(), false, false, nil)
		if err != nil {
			return err
		}
		token := make([]byte, 32)
		if _, err = rand.Read(token); err != nil {
			return err
		}
		invite = Invitation{Version: 1, Transport: "quic/udp", Capability: "flock1:" + hex.EncodeToString(token), Relay: *relay, Expires: time.Now().Add(*timeout).Unix(), Files: []File{}, Directories: []string{}}
		for _, f := range files {
			relative, err := safePath(path.Join(f.FolderRemote, f.Name))
			if err != nil || f.Symlink != "" || f.Mode&os.ModeSymlink != 0 {
				return errors.New("agent transfers require regular files and directories")
			}
			digest, size, err := hashFile(ctx, filepath.Join(f.FolderSource, f.Name))
			if err != nil {
				return err
			}
			invite.Files = append(invite.Files, File{relative, size, digest})
		}
		for _, f := range folders {
			relative, err := safePath(f.FolderRemote)
			if err != nil {
				return err
			}
			invite.Directories = append(invite.Directories, relative)
		}
	} else {
		if flags.NArg() != 0 || *out == "" {
			return errors.New("receive requires --out NEW_DIRECTORY and an invitation on stdin")
		}
		if *inviteFile != "" {
			f, err := os.Open(*inviteFile)
			if err != nil {
				return err
			}
			defer f.Close()
			input = f
		}
		var envelope struct {
			Invite *Invitation `json:"invite"`
			Invitation
		}
		decoded := make(chan error, 1)
		go func() { decoded <- json.NewDecoder(io.LimitReader(input, maxInvite+1)).Decode(&envelope) }()
		select {
		case err := <-decoded:
			if err != nil {
				return errors.New("invalid invitation JSON")
			}
		case <-ctx.Done():
			return ctx.Err()
		}
		invite = envelope.Invitation
		if envelope.Invite != nil {
			invite = *envelope.Invite
		}
	}
	if err := invite.validate(); err != nil {
		return err
	}
	payload, _ := json.Marshal(invite)
	if len(payload) > maxInvite {
		return errors.New("invitation exceeds 4 MiB")
	}
	if command == "receive" {
		absolute, err := filepath.Abs(*out)
		if err != nil {
			return err
		}
		if err = os.Mkdir(absolute, 0o700); err != nil {
			return fmt.Errorf("receive directory must be new: %w", err)
		}
		previous, err := os.Getwd()
		if err != nil {
			return err
		}
		if err = os.Chdir(absolute); err != nil {
			return err
		}
		defer os.Chdir(previous)
	}
	options := croc.Options{IsSender: command == "send", SharedSecret: invite.Capability, RelayAddress: invite.Relay, RelayPassword: password, NoPrompt: true, DisableLocal: true, DisableClipboard: true, IgnoreStdin: true, NoCompress: true, Curve: "p256", HashAlgorithm: "xxhash", Agent: true, OnProgress: e.progress, OnTransport: func(status string) { e.emit("transport", map[string]any{"transport": "quic/udp", "status": status}) },
		OnOffer: func(offered, empty []croc.FileInfo) error { return matchOffer(invite, offered, empty) }}
	client, err := croc.NewQUICCtx(ctx, options, croc.QUICConfig{Listen: *udpListen, Advertise: *advertise, Streams: *streams})
	if err != nil {
		return redact.Error(err, invite.Capability)
	}
	defer client.Cancel()
	if command == "send" {
		e.emit("invitation", map[string]any{"invite": invite})
		err = client.Send(files, folders, folderCount)
	} else {
		e.emit("connecting", map[string]any{"transport": "quic/udp"})
		err = client.Receive()
		if err == nil {
			for _, file := range invite.Files {
				digest, size, hashErr := hashFile(ctx, filepath.FromSlash(file.Path))
				if hashErr != nil {
					return redact.Error(hashErr, invite.Capability)
				}
				if digest != file.SHA256 || size != file.Size {
					return errors.New("received file SHA-256 or size mismatch")
				}
			}
			for _, dir := range invite.Directories {
				info, err := os.Lstat(filepath.FromSlash(dir))
				if err != nil || !info.IsDir() {
					return errors.New("received empty directory missing or changed")
				}
			}
		}
	}
	if err != nil {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		return redact.Error(err, invite.Capability)
	}
	var total int64
	for _, file := range invite.Files {
		total += file.Size
	}
	e.emit("completed", map[string]any{"transport": "quic/udp", "role": command, "bytes": total, "files": invite.Files, "directories": invite.Directories, "sha256_verified": command == "receive"})
	return nil
}
func (i Invitation) validate() error {
	if i.Version != 1 || i.Transport != "quic/udp" || !strings.HasPrefix(i.Capability, "flock1:") || i.Expires <= time.Now().Unix() || len(i.Files)+len(i.Directories) > 10000 {
		return errors.New("invalid or expired Flock invitation")
	}
	if _, err := codephrase.Parse(i.Capability); err != nil {
		return errors.New("invalid Flock capability")
	}
	host, port, err := net.SplitHostPort(i.Relay)
	if err != nil || host == "" || port == "" {
		return errors.New("invalid control relay")
	}
	seen := map[string]bool{}
	for _, f := range i.Files {
		name, err := safePath(f.Path)
		digest, e := hex.DecodeString(f.SHA256)
		if err != nil || name != f.Path || f.Size < 0 || e != nil || len(digest) != 32 || seen[strings.ToLower(name)] {
			return errors.New("invalid invitation file manifest")
		}
		seen[strings.ToLower(name)] = true
	}
	for _, d := range i.Directories {
		name, err := safePath(d)
		if err != nil || name != d || seen[strings.ToLower(name)] {
			return errors.New("invalid invitation directory manifest")
		}
		seen[strings.ToLower(name)] = true
	}
	return nil
}
func safePath(name string) (string, error) {
	clean := path.Clean(name)
	if clean == "." || len(clean) > 4096 || strings.ContainsAny(clean, "\\:\x00") || !filepath.IsLocal(filepath.FromSlash(clean)) {
		return "", errors.New("invalid relative transfer path")
	}
	return clean, nil
}
func hashFile(ctx context.Context, name string) (string, int64, error) {
	info, err := os.Lstat(name)
	if err != nil {
		return "", 0, err
	}
	if !info.Mode().IsRegular() {
		return "", 0, errors.New("transfer entry is not a regular file")
	}
	f, err := os.Open(name)
	if err != nil {
		return "", 0, err
	}
	defer f.Close()
	digest := sha256.New()
	buffer := make([]byte, 1<<20)
	var count int64
	for {
		if err = ctx.Err(); err != nil {
			return "", 0, err
		}
		n, readErr := f.Read(buffer)
		if n > 0 {
			digest.Write(buffer[:n])
			count += int64(n)
		}
		if readErr == io.EOF {
			break
		}
		if readErr != nil {
			return "", 0, readErr
		}
	}
	after, err := f.Stat()
	if err != nil || after.Size() != info.Size() || !after.ModTime().Equal(info.ModTime()) || count != info.Size() {
		return "", 0, errors.New("source changed while hashing")
	}
	return hex.EncodeToString(digest.Sum(nil)), count, nil
}
func matchOffer(i Invitation, files, dirs []croc.FileInfo) error {
	if len(files) != len(i.Files) || len(dirs) != len(i.Directories) {
		return errors.New("peer manifest differs from invitation")
	}
	expected := map[string]File{}
	for _, f := range i.Files {
		expected[f.Path] = f
	}
	for _, f := range files {
		name, err := safePath(path.Join(f.FolderRemote, f.Name))
		want, ok := expected[name]
		if err != nil || !ok || f.Size != want.Size || f.Symlink != "" || f.Mode&os.ModeSymlink != 0 {
			return errors.New("peer file differs from invitation")
		}
		delete(expected, name)
	}
	empty := map[string]bool{}
	for _, d := range i.Directories {
		empty[d] = true
	}
	for _, d := range dirs {
		name, err := safePath(d.FolderRemote)
		if err != nil || !empty[name] {
			return errors.New("peer directory differs from invitation")
		}
		delete(empty, name)
	}
	return nil
}
