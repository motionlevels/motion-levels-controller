package adapter

import (
	"bufio"
	"bytes"
	"testing"
	"time"

	"github.com/motionlevels/motion-levels-controller/internal/floor"
)

func TestDesiredFrameWireRoundTrip(t *testing.T) {
	rgb := make([]byte, floor.RGBByteCount)
	rgb[0], rgb[1], rgb[2] = 10, 20, 30
	payload, err := encodeDesiredFrame(7, rgb)
	if err != nil {
		t.Fatal(err)
	}
	sequence, decoded, err := decodeDesiredFrame(payload)
	if err != nil {
		t.Fatal(err)
	}
	if sequence != 7 || !bytes.Equal(decoded, rgb) {
		t.Fatalf("decoded sequence=%d RGB equal=%v", sequence, bytes.Equal(decoded, rgb))
	}
}

func TestDesiredFrameRejectsInvalidPayloads(t *testing.T) {
	if _, err := encodeDesiredFrame(0, make([]byte, floor.RGBByteCount)); err == nil {
		t.Fatal("zero sequence should be rejected")
	}
	if _, err := encodeDesiredFrame(1, []byte{1, 2, 3}); err == nil {
		t.Fatal("short RGB should be rejected")
	}
	if _, _, err := decodeDesiredFrame([]byte{1, 2, 3}); err == nil {
		t.Fatal("short wire payload should be rejected")
	}
}

func TestPressureAndOutputWireRoundTrip(t *testing.T) {
	pressure := PressureSnapshot{Sequence: 9, ObservedAt: time.Unix(0, 123)}
	pressure.Bits[2] = 0x80
	decodedPressure, err := decodePressureSnapshot(encodePressureSnapshot(pressure))
	if err != nil {
		t.Fatal(err)
	}
	if decodedPressure.Sequence != pressure.Sequence || decodedPressure.ObservedAt.UnixNano() != 123 || decodedPressure.Bits != pressure.Bits {
		t.Fatalf("unexpected pressure round trip: %+v", decodedPressure)
	}

	output := OutputSnapshot{
		FramesSent:           11,
		DesiredSequence:      7,
		ObservedAt:           time.Unix(0, 456),
		DesiredFrameAge:      250 * time.Millisecond,
		FadeRatio:            0.25,
		PressureSequence:     9,
		PressureBits:         pressure.Bits,
		UDPWriteAvailable:    true,
		FloorSeenRecently:    true,
		UDPWriteErrors:       3,
		PhysicalFrameWasSent: true,
	}
	decodedOutput, err := decodeOutputSnapshot(encodeOutputSnapshot(output))
	if err != nil {
		t.Fatal(err)
	}
	if decodedOutput.FramesSent != 11 || decodedOutput.DesiredSequence != 7 || decodedOutput.ObservedAt.UnixNano() != 456 || decodedOutput.DesiredFrameAge != 250*time.Millisecond || decodedOutput.FadeRatio != 0.25 || decodedOutput.PressureSequence != 9 || decodedOutput.PressureBits != pressure.Bits || !decodedOutput.UDPWriteAvailable || !decodedOutput.FloorSeenRecently || decodedOutput.UDPWriteErrors != 3 || !decodedOutput.PhysicalFrameWasSent {
		t.Fatalf("unexpected output round trip: %+v", decodedOutput)
	}
}

func TestWireMessageRejectsOversizedPayloadBeforeRead(t *testing.T) {
	var input bytes.Buffer
	input.WriteByte(messageDesiredFrame)
	input.Write([]byte{0, 0, 0x20, 0}) // 8192 bytes
	if _, _, err := readWireMessage(bufioReader(&input)); err == nil {
		t.Fatal("oversized payload should be rejected")
	}
}

func bufioReader(input *bytes.Buffer) *bufio.Reader {
	return bufio.NewReader(input)
}
