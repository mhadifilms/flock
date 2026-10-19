//go:build darwin

package croc

import (
	"encoding/binary"
	"errors"
	"io"
	"net"
	"os"
	"runtime"
	"sync"
	"sync/atomic"
	"syscall"
	"time"
	"unsafe"

	"github.com/ebitengine/purego"
	"golang.org/x/net/ipv4"
	"golang.org/x/sys/unix"
)

const udpBatchSize = 32

// Darwin's extended message ABI (64-bit). Calls never retain Go pointers.
// https://github.com/apple-oss-distributions/xnu/blob/main/bsd/sys/socket_private.h
// sendmsg_x requires a connected socket and no per-message ancillary data.
type darwinMessage struct {
	name          unsafe.Pointer
	nameLength    uint32
	iov           *unix.Iovec
	iovLength     int32
	control       unsafe.Pointer
	controlLength uint32
	flags         int32
	length        uintptr
}

type batchFunctions struct{ send, receive uintptr }

var loadBatch = sync.OnceValue(func() *batchFunctions {
	handle, err := purego.Dlopen("/usr/lib/libSystem.B.dylib", purego.RTLD_NOW|purego.RTLD_LOCAL)
	if err != nil {
		return nil
	}
	send, err := purego.Dlsym(handle, "sendmsg_x")
	if err != nil {
		return nil
	}
	receive, err := purego.Dlsym(handle, "recvmsg_x")
	if err != nil {
		return nil
	}
	return &batchFunctions{send: send, receive: receive}
})

type udpPacket struct {
	data      [2048]byte
	oob       [256]byte
	n, nn     int
	tos       int
	batchable bool
}

var udpPackets = sync.Pool{New: func() any { return new(udpPacket) }}

type darwinPacketConn struct {
	*net.UDPConn
	raw       syscall.RawConn
	functions *batchFunctions
	sendBatch func(uintptr, []darwinMessage) (int, error)
	peer      atomic.Pointer[net.UDPAddr]
	pairing   sync.Mutex
	done      chan struct{}
	closeOnce sync.Once
	stopOnce  sync.Once
	closeErr  error
	outgoing  chan *udpPacket
	writeDone chan struct{}
	failureMu sync.Mutex
	failure   error
	// ReadBatch is called by quic-go's single socket reader.
	readMu        sync.Mutex
	received      int
	next          int
	readBuffers   [udpBatchSize][65536]byte
	readOOB       [udpBatchSize][256]byte
	readAddresses [udpBatchSize]unix.RawSockaddrInet4
	readVectors   [udpBatchSize]unix.Iovec
	readMessages  [udpBatchSize]darwinMessage
	readFallback  bool
}

func flockPacketConn(socket *net.UDPConn) net.PacketConn {
	if os.Getenv("FLOCK_DISABLE_UDP_BATCH") == "1" {
		return socket
	}
	functions := loadBatch()
	if functions == nil {
		return socket
	}
	raw, err := socket.SyscallConn()
	if err != nil {
		return socket
	}
	c := &darwinPacketConn{UDPConn: socket, raw: raw, functions: functions, done: make(chan struct{}), outgoing: make(chan *udpPacket, 64), writeDone: make(chan struct{})}
	c.sendBatch = func(fd uintptr, messages []darwinMessage) (int, error) { return c.nativeCall(true, fd, messages) }
	go c.writeLoop()

	return c
}
func (c *darwinPacketConn) Pair(peer *net.UDPAddr) error {
	ip := peer.IP.To4()
	if ip == nil {
		return errors.New("native batching requires IPv4")
	}
	c.pairing.Lock()
	defer c.pairing.Unlock()
	if previous := c.peer.Load(); previous != nil {
		return errors.New("UDP socket is already paired")
	}
	address := &unix.SockaddrInet4{Port: peer.Port}
	copy(address.Addr[:], ip)
	var pairErr error
	err := c.raw.Control(func(fd uintptr) { pairErr = unix.Connect(int(fd), address) })
	if err != nil {
		return err
	}
	if pairErr != nil {
		return pairErr
	}
	c.peer.Store(&net.UDPAddr{IP: append(net.IP(nil), ip...), Port: peer.Port})
	return nil
}
func (c *darwinPacketConn) stopSocket() {
	c.stopOnce.Do(func() { close(c.done); c.closeErr = c.UDPConn.Close() })
}
func (c *darwinPacketConn) Close() error {
	c.closeOnce.Do(func() {
		c.stopSocket()
		// Wait for an enqueuer and the writer after closing the socket, so a full
		// queue or a blocked kernel write cannot prevent cancellation.
		c.pairing.Lock()
		c.pairing.Unlock()
		<-c.writeDone
		for {
			select {
			case packet := <-c.outgoing:
				udpPackets.Put(packet)
			default:
				return
			}
		}
	})
	return c.closeErr
}
func (c *darwinPacketConn) WriteTo(data []byte, address net.Addr) (int, error) {
	peer := c.peer.Load()
	if peer == nil {
		return c.UDPConn.WriteTo(data, address)
	}
	target, ok := address.(*net.UDPAddr)
	if !ok || target.Port != peer.Port || !target.IP.Equal(peer.IP) {
		return 0, net.ErrWriteToConnected
	}
	return c.sendOne(data, nil)
}

// A saturated local UDP queue is backpressure, not a broken authenticated
// session. Bound retries by socket cancellation instead of spinning on a
// descriptor that the kernel still reports writable.
func (c *darwinPacketConn) waitForSendBuffer() error {
	timer := time.NewTimer(time.Millisecond)
	defer timer.Stop()
	select {
	case <-c.done:
		return net.ErrClosed
	case <-timer.C:
		return nil
	}
}
func (c *darwinPacketConn) sendOne(data, oob []byte) (int, error) {
	n := 0
	var sendErr error
	err := c.raw.Write(func(fd uintptr) bool {
		for {
			n, sendErr = unix.SendmsgN(int(fd), data, oob, nil, unix.MSG_DONTWAIT)
			if sendErr == unix.ENOBUFS {
				if err := c.waitForSendBuffer(); err != nil {
					sendErr = err
					break
				}
				continue
			}
			if sendErr != unix.EINTR {
				break
			}
		}
		return sendErr != unix.EAGAIN && sendErr != unix.EWOULDBLOCK
	})
	if err != nil {
		return n, err
	}
	return n, sendErr
}
func (c *darwinPacketConn) WriteMsgUDP(data, oob []byte, address *net.UDPAddr) (int, int, error) {
	peer := c.peer.Load()
	if peer == nil {
		c.pairing.Lock()
		peer = c.peer.Load()
		if peer == nil {
			n, nn, err := c.UDPConn.WriteMsgUDP(data, oob, address)
			c.pairing.Unlock()
			return n, nn, err
		}
		c.pairing.Unlock()
	}
	if address == nil || address.Port != peer.Port || !address.IP.Equal(peer.IP) {
		return 0, 0, net.ErrWriteToConnected
	}
	c.failureMu.Lock()
	err := c.failure
	c.failureMu.Unlock()
	if err != nil {
		return 0, 0, err
	}
	if len(data) > 2048 || len(oob) > 256 {
		n, err := c.sendOne(data, oob)
		return n, len(oob), err
	}
	packet := udpPackets.Get().(*udpPacket)
	packet.n = copy(packet.data[:], data)
	packet.nn = copy(packet.oob[:], oob)
	packet.tos = 0
	packet.batchable = true
	for control := oob; len(control) > 0; {
		header, body, rest, err := unix.ParseOneSocketControlMessage(control)
		if err != nil || header.Level != unix.IPPROTO_IP || header.Type != unix.IP_TOS || len(body) != 4 {
			packet.batchable = false
			break
		}
		packet.tos = int(binary.NativeEndian.Uint32(body))
		control = rest
	}
	c.pairing.Lock()
	defer c.pairing.Unlock()
	select {
	case <-c.done:
		udpPackets.Put(packet)
		return 0, 0, net.ErrClosed
	default:
	}
	select {
	case <-c.done:
		udpPackets.Put(packet)
		return 0, 0, net.ErrClosed
	case c.outgoing <- packet:
		return len(data), len(oob), nil
	}
}
func (c *darwinPacketConn) nativeCall(send bool, fd uintptr, messages []darwinMessage) (int, error) {
	fn := c.functions.receive
	if send {
		fn = c.functions.send
	}
	// Capture errno in the same native call. A second call to __error can observe
	// errno changed by the Go runtime between foreign calls.
	result, _, errno := purego.SyscallN(fn, fd, uintptr(unsafe.Pointer(&messages[0])), uintptr(len(messages)), unix.MSG_DONTWAIT)
	runtime.KeepAlive(messages)
	if int64(result) < 0 {
		return 0, syscall.Errno(uint32(errno))
	}
	if result > uintptr(len(messages)) {
		return 0, errors.New("invalid native UDP batch count")
	}
	return int(result), nil
}
func (c *darwinPacketConn) writeLoop() {
	defer close(c.writeDone)

	var packets [udpBatchSize]*udpPacket
	var vectors [udpBatchSize]unix.Iovec
	var messages [udpBatchSize]darwinMessage
	tos := -1
	fallback := false
	for {
		count := 0
		select {
		case <-c.done:
			return
		case packets[0] = <-c.outgoing:
			count = 1
		}
	drain:
		for count < len(packets) {
			select {
			case packets[count] = <-c.outgoing:
				count++
			default:
				break drain
			}
		}
		var sendErr error
		for first := 0; first < count; {
			if !packets[first].batchable || fallback {
				_, sendErr = c.sendOne(packets[first].data[:packets[first].n], packets[first].oob[:packets[first].nn])
				first++
				if errors.Is(sendErr, unix.EMSGSIZE) {
					sendErr = nil
				}
			} else {
				end := first + 1
				for end < count && packets[end].batchable && packets[end].tos == packets[first].tos {
					end++
				}
				for i := first; i < end; i++ {
					vectors[i] = unix.Iovec{Base: &packets[i].data[0], Len: uint64(packets[i].n)}
					messages[i] = darwinMessage{iov: &vectors[i], iovLength: 1}
				}
				offset := first
				limit := end
				rawErr := c.raw.Write(func(fd uintptr) bool {
					if tos != packets[first].tos {
						if err := unix.SetsockoptInt(int(fd), unix.IPPROTO_IP, unix.IP_TOS, packets[first].tos); err != nil {
							sendErr = err
							return true
						}
						tos = packets[first].tos
					}
					for offset < end {
						n, err := c.sendBatch(fd, messages[offset:limit])
						offset += n
						if err == unix.EMSGSIZE {
							if limit-offset > 1 {
								limit = offset + 1
								continue
							}
							// Like quic-go's send queue, let loss of an oversized probe drive
							// path-MTU discovery instead of killing the entire connection.
							offset++
							limit = end
							continue
						}
						if offset == limit {
							limit = end
						}
						if err == unix.ENOBUFS {
							if retryErr := c.waitForSendBuffer(); retryErr != nil {
								sendErr = retryErr
								return true
							}
							continue
						}
						if err == unix.EINTR {
							continue
						}
						if err == unix.EAGAIN || err == unix.EWOULDBLOCK {
							return false
						}
						if err != nil {
							sendErr = err
							return true
						}
						if n == 0 {
							sendErr = io.ErrNoProgress
							return true
						}
					}
					return true
				})
				if rawErr != nil {
					sendErr = rawErr
				}
				// Preserve partial progress before falling back on systems without this API
				// and unsupported socket options.
				if errors.Is(sendErr, unix.ENOSYS) || errors.Is(sendErr, unix.EOPNOTSUPP) || errors.Is(sendErr, unix.EINVAL) {
					fallback = true
					first = offset
					sendErr = nil
					continue
				}
				first = end
			}
			if sendErr != nil {
				break
			}
		}
		for i := 0; i < count; i++ {
			udpPackets.Put(packets[i])
			packets[i] = nil
		}
		if sendErr != nil {
			c.failureMu.Lock()
			c.failure = sendErr
			c.failureMu.Unlock()
			c.stopSocket()
			return
		}
	}
}
func (c *darwinPacketConn) ReadBatch(messages []ipv4.Message, _ int) (int, error) {
	c.readMu.Lock()
	defer c.readMu.Unlock()
	if len(messages) == 0 {
		return 0, nil
	}
	if c.readFallback {
		return c.readSingle(messages)
	}
	if c.next >= c.received {
		c.next, c.received = 0, 0
		for i := range c.readMessages {
			c.readVectors[i] = unix.Iovec{Base: &c.readBuffers[i][0], Len: uint64(len(c.readBuffers[i]))}
			c.readMessages[i] = darwinMessage{name: unsafe.Pointer(&c.readAddresses[i]), nameLength: uint32(unsafe.Sizeof(c.readAddresses[i])), iov: &c.readVectors[i], iovLength: 1, control: unsafe.Pointer(&c.readOOB[i][0]), controlLength: uint32(len(c.readOOB[i]))}
		}
		var readErr error
		err := c.raw.Read(func(fd uintptr) bool {
			for {
				c.received, readErr = c.nativeCall(false, fd, c.readMessages[:])
				if readErr != unix.EINTR {
					break
				}
			}
			return readErr != unix.EAGAIN && readErr != unix.EWOULDBLOCK
		})
		if err != nil {
			return 0, err
		}
		if readErr == unix.ENOSYS || readErr == unix.EOPNOTSUPP || readErr == unix.EINVAL {
			c.readFallback = true
			return c.readSingle(messages)
		}
		if readErr != nil {
			return 0, readErr
		}
		c.next = 0
	}
	count := 0
	for count < len(messages) && c.next < c.received {
		index := c.next
		header := c.readMessages[index]
		target := &messages[count]
		if header.length > uintptr(len(c.readBuffers[index])) || header.controlLength > uint32(len(c.readOOB[index])) {
			c.next++
			return count, io.ErrShortBuffer
		}
		payload := c.readBuffers[index][:header.length]
		target.N = 0
		for _, buffer := range target.Buffers {
			n := copy(buffer, payload)
			target.N += n
			payload = payload[n:]
		}
		target.NN = copy(target.OOB, c.readOOB[index][:header.controlLength])
		target.Flags = int(header.flags)
		if target.NN < int(header.controlLength) {
			target.Flags |= unix.MSG_CTRUNC
		}
		if len(payload) > 0 {
			target.Flags |= unix.MSG_TRUNC
		}
		address := c.readAddresses[index]
		port := int(address.Port>>8 | address.Port<<8)
		peer := c.peer.Load()
		if peer != nil && peer.Port == port && peer.IP.Equal(net.IP(address.Addr[:])) {
			target.Addr = peer
		} else {
			target.Addr = &net.UDPAddr{IP: append(net.IP(nil), address.Addr[:]...), Port: port}
		}
		c.next++
		count++
	}
	return count, nil
}
func (c *darwinPacketConn) readSingle(messages []ipv4.Message) (int, error) {
	if len(messages[0].Buffers) != 1 {
		return 0, errors.New("single UDP read requires one buffer")
	}
	n, nn, flags, addr, err := c.UDPConn.ReadMsgUDP(messages[0].Buffers[0], messages[0].OOB)
	if err != nil {
		return 0, err
	}
	messages[0].N = n
	messages[0].NN = nn
	messages[0].Flags = flags
	messages[0].Addr = addr
	return 1, nil
}
