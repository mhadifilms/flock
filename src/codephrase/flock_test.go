package codephrase

import (
	"strings"
	"testing"
)

func TestFlockCapabilityRoomUsesEntireSecret(t *testing.T) {
	first := "flock1:" + strings.Repeat("00", 32)
	second := first[:len(first)-2] + "01"
	a, err := Parse(first)
	if err != nil {
		t.Fatal(err)
	}
	b, err := Parse(second)
	if err != nil {
		t.Fatal(err)
	}
	if len(a.RoomName) != 64 || a.RoomName == b.RoomName || a.PAKEPassphrase != first || a.Format != Format("flock-v1") {
		t.Fatal("capability lost entropy or format")
	}
	again, _ := Parse(first)
	if again != a {
		t.Fatal("room is not deterministic")
	}
	for _, bad := range []string{"flock1:", "flock1:" + strings.Repeat("f", 63), "flock1:" + strings.Repeat("z", 64)} {
		if _, err := Parse(bad); err == nil || strings.Contains(err.Error(), bad) {
			t.Fatal("invalid capability accepted or echoed")
		}
	}
}
