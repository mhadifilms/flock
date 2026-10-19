package croc

import (
	"errors"

	"github.com/mhadifilms/flock/src/message"
)

const flockSourceHashFeature = "flock-source-sha256-v1"
const flockSourceHashMessage message.Type = "flock-source-sha256"

// The CLI computes the full source digest alongside data transfer. Delivery
// uses the existing PAKE-authenticated encrypted control connection; required
// support is negotiated before opening a data bundle.
func (c *Client) startSourceManifest(attempt *transferAttemptState) {
	if c.Options.SourceManifest == nil {
		return
	}
	c.sourceManifestOnce.Do(func() {
		go func() {
			data, err := c.Options.SourceManifest(c.stop.ctx)
			if err == nil && (len(data) == 0 || len(data) > 4<<20) {
				err = errors.New("invalid Flock source manifest size")
			}
			if err == nil {
				err = message.Send(c.connection(0), c.Key, message.Message{Type: flockSourceHashMessage, Bytes: data})
			}
			if err != nil && c.ctxErr() == nil {
				attempt.report(err)
			}
		}()
	})
}
func (c *Client) processSourceManifest(m message.Message) error {
	if c.Options.IsSender || c.Options.OnSourceManifest == nil || len(c.Key) == 0 || len(m.Bytes) == 0 || len(m.Bytes) > 4<<20 {
		return errors.New("unexpected Flock source SHA-256 manifest")
	}
	return c.Options.OnSourceManifest(m.Bytes)
}
