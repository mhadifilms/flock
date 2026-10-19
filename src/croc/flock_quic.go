package croc

import (
	"context"
	"crypto/ed25519"
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/base64"
	"encoding/json"
	"errors"
	"io"
	"math/big"
	"net"
	"strconv"
	"sync"
	"time"

	"github.com/quic-go/quic-go"
	"github.com/mhadifilms/flock/src/message"
	"github.com/mhadifilms/flock/src/tailcattransport"
)

const flockQUICFeature = "flock-quic-v1"
const flockUDPPeer message.Type = "flock-udp-peer"

// QUICConfig selects directly reachable UDP file-data endpoints. The Croc relay
// is used only for the PAKE-authenticated control channel.
type QUICConfig struct {
	Listen    string
	Advertise string
	Streams   int
}

type flockTransport struct {
	config QUICConfig
	peers  chan *net.UDPAddr
	notify func(string) error
}

// NewQUICCtx retains Croc's transfer engine and PAKE while requiring native
// QUIC data streams, mutual TLS, and a Flock-capable peer. No TCP data fallback.
func NewQUICCtx(ctx context.Context, ops Options, config QUICConfig) (*Client, error) {
	if config.Listen == "" {
		config.Listen = "0.0.0.0:0"
	}
	if config.Streams == 0 {
		config.Streams = 8
	}
	if config.Streams < 1 || config.Streams > 16 {
		return nil, errors.New("QUIC streams must be between 1 and 16")
	}
	if ops.IsSender {
		ip := net.ParseIP(config.Advertise)
		if ip == nil || ip.To4() == nil || ip.IsUnspecified() || ip.IsMulticast() {
			return nil, errors.New("--advertise requires a reachable IPv4 address")
		}
		ops.Transport = TransportDERP
	} else {
		ops.Transport = TransportAuto
	}
	ops.DisableLocal = true
	provider := &flockTransport{config: config, peers: make(chan *net.UDPAddr, 1)}
	c, err := newClientCtx(ctx, ops, provider)
	if err != nil {
		return nil, err
	}
	provider.notify = func(address string) error {
		return message.Send(c.connection(0), c.Key, message.Message{Type: flockUDPPeer, Message: address})
	}
	return c, nil
}

func (*flockTransport) Available() bool { return true }

type flockOffer struct {
	Version int    `json:"version"`
	Address string `json:"address"`
	Streams int    `json:"streams"`
	Expires int64  `json:"expires"`
	MAC     []byte `json:"mac"`
}

func offerMAC(offer flockOffer, key []byte) []byte {
	offer.MAC = nil
	data, _ := json.Marshal(offer)
	mac := hmac.New(sha256.New, key)
	mac.Write([]byte("flock-offer-v1\x00"))
	mac.Write(data)
	return mac.Sum(nil)
}
func parseFlockOffer(value string, key []byte) (flockOffer, error) {
	var o flockOffer
	if len(value) > 4096 || len(key) < 16 {
		return o, errors.New("invalid Flock QUIC offer")
	}
	data, err := base64.RawURLEncoding.DecodeString(value)
	if err != nil || json.Unmarshal(data, &o) != nil || o.Version != 1 || o.Streams < 1 || o.Streams > 16 || o.Expires < time.Now().Unix() || !hmac.Equal(o.MAC, offerMAC(o, key)) {
		return o, errors.New("invalid Flock QUIC offer")
	}
	if _, err = literalUDPAddress(o.Address); err != nil {
		return o, err
	}
	return o, nil
}
func (*flockTransport) ValidateOffer(value string, key []byte) error {
	_, err := parseFlockOffer(value, key)
	return err
}
func literalUDPAddress(value string) (*net.UDPAddr, error) {
	host, port, err := net.SplitHostPort(value)
	ip := net.ParseIP(host)
	n, e := strconv.Atoi(port)
	if err != nil || e != nil || ip == nil || ip.To4() == nil || ip.IsUnspecified() || ip.IsMulticast() || n < 1 || n > 65535 {
		return nil, errors.New("invalid Flock UDP endpoint")
	}
	return &net.UDPAddr{IP: ip.To4(), Port: n}, nil
}

// Session-derived, role-separated Ed25519 identities make TLS mutually
// authenticated by the PAKE session key. Certificates never leave that session.
func flockTLS(key []byte, server bool) (*tls.Config, error) {
	identity := func(role string) (tls.Certificate, *x509.Certificate, error) {
		mac := hmac.New(sha256.New, key)
		mac.Write([]byte("flock-tls-v1/" + role))
		private := ed25519.NewKeyFromSeed(mac.Sum(nil))
		template := &x509.Certificate{SerialNumber: big.NewInt(1), Subject: pkix.Name{CommonName: "flock"}, DNSNames: []string{"flock"}, NotBefore: time.Date(2000, 1, 1, 0, 0, 0, 0, time.UTC), NotAfter: time.Date(2100, 1, 1, 0, 0, 0, 0, time.UTC), KeyUsage: x509.KeyUsageDigitalSignature, ExtKeyUsage: []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth, x509.ExtKeyUsageClientAuth}}
		der, err := x509.CreateCertificate(rand.Reader, template, template, private.Public(), private)
		if err != nil {
			return tls.Certificate{}, nil, err
		}
		cert, err := x509.ParseCertificate(der)
		return tls.Certificate{Certificate: [][]byte{der}, PrivateKey: private}, cert, err
	}
	own, peer := "client", "server"
	if server {
		own, peer = peer, own
	}
	local, _, err := identity(own)
	if err != nil {
		return nil, err
	}
	_, remote, err := identity(peer)
	if err != nil {
		return nil, err
	}
	roots := x509.NewCertPool()
	roots.AddCert(remote)
	conf := &tls.Config{MinVersion: tls.VersionTLS13, Certificates: []tls.Certificate{local}, NextProtos: []string{"flock/croc-quic/1"}, RootCAs: roots, ServerName: "flock"}
	if server {
		conf.ClientAuth = tls.RequireAndVerifyClientCert
		conf.ClientCAs = roots
	}
	return conf, nil
}
func flockQUICConfig() *quic.Config {
	return &quic.Config{HandshakeIdleTimeout: 15 * time.Second, MaxIdleTimeout: 60 * time.Second, KeepAlivePeriod: 5 * time.Second, MaxIncomingStreams: 16, InitialStreamReceiveWindow: 8 << 20, MaxStreamReceiveWindow: 16 << 20, InitialConnectionReceiveWindow: 32 << 20, MaxConnectionReceiveWindow: 64 << 20}
}

type flockListener struct {
	listener  *quic.Listener
	transport *quic.Transport
	socket    *net.UDPConn
	peers     <-chan *net.UDPAddr
	offer     string
	streams   int
	events    tailcattransport.PathEvent
}

func (p *flockTransport) Listen(_ context.Context, key []byte, events tailcattransport.PathEvent) (tailcatDataListener, error) {
	address, err := net.ResolveUDPAddr("udp4", p.config.Listen)
	if err != nil {
		return nil, err
	}
	socket, err := net.ListenUDP("udp4", address)
	if err != nil {
		return nil, err
	}
	conf, err := flockTLS(key, true)
	if err != nil {
		socket.Close()
		return nil, err
	}
	transport := &quic.Transport{Conn: socket}
	listener, err := transport.Listen(conf, flockQUICConfig())
	if err != nil {
		transport.Close()
		socket.Close()
		return nil, err
	}
	port := socket.LocalAddr().(*net.UDPAddr).Port
	offer := flockOffer{Version: 1, Address: net.JoinHostPort(p.config.Advertise, strconv.Itoa(port)), Streams: p.config.Streams, Expires: time.Now().Add(time.Hour).Unix()}
	offer.MAC = offerMAC(offer, key)
	data, _ := json.Marshal(offer)
	if events != nil {
		events("quic_listening")
	}
	return &flockListener{listener, transport, socket, p.peers, base64.RawURLEncoding.EncodeToString(data), p.config.Streams, events}, nil
}
func (l *flockListener) Offer() string { return l.offer }
func (l *flockListener) Close() error {
	l.listener.Close()
	l.transport.Close()
	return l.socket.Close()
}
func (l *flockListener) Accept(ctx context.Context) (*tailcatDataBundle, error) {
	// The authenticated control channel exchanges the recipient's chosen UDP
	// socket before the handshake, allowing both endpoints to originate traffic.
	var peer *net.UDPAddr
	select {
	case peer = <-l.peers:
	case <-ctx.Done():
		return nil, ctx.Err()
	}
	probeCtx, cancel := context.WithCancel(ctx)
	defer cancel()
	go func() {
		timer := time.NewTicker(100 * time.Millisecond)
		defer timer.Stop()
		for {
			l.socket.WriteToUDP([]byte{0}, peer)
			select {
			case <-timer.C:
			case <-probeCtx.Done():
				return
			}
		}
	}()
	conn, err := l.listener.Accept(ctx)
	if err != nil {
		return nil, err
	}
	streams := make([]net.Conn, l.streams)
	for i := range streams {
		stream, err := conn.OpenStreamSync(ctx)
		if err == nil {
			_, err = stream.Write([]byte{byte(i)})
		}
		if err != nil {
			conn.CloseWithError(1, "stream setup failed")
			return nil, err
		}
		streams[i] = &flockStream{Stream: stream, conn: conn}
	}
	if l.events != nil {
		l.events("quic_connected")
	}
	return &tailcatDataBundle{connections: streams, cleanup: func() error { return conn.CloseWithError(0, "finished") }}, nil
}
func (p *flockTransport) Dial(ctx context.Context, value string, key []byte, events tailcattransport.PathEvent) (*tailcatDataBundle, error) {
	offer, err := parseFlockOffer(value, key)
	if err != nil {
		return nil, err
	}
	address, _ := literalUDPAddress(offer.Address)
	route, err := net.DialUDP("udp4", nil, address)
	if err != nil {
		return nil, err
	}
	localIP := route.LocalAddr().(*net.UDPAddr).IP
	route.Close()
	socket, err := net.ListenUDP("udp4", &net.UDPAddr{IP: localIP})
	if err != nil {
		return nil, err
	}
	transport := &quic.Transport{Conn: socket}
	cleanup := func() { transport.Close(); socket.Close() }
	if p.notify == nil {
		cleanup()
		return nil, errors.New("missing authenticated UDP pairing channel")
	}
	if err = p.notify(socket.LocalAddr().String()); err != nil {
		cleanup()
		return nil, err
	}
	socket.WriteToUDP([]byte{0}, address)
	conf, err := flockTLS(key, false)
	if err != nil {
		cleanup()
		return nil, err
	}
	conn, err := transport.Dial(ctx, address, conf, flockQUICConfig())
	if err != nil {
		cleanup()
		return nil, err
	}
	streams := make([]net.Conn, offer.Streams)
	for range streams {
		stream, err := conn.AcceptStream(ctx)
		var index [1]byte
		if err == nil {
			_, err = io.ReadFull(stream, index[:])
		}
		if err != nil || int(index[0]) >= len(streams) || streams[index[0]] != nil {
			conn.CloseWithError(1, "invalid stream setup")
			cleanup()
			return nil, errors.New("invalid Flock stream setup")
		}
		streams[index[0]] = &flockStream{Stream: stream, conn: conn}
	}
	if events != nil {
		events("quic_connected")
	}
	return &tailcatDataBundle{connections: streams, cleanup: func() error { err := conn.CloseWithError(0, "finished"); cleanup(); return err }}, nil
}

type flockStream struct {
	*quic.Stream
	conn *quic.Conn
	once sync.Once
}

func (s *flockStream) LocalAddr() net.Addr  { return s.conn.LocalAddr() }
func (s *flockStream) RemoteAddr() net.Addr { return s.conn.RemoteAddr() }
func (s *flockStream) Close() error {
	var err error
	s.once.Do(func() { s.CancelRead(0); err = s.Stream.Close() })
	return err
}
