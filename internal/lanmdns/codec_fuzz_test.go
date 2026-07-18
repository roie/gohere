package lanmdns

import (
	"net/netip"
	"testing"
)

func FuzzDecodePacket(f *testing.F) {
	valid, err := probeMessage("shop.local.", netip.MustParseAddr("192.168.1.42"))
	if err != nil {
		f.Fatal(err)
	}
	for _, seed := range [][]byte{
		valid,
		{},
		{0},
		{0, 0, 0, 0, 0, 1},
		{0, 0, 0, 0, 0, 1, 0xc0, 0x0c, 0, 1, 0, 1},
	} {
		f.Add(seed)
	}
	f.Fuzz(func(t *testing.T, payload []byte) {
		_, _ = decodePacket(payload)
	})
}
