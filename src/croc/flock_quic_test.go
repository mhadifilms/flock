package croc

import (
	"context"
	"crypto/tls"
	"encoding/base64"
	"encoding/json"
	"github.com/mhadifilms/flock/src/message"
	"net"
	"strings"
	"testing"
	"time"
)

func TestFlockOfferAuthentication(t *testing.T) {
	key := []byte(strings.Repeat("k", 32))
	offer := flockOffer{Version: 1, Address: "127.0.0.1:9000", Streams: 8, Expires: time.Now().Add(time.Minute).Unix()}
	encode := func(o flockOffer) string {
		data, _ := json.Marshal(o)
		return base64.RawURLEncoding.EncodeToString(data)
	}
	offer.MAC = offerMAC(offer, key)
	if _, err := parseFlockOffer(encode(offer), key); err != nil {
		t.Fatal(err)
	}
	changed := offer
	changed.Address = "127.0.0.1:9001"
	if _, err := parseFlockOffer(encode(changed), key); err == nil {
		t.Fatal("accepted tampered endpoint")
	}
	if _, err := parseFlockOffer(encode(offer), []byte(strings.Repeat("z", 32))); err == nil {
		t.Fatal("accepted wrong session key")
	}
	changed = offer
	changed.Expires = time.Now().Add(-time.Minute).Unix()
	changed.MAC = offerMAC(changed, key)
	if _, err := parseFlockOffer(encode(changed), key); err == nil {
		t.Fatal("accepted expired offer")
	}
	for _, bad := range []string{"", "not-an-offer", strings.Repeat("x", 4097)} {
		if _, err := parseFlockOffer(bad, key); err == nil {
			t.Fatal("accepted invalid offer")
		}
	}
}
func TestFlockTLSAuthenticatesBothRoles(t *testing.T) {
	key := []byte(strings.Repeat("k", 32))
	serverConfig, err := flockTLS(key, true)
	if err != nil {
		t.Fatal(err)
	}
	for _, sameKey := range []bool{true, false} {
		t.Run(map[bool]string{true: "valid", false: "wrong-session"}[sameKey], func(t *testing.T) {
			clientKey := key
			if !sameKey {
				clientKey = []byte(strings.Repeat("z", 32))
			}
			clientConfig, err := flockTLS(clientKey, false)
			if err != nil {
				t.Fatal(err)
			}
			serverEnd, clientEnd := net.Pipe()
			defer serverEnd.Close()
			defer clientEnd.Close()
			server := tls.Server(serverEnd, serverConfig)
			client := tls.Client(clientEnd, clientConfig)
			ctx, cancel := context.WithTimeout(context.Background(), time.Second)
			defer cancel()
			serverResult := make(chan error, 1)
			go func() { serverResult <- server.HandshakeContext(ctx) }()
			clientErr := client.HandshakeContext(ctx)
			if clientErr != nil {
				clientEnd.Close()
			}
			serverErr := <-serverResult
			if sameKey && (clientErr != nil || serverErr != nil) {
				t.Fatalf("valid session rejected: %v %v", clientErr, serverErr)
			}
			if !sameKey && (clientErr == nil || serverErr == nil) {
				t.Fatal("wrong session accepted")
			}
		})
	}
}

func TestFlockSourceManifestRequiresAuthenticatedReceiver(t *testing.T) {
	called := false
	client := &Client{Options: Options{OnSourceManifest: func(data []byte) error { called = true; return nil }}}
	m := message.Message{Type: flockSourceHashMessage, Bytes: []byte("[]")}
	if err := client.processSourceManifest(m); err == nil || called {
		t.Fatal("accepted unauthenticated source manifest")
	}
	client.Key = make([]byte, 32)
	client.Options.IsSender = true
	if err := client.processSourceManifest(m); err == nil || called {
		t.Fatal("sender accepted a source manifest")
	}
	client.Options.IsSender = false
	if err := client.processSourceManifest(m); err != nil || !called {
		t.Fatalf("authenticated receiver rejected manifest: %v", err)
	}
}
