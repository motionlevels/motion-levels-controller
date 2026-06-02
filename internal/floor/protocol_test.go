package floor

import "testing"

func TestBuildSyncPacketChecksum(t *testing.T) {
	packet := BuildSyncPacket(0x43, 0x53, 8, []byte{255, 255, 255, 255})
	if len(packet) != 23 {
		t.Fatalf("sync packet length = %d, want 23", len(packet))
	}
	if packet[0] != 0x67 {
		t.Fatalf("sync packet type = 0x%02x, want 0x67", packet[0])
	}
	if got, want := packet[22], byte(0x4C); got != want {
		t.Fatalf("sync checksum = 0x%02x, want 0x%02x", got, want)
	}
}

func TestBuildFramePacketSequence(t *testing.T) {
	packets := BuildFrame(1, 8, 64, func(_, channel, position int) RGB {
		return RGB{R: byte(position), G: byte(channel), B: 7}
	})

	if len(packets) < 4 {
		t.Fatalf("packet count = %d, want at least 4", len(packets))
	}
	if packets[0][0] != 0x75 || packets[0][8] != 0x33 || packets[0][9] != 0x44 {
		t.Fatalf("first packet is not start marker: % x", packets[0][:10])
	}
	if packets[1][0] != 0x75 || packets[1][10] != 0xFF || packets[1][11] != 0xF0 {
		t.Fatalf("second packet is not channel config: % x", packets[1][:12])
	}

	data := packets[2]
	if data[0] != 0x75 || data[8] != 0x88 || data[9] != 0x77 {
		t.Fatalf("third packet is not RGB data: % x", data[:10])
	}
	if data[14] != 0 || data[22] != 0 || data[30] != 7 {
		t.Fatalf("first RGB planes got G=%d R=%d B=%d, want 0,0,7", data[14], data[22], data[30])
	}

	last := packets[len(packets)-1]
	if last[0] != 0x75 || last[8] != 0x55 || last[9] != 0x66 {
		t.Fatalf("last packet is not end marker: % x", last[:10])
	}
}
