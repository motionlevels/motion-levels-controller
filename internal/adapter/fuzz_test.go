package adapter

import (
	"bufio"
	"bytes"
	"encoding/binary"
	"testing"

	"github.com/motionlevels/motion-levels-controller/internal/floor"
)

func FuzzReadWireMessage(f *testing.F) {
	f.Add([]byte{})
	f.Add([]byte{messageDesiredFrame, 0, 0, 0, 0})
	valid, _ := encodeDesiredFrame(1, make([]byte, floor.RGBByteCount))
	var encoded bytes.Buffer
	_ = writeWireMessage(&encoded, messageDesiredFrame, valid)
	f.Add(encoded.Bytes())
	f.Fuzz(func(t *testing.T, data []byte) {
		_, _, _ = readWireMessage(bufio.NewReader(bytes.NewReader(data)))
	})
}

func FuzzDecodeDesiredFrame(f *testing.F) {
	valid, _ := encodeDesiredFrame(1, make([]byte, floor.RGBByteCount))
	f.Add(valid)
	f.Add([]byte{})
	f.Fuzz(func(t *testing.T, payload []byte) {
		sequence, rgb, err := decodeDesiredFrame(payload)
		if err == nil {
			if sequence == 0 || len(rgb) != floor.RGBByteCount {
				t.Fatalf("accepted invalid desired frame: sequence=%d RGB=%d", sequence, len(rgb))
			}
		}
	})
}

func FuzzDecodeOutputSnapshot(f *testing.F) {
	seed := encodeOutputSnapshot(OutputSnapshot{})
	f.Add(seed)
	f.Add([]byte{})
	f.Fuzz(func(t *testing.T, payload []byte) {
		_, _ = decodeOutputSnapshot(payload)
	})
}

func FuzzDecodeSensorPacket(f *testing.F) {
	valid := make([]byte, sensorPacketSize)
	valid[0] = 0x88
	f.Add(valid, uint8(0))
	f.Add([]byte{}, uint8(180))
	f.Fuzz(func(t *testing.T, packet []byte, rotationByte uint8) {
		rotation := floor.Rotation0
		if rotationByte%2 == 1 {
			rotation = floor.Rotation180
		}
		_, _ = decodeSensorPacket(packet, rotation)
	})
}

func FuzzWireLengthLimit(f *testing.F) {
	f.Add(uint32(maxWirePayload + 1))
	f.Add(uint32(0))
	f.Fuzz(func(t *testing.T, length uint32) {
		var header [wireHeaderSize]byte
		header[0] = messageDesiredFrame
		binary.BigEndian.PutUint32(header[1:], length)
		_, _, _ = readWireMessage(bufio.NewReader(bytes.NewReader(header[:])))
	})
}
