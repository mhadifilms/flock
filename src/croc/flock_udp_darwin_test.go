//go:build darwin

package croc

import (
	"bytes"
	"encoding/binary"
	"errors"
	"net"
	"os"
	"sync"
	"sync/atomic"
	"syscall"
	"testing"
	"time"
	"unsafe"

	"golang.org/x/net/ipv4"
	"golang.org/x/sys/unix"
)

func batchSocket(t *testing.T) *darwinPacketConn {
	t.Helper()
	socket, err := net.ListenUDP("udp4", &net.UDPAddr{IP: net.IPv4(127, 0, 0, 1)})
	if err != nil {
		t.Fatal(err)
	}
	conn, ok := flockPacketConn(socket).(*darwinPacketConn)
	if !ok {
		socket.Close()
		t.Skip("native UDP batching is unavailable")
	}
	t.Cleanup(func() { conn.Close() })
	if err = conn.SetDeadline(time.Now().Add(5 * time.Second)); err != nil {
		t.Fatal(err)
	}
	return conn
}
func batchMessage(size int) []ipv4.Message {
	return []ipv4.Message{{Buffers: [][]byte{make([]byte, size)}, OOB: make([]byte, 128)}}
}
func TestFlockDarwinBatchABI(t *testing.T) {
	var m darwinMessage
	if unsafe.Sizeof(m) != 56 || unsafe.Offsetof(m.iov) != 16 || unsafe.Offsetof(m.control) != 32 || unsafe.Offsetof(m.length) != 48 {
		t.Fatal("msghdr_x ABI mismatch")
	}
}
func TestFlockDarwinPackets(t *testing.T) {
	a, b := batchSocket(t), batchSocket(t)
	if err := a.Pair(b.LocalAddr().(*net.UDPAddr)); err != nil {
		t.Fatal(err)
	}
	if err := b.Pair(a.LocalAddr().(*net.UDPAddr)); err != nil {
		t.Fatal(err)
	}
	if err := a.Pair(b.LocalAddr().(*net.UDPAddr)); err == nil {
		t.Fatal("paired twice")
	}
	foreign := &net.UDPAddr{IP: net.IPv4(127, 0, 0, 1), Port: 1}
	if _, _, err := a.WriteMsgUDP([]byte{1}, nil, foreign); !errors.Is(err, net.ErrWriteToConnected) {
		t.Fatal(err)
	}
	if _, err := a.WriteTo([]byte{1}, foreign); !errors.Is(err, net.ErrWriteToConnected) {
		t.Fatal(err)
	}
	for i := range 32 {
		payload := bytes.Repeat([]byte{byte(i)}, 1280)
		if _, _, err := a.WriteMsgUDP(payload, nil, b.LocalAddr().(*net.UDPAddr)); err != nil {
			t.Fatal(err)
		}
		clear(payload) // The writer must own a copy after WriteMsgUDP returns.
	}
	messages := batchMessage(1500)
	for i := range 32 {
		if n, err := b.ReadBatch(messages, 0); err != nil || n != 1 {
			t.Fatalf("read %d: %v", n, err)
		}
		if !bytes.Equal(messages[0].Buffers[0][:messages[0].N], bytes.Repeat([]byte{byte(i)}, 1280)) {
			t.Fatalf("packet %d mismatch", i)
		}
		if messages[0].Addr.String() != a.LocalAddr().String() {
			t.Fatal(messages[0].Addr)
		}
	}
	// Exercise an empty kernel queue after consuming a prefetched batch. Deadline
	// failures must repeat correctly instead of returning stale packets or spinning.
	b.SetReadDeadline(time.Now().Add(10 * time.Millisecond))
	for range 2 {
		if _, err := b.ReadBatch(messages, 0); !errors.Is(err, os.ErrDeadlineExceeded) {
			t.Fatalf("deadline: %v", err)
		}
	}
	b.SetReadDeadline(time.Now().Add(time.Second))
	if _, err := a.WriteTo([]byte("probe"), b.LocalAddr()); err != nil {
		t.Fatal(err)
	}
	if _, err := b.ReadBatch(messages, 0); err != nil || string(messages[0].Buffers[0][:messages[0].N]) != "probe" {
		t.Fatal("deadline recovery", err)
	}
}
func TestFlockDarwinFallbackAndTruncation(t *testing.T) {
	a, b := batchSocket(t), batchSocket(t)
	// Before pairing ordinary addressed UDP writes must continue to work.
	if _, _, err := a.WriteMsgUDP([]byte("unpaired"), nil, b.LocalAddr().(*net.UDPAddr)); err != nil {
		t.Fatal(err)
	}
	messages := batchMessage(4)
	if _, err := b.ReadBatch(messages, 0); err != nil || messages[0].Flags&unix.MSG_TRUNC == 0 {
		t.Fatal("missing truncation", err)
	}
	b.readFallback = true
	if _, err := a.WriteTo([]byte("fallback"), b.LocalAddr()); err != nil {
		t.Fatal(err)
	}
	messages = batchMessage(32)
	if _, err := b.ReadBatch(messages, 0); err != nil || string(messages[0].Buffers[0][:messages[0].N]) != "fallback" {
		t.Fatal("fallback failed", err)
	}
}
func TestFlockDarwinCloseUnblocks(t *testing.T) {
	a := batchSocket(t)
	result := make(chan error, 1)
	go func() { _, err := a.ReadBatch(batchMessage(32), 0); result <- err }()
	a.Close()
	select {
	case err := <-result:
		if !errors.Is(err, net.ErrClosed) {
			t.Fatal(err)
		}
	case <-time.After(time.Second):
		t.Fatal("read blocked after close")
	}
	if _, _, err := a.WriteMsgUDP([]byte{1}, nil, &net.UDPAddr{IP: net.IPv4(127, 0, 0, 1), Port: 1}); !errors.Is(err, net.ErrClosed) {
		t.Fatal(err)
	}
}

func TestFlockDarwinNativeErrno(t *testing.T) {
	c := batchSocket(t)
	messages := []darwinMessage{{}}
	for _, send := range []bool{false, true} {
		if _, err := c.nativeCall(send, ^uintptr(0), messages); !errors.Is(err, unix.EBADF) {
			t.Fatalf("native errno: %v", err)
		}
	}
}
func TestFlockDarwinECN(t *testing.T) {
	a, b := batchSocket(t), batchSocket(t)
	if err := b.raw.Control(func(fd uintptr) {
		if err := unix.SetsockoptInt(int(fd), unix.IPPROTO_IP, unix.IP_RECVTOS, 1); err != nil {
			t.Error(err)
		}
	}); err != nil {
		t.Fatal(err)
	}
	if err := a.Pair(b.LocalAddr().(*net.UDPAddr)); err != nil {
		t.Fatal(err)
	}
	for _, tos := range []byte{0, 2, 1, 0} {
		oob := make([]byte, unix.CmsgSpace(4))
		header := (*unix.Cmsghdr)(unsafe.Pointer(&oob[0]))
		header.Level = unix.IPPROTO_IP
		header.Type = unix.IP_TOS
		header.SetLen(unix.CmsgLen(4))
		binary.NativeEndian.PutUint32(oob[unix.CmsgLen(0):], uint32(tos))
		if _, _, err := a.WriteMsgUDP([]byte{tos}, oob, b.LocalAddr().(*net.UDPAddr)); err != nil {
			t.Fatal(err)
		}
	}
	for _, tos := range []byte{0, 2, 1, 0} {
		messages := batchMessage(32)
		if _, err := b.ReadBatch(messages, 0); err != nil {
			t.Fatal(err)
		}
		controls, err := unix.ParseSocketControlMessage(messages[0].OOB[:messages[0].NN])
		if err != nil {
			t.Fatal(err)
		}
		found := false
		for _, control := range controls {
			if control.Header.Level == unix.IPPROTO_IP && control.Header.Type == unix.IP_RECVTOS {
				found = true
				if len(control.Data) != 1 || control.Data[0] != tos {
					t.Fatalf("ECN mismatch: %v, want %d", control.Data, tos)
				}
			}
		}
		if !found {
			t.Fatal("missing received ECN")
		}
	}
}

type blockedBatchWriter struct {
	syscall.RawConn
	blocked chan struct{}
	done    <-chan struct{}
	once    sync.Once
}

func (w *blockedBatchWriter) Write(func(uintptr) bool) error {
	w.once.Do(func() { close(w.blocked) })
	<-w.done
	return net.ErrClosed
}
func TestFlockDarwinCloseFullQueue(t *testing.T) {
	a, b := batchSocket(t), batchSocket(t)
	if err := a.Pair(b.LocalAddr().(*net.UDPAddr)); err != nil {
		t.Fatal(err)
	}
	blocked := &blockedBatchWriter{RawConn: a.raw, blocked: make(chan struct{}), done: a.done}
	a.raw = blocked
	if _, _, err := a.WriteMsgUDP([]byte{1}, nil, b.LocalAddr().(*net.UDPAddr)); err != nil {
		t.Fatal(err)
	}
	<-blocked.blocked
	for i := 0; i < cap(a.outgoing); i++ {
		if _, _, err := a.WriteMsgUDP([]byte{1}, nil, b.LocalAddr().(*net.UDPAddr)); err != nil {
			t.Fatal(err)
		}
	}
	enqueued := make(chan error, 1)
	go func() { _, _, err := a.WriteMsgUDP([]byte{1}, nil, b.LocalAddr().(*net.UDPAddr)); enqueued <- err }()
	closed := make(chan struct{})
	go func() { a.Close(); close(closed) }()
	select {
	case <-closed:
	case <-time.After(time.Second):
		t.Fatal("close blocked behind full queue")
	}
	if err := <-enqueued; !errors.Is(err, net.ErrClosed) {
		t.Fatal(err)
	}
	if len(a.outgoing) != 0 {
		t.Fatal("queued buffers retained after close")
	}
}

func TestFlockDarwinSendBufferBackpressure(t *testing.T) {
	a, b := batchSocket(t), batchSocket(t)
	if err := a.Pair(b.LocalAddr().(*net.UDPAddr)); err != nil {
		t.Fatal(err)
	}
	var calls atomic.Int32
	a.sendBatch = func(fd uintptr, messages []darwinMessage) (int, error) {
		switch calls.Add(1) {
		case 1:
			n, err := a.nativeCall(true, fd, messages[:1])
			if err != nil {
				return n, err
			}
			return n, unix.ENOBUFS
		case 2:
			return 0, unix.ENOBUFS
		case 3:
			return 0, unix.EINTR
		default:
			return a.nativeCall(true, fd, messages)
		}
	}
	for i := range 32 {
		if _, _, err := a.WriteMsgUDP([]byte{byte(i)}, nil, b.LocalAddr().(*net.UDPAddr)); err != nil {
			t.Fatal(err)
		}
	}
	for i := range 32 {
		messages := batchMessage(32)
		if _, err := b.ReadBatch(messages, 0); err != nil {
			t.Fatal(err)
		}
		if messages[0].N != 1 || messages[0].Buffers[0][0] != byte(i) {
			t.Fatalf("partial retry duplicated/lost packet %d", i)
		}
	}
	if calls.Load() < 4 {
		t.Fatal("did not exercise retries")
	}
}
func TestFlockDarwinCancelSendBufferRetry(t *testing.T) {
	a, b := batchSocket(t), batchSocket(t)
	if err := a.Pair(b.LocalAddr().(*net.UDPAddr)); err != nil {
		t.Fatal(err)
	}
	entered := make(chan struct{})
	var once sync.Once
	a.sendBatch = func(uintptr, []darwinMessage) (int, error) {
		once.Do(func() { close(entered) })
		return 0, unix.ENOBUFS
	}
	if _, _, err := a.WriteMsgUDP([]byte{1}, nil, b.LocalAddr().(*net.UDPAddr)); err != nil {
		t.Fatal(err)
	}
	<-entered
	closed := make(chan struct{})
	go func() { a.Close(); close(closed) }()
	select {
	case <-closed:
	case <-time.After(time.Second):
		t.Fatal("buffer retry did not cancel")
	}
}

func TestFlockDarwinOversizedProbeDoesNotClose(t *testing.T) {
	a, b := batchSocket(t), batchSocket(t)
	if err := a.Pair(b.LocalAddr().(*net.UDPAddr)); err != nil {
		t.Fatal(err)
	}
	entered := make(chan struct{})
	var calls atomic.Int32
	a.sendBatch = func(fd uintptr, messages []darwinMessage) (int, error) {
		if calls.Add(1) == 1 {
			close(entered)
			return 0, unix.EMSGSIZE
		}
		return a.nativeCall(true, fd, messages)
	}
	if _, _, err := a.WriteMsgUDP([]byte("probe"), nil, b.LocalAddr().(*net.UDPAddr)); err != nil {
		t.Fatal(err)
	}
	<-entered
	if _, _, err := a.WriteMsgUDP([]byte("data"), nil, b.LocalAddr().(*net.UDPAddr)); err != nil {
		t.Fatal(err)
	}
	messages := batchMessage(32)
	if _, err := b.ReadBatch(messages, 0); err != nil || string(messages[0].Buffers[0][:messages[0].N]) != "data" {
		t.Fatal("oversized probe closed or corrupted connection", err)
	}
}
