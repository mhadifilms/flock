package flockcli

import (
	"bufio"
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// Separate processes exercise the public JSON interface and isolate Croc's cwd.
func TestFlockProcess(t *testing.T) {
	if os.Getenv("FLOCK_TEST_PROCESS") != "1" {
		return
	}
	for i, arg := range os.Args {
		if arg == "--" {
			os.Exit(Run(context.Background(), os.Args[i+1:], os.Stdin, os.Stdout, os.Stderr))
		}
	}
	os.Exit(2)
}

type process struct {
	cmd    *exec.Cmd
	events chan map[string]json.RawMessage
	done   chan error
	stderr bytes.Buffer
}

func startProcess(t *testing.T, ctx context.Context, input []byte, args ...string) *process {
	t.Helper()
	p := &process{events: make(chan map[string]json.RawMessage, 256), done: make(chan error, 1)}
	p.cmd = exec.CommandContext(ctx, os.Args[0], append([]string{"-test.run=^TestFlockProcess$", "--"}, args...)...)
	p.cmd.Env = append(os.Environ(), "FLOCK_TEST_PROCESS=1", "FLOCK_RELAY_PASSWORD=flock-test")
	p.cmd.Stdin = bytes.NewReader(input)
	p.cmd.Stderr = &p.stderr
	stdout, err := p.cmd.StdoutPipe()
	if err != nil {
		t.Fatal(err)
	}
	if err = p.cmd.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if p.cmd.Process != nil {
			p.cmd.Process.Kill()
		}
	})
	go func() {
		scanner := bufio.NewScanner(stdout)
		scanner.Buffer(make([]byte, 4096), maxInvite+4096)
		var parseErr error
		for scanner.Scan() {
			var event map[string]json.RawMessage
			if err := json.Unmarshal(scanner.Bytes(), &event); err != nil {
				parseErr = fmt.Errorf("stdout contained non-JSON: %w", err)
				continue
			}
			p.events <- event
		}
		if scanner.Err() != nil {
			parseErr = scanner.Err()
		}
		close(p.events)
		err := p.cmd.Wait()
		if parseErr != nil {
			err = parseErr
		}
		p.done <- err
	}()
	return p
}
func eventName(e map[string]json.RawMessage) string {
	var name string
	json.Unmarshal(e["event"], &name)
	return name
}
func nextEvent(t *testing.T, ctx context.Context, p *process, name string) map[string]json.RawMessage {
	t.Helper()
	for {
		select {
		case e, ok := <-p.events:
			if !ok {
				t.Fatalf("missing %s event (exit %v): %s", name, <-p.done, p.stderr.String())
			}
			if eventName(e) == "error" {
				t.Fatalf("unexpected error event: %s; stderr: %s", e["message"], p.stderr.String())
			}
			if eventName(e) == name {
				return e
			}
		case <-ctx.Done():
			t.Fatalf("waiting for %s: %v", name, ctx.Err())
		}
	}
}
func waitSuccess(t *testing.T, ctx context.Context, p *process) {
	t.Helper()
	select {
	case err := <-p.done:
		if err != nil {
			t.Fatalf("process failed: %v: %s", err, p.stderr.String())
		}
	case <-ctx.Done():
		t.Fatal(ctx.Err())
	}
}

func TestAgentQUICTransfer(t *testing.T) {
	for _, streams := range []string{"1", "8"} {
		t.Run("streams="+streams, func(t *testing.T) {
			ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
			defer cancel()
			relay := startProcess(t, ctx, nil, "relay", "--listen", "127.0.0.1:0")
			ready := nextEvent(t, ctx, relay, "ready")
			var address string
			json.Unmarshal(ready["address"], &address)
			root := t.TempDir()
			source := filepath.Join(root, "payload")
			os.MkdirAll(filepath.Join(source, "nested", "empty"), 0700)
			// More than one chunk per stream, non-aligned, not compressible by repeated text.
			data := make([]byte, (9<<20)+37)
			for i := range data {
				data[i] = byte((i*73 + i/997) % 251)
			}
			if err := os.WriteFile(filepath.Join(source, "data.bin"), data, 0600); err != nil {
				t.Fatal(err)
			}
			os.WriteFile(filepath.Join(source, "nested", "zero"), nil, 0600)
			sender := startProcess(t, ctx, nil, "send", "--relay", address, "--advertise", "127.0.0.1", "--streams", streams, source)
			invitation := nextEvent(t, ctx, sender, "invitation")
			var invite Invitation
			if err := json.Unmarshal(invitation["invite"], &invite); err != nil {
				t.Fatal(err)
			}
			if !strings.HasPrefix(invite.Capability, "flock1:") || len(invite.Capability) != 71 {
				t.Fatal("capability must have 256 random bits")
			}
			wire, _ := json.Marshal(invitation)
			receiver := startProcess(t, ctx, wire, "receive", "--out", filepath.Join(root, "received"))
			result := nextEvent(t, ctx, receiver, "completed")
			var verified bool
			json.Unmarshal(result["sha256_verified"], &verified)
			if !verified {
				t.Fatal("missing receiver SHA-256 verification")
			}
			nextEvent(t, ctx, sender, "completed")
			waitSuccess(t, ctx, receiver)
			waitSuccess(t, ctx, sender)
			got, err := os.ReadFile(filepath.Join(root, "received", "payload", "data.bin"))
			if err != nil {
				t.Fatal(err)
			}
			if !bytes.Equal(data, got) {
				t.Fatal("received bytes differ")
			}
			for _, name := range []string{"nested/empty", "nested/zero"} {
				if _, err := os.Stat(filepath.Join(root, "received", "payload", name)); err != nil {
					t.Fatal(err)
				}
			}
		})
	}
}
func TestAgentFailuresAreJSON(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	for _, args := range [][]string{{"receive", "--out", filepath.Join(t.TempDir(), "new")}, {"send"}, {"unknown"}} {
		p := startProcess(t, ctx, []byte(`{"version":1}`), args...)
		count := 0
		for e := range p.events {
			if eventName(e) != "error" {
				t.Fatalf("unexpected event %s", eventName(e))
			}
			count++
		}
		if err := <-p.done; err == nil || count != 1 {
			t.Fatalf("expected one JSON error and failure: %v %d", err, count)
		}
	}
}
func TestInvitationRejectsUnsafeManifest(t *testing.T) {
	sum := sha256.Sum256([]byte("data"))
	valid := Invitation{Version: 1, Transport: "quic/udp", Capability: "flock1:" + strings.Repeat("12", 32), Relay: "127.0.0.1:9009", Expires: time.Now().Add(time.Minute).Unix(), Files: []File{{Path: "data", Size: 4, SHA256: hex.EncodeToString(sum[:])}}}
	if err := valid.validate(); err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"../escape", "/absolute", "a\\b", "a:b", ".", "a/../data"} {
		bad := valid
		bad.Files = []File{{Path: name, Size: 4, SHA256: valid.Files[0].SHA256}}
		if bad.validate() == nil {
			t.Fatalf("accepted unsafe path %q", name)
		}
	}
	bad := valid
	bad.Expires = time.Now().Add(-time.Minute).Unix()
	if bad.validate() == nil {
		t.Fatal("accepted expired invite")
	}
	bad = valid
	bad.Files = append(append([]File{}, valid.Files...), File{Path: "DATA", Size: 4, SHA256: valid.Files[0].SHA256})
	if bad.validate() == nil {
		t.Fatal("accepted duplicate case-insensitive path")
	}
	bad = valid
	bad.Capability = "flock1:secret"
	if bad.validate() == nil {
		t.Fatal("accepted short capability")
	}
}

func TestAgentBlockedUDPDoesNotFallBack(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	relay := startProcess(t, ctx, nil, "relay", "--listen", "127.0.0.1:0")
	ready := nextEvent(t, ctx, relay, "ready")
	var address string
	json.Unmarshal(ready["address"], &address)
	source := filepath.Join(t.TempDir(), "source.bin")
	os.WriteFile(source, []byte("UDP must connect"), 0600)
	// Bind only 127.0.0.1 but advertise a different loopback address: no reachable UDP listener.
	sender := startProcess(t, ctx, nil, "send", "--relay", address, "--udp-listen", "127.0.0.1:0", "--advertise", "127.0.0.2", "--timeout", "2s", source)
	invite := nextEvent(t, ctx, sender, "invitation")
	wire, _ := json.Marshal(invite)
	receiver := startProcess(t, ctx, wire, "receive", "--out", filepath.Join(t.TempDir(), "new"), "--timeout", "2s")
	for _, p := range []*process{sender, receiver} {
		errors := 0
		for e := range p.events {
			if eventName(e) == "completed" {
				t.Fatal("UDP failure silently completed")
			}
			if eventName(e) == "error" {
				errors++
			}
		}
		if err := <-p.done; err == nil || errors != 1 {
			t.Fatalf("UDP failure exit: %v; error events %d", err, errors)
		}
	}
}
func TestReceiveDeadlineIncludesInvitationInput(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	reader, writer := io.Pipe()
	defer reader.Close()
	defer writer.Close()
	var stdout bytes.Buffer
	status := Run(ctx, []string{"receive", "--out", filepath.Join(t.TempDir(), "new"), "--timeout", "20ms"}, reader, &stdout, io.Discard)
	if status != 124 {
		t.Fatalf("stdin deadline status %d: %s", status, stdout.String())
	}
	var event struct{ Event, Code string }
	if err := json.Unmarshal(stdout.Bytes(), &event); err != nil || event.Event != "error" || event.Code != "timeout" {
		t.Fatalf("invalid timeout event: %s", stdout.String())
	}
}

func TestAgentEmbeddedRelay(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	root := t.TempDir()
	source := filepath.Join(root, "automatic.bin")
	os.WriteFile(source, []byte("two processes, native UDP"), 0600)
	sender := startProcess(t, ctx, nil, "send", "--advertise", "127.0.0.1", source)
	invite := nextEvent(t, ctx, sender, "invitation")
	wire, _ := json.Marshal(invite)
	receiver := startProcess(t, ctx, wire, "receive", "--out", filepath.Join(root, "new"))
	nextEvent(t, ctx, receiver, "completed")
	nextEvent(t, ctx, sender, "completed")
	waitSuccess(t, ctx, receiver)
	waitSuccess(t, ctx, sender)
	got, err := os.ReadFile(filepath.Join(root, "new", "automatic.bin"))
	if err != nil || string(got) != "two processes, native UDP" {
		t.Fatal("embedded relay transfer failed")
	}
}

func TestAgentRejectsChangedManifestAndChecksum(t *testing.T) {
	for _, change := range []string{"size", "checksum"} {
		t.Run(change, func(t *testing.T) {
			ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
			defer cancel()
			root := t.TempDir()
			source := filepath.Join(root, "data.bin")
			data := []byte("the sender's original bytes")
			if change == "checksum" {
				data = bytes.Repeat(data, (9<<20)/len(data)+1)
			}
			os.WriteFile(source, data, 0600)
			sender := startProcess(t, ctx, nil, "send", "--advertise", "127.0.0.1", source)
			event := nextEvent(t, ctx, sender, "invitation")
			var invite Invitation
			json.Unmarshal(event["invite"], &invite)
			if change == "size" {
				invite.Files[0].Size++
			} else {
				invite.Files[0].SHA256 = strings.Repeat("0", 64)
			}
			wire, _ := json.Marshal(invite)
			receiver := startProcess(t, ctx, wire, "receive", "--out", filepath.Join(root, "new"))
			errors := 0
			for e := range receiver.events {
				if eventName(e) == "completed" {
					t.Fatal("changed manifest reported success")
				}
				if eventName(e) == "error" {
					errors++
					if strings.Contains(string(e["message"]), invite.Capability) {
						t.Fatal("error exposed capability")
					}
				}
			}
			if err := <-receiver.done; err == nil || errors != 1 {
				t.Fatalf("changed manifest exit %v, errors %d", err, errors)
			}
			for range sender.events {
			}
			<-sender.done
		})
	}
}
