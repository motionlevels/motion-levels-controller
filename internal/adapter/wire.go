package adapter

import (
	"bufio"
	"encoding/binary"
	"fmt"
	"io"
	"math"
	"time"

	"github.com/motionlevels/motion-levels-controller/internal/floor"
)

const (
	messageDesiredFrame  byte = 1
	messagePressureState byte = 2
	messageOutputState   byte = 3

	wireHeaderSize = 5
	maxWirePayload = 4096
)

func readWireMessage(reader *bufio.Reader) (byte, []byte, error) {
	var header [wireHeaderSize]byte
	if _, err := io.ReadFull(reader, header[:]); err != nil {
		return 0, nil, err
	}
	length := int(binary.BigEndian.Uint32(header[1:]))
	if length < 0 || length > maxWirePayload {
		return 0, nil, fmt.Errorf("wire payload is %d bytes, limit is %d", length, maxWirePayload)
	}
	payload := make([]byte, length)
	if _, err := io.ReadFull(reader, payload); err != nil {
		return 0, nil, err
	}
	return header[0], payload, nil
}

func writeWireMessage(writer io.Writer, messageType byte, payload []byte) error {
	if len(payload) > maxWirePayload {
		return fmt.Errorf("wire payload is %d bytes, limit is %d", len(payload), maxWirePayload)
	}
	var header [wireHeaderSize]byte
	header[0] = messageType
	binary.BigEndian.PutUint32(header[1:], uint32(len(payload)))
	if _, err := writer.Write(header[:]); err != nil {
		return err
	}
	_, err := writer.Write(payload)
	return err
}

func decodeDesiredFrame(payload []byte) (uint64, []byte, error) {
	const metadataBytes = 8
	if len(payload) != metadataBytes+floor.RGBByteCount {
		return 0, nil, fmt.Errorf("desired frame payload is %d bytes, want %d", len(payload), metadataBytes+floor.RGBByteCount)
	}
	sequence := binary.BigEndian.Uint64(payload[:metadataBytes])
	if sequence == 0 {
		return 0, nil, fmt.Errorf("desired frame sequence must be positive")
	}
	return sequence, payload[metadataBytes:], nil
}

func encodeDesiredFrame(sequence uint64, rgb []byte) ([]byte, error) {
	if sequence == 0 {
		return nil, fmt.Errorf("desired frame sequence must be positive")
	}
	if len(rgb) != floor.RGBByteCount {
		return nil, fmt.Errorf("RGB payload is %d bytes, want %d", len(rgb), floor.RGBByteCount)
	}
	payload := make([]byte, 8+len(rgb))
	binary.BigEndian.PutUint64(payload[:8], sequence)
	copy(payload[8:], rgb)
	return payload, nil
}

func encodePressureSnapshot(snapshot PressureSnapshot) []byte {
	payload := make([]byte, 16+floor.PressureByteCount)
	binary.BigEndian.PutUint64(payload[:8], snapshot.Sequence)
	binary.BigEndian.PutUint64(payload[8:16], uint64(snapshot.ObservedAt.UnixNano()))
	copy(payload[16:], snapshot.Bits[:])
	return payload
}

func decodePressureSnapshot(payload []byte) (PressureSnapshot, error) {
	if len(payload) != 16+floor.PressureByteCount {
		return PressureSnapshot{}, fmt.Errorf("pressure payload is %d bytes, want %d", len(payload), 16+floor.PressureByteCount)
	}
	var snapshot PressureSnapshot
	snapshot.Sequence = binary.BigEndian.Uint64(payload[:8])
	snapshot.ObservedAt = time.Unix(0, int64(binary.BigEndian.Uint64(payload[8:16])))
	copy(snapshot.Bits[:], payload[16:])
	return snapshot, nil
}

func encodeOutputSnapshot(snapshot OutputSnapshot) []byte {
	const metadataBytes = 53
	payload := make([]byte, metadataBytes+floor.PressureByteCount)
	binary.BigEndian.PutUint64(payload[0:8], snapshot.FramesSent)
	binary.BigEndian.PutUint64(payload[8:16], snapshot.DesiredSequence)
	binary.BigEndian.PutUint64(payload[16:24], uint64(snapshot.ObservedAt.UnixNano()))
	binary.BigEndian.PutUint64(payload[24:32], uint64(snapshot.DesiredFrameAge.Milliseconds()))
	binary.BigEndian.PutUint32(payload[32:36], math.Float32bits(snapshot.FadeRatio))
	if snapshot.UDPWriteAvailable {
		payload[36] |= 1 << 0
	}
	if snapshot.FloorSeenRecently {
		payload[36] |= 1 << 1
	}
	if snapshot.PhysicalFrameWasSent {
		payload[36] |= 1 << 2
	}
	binary.BigEndian.PutUint64(payload[37:45], snapshot.UDPWriteErrors)
	binary.BigEndian.PutUint64(payload[45:53], snapshot.PressureSequence)
	copy(payload[metadataBytes:], snapshot.PressureBits[:])
	return payload
}

func decodeOutputSnapshot(payload []byte) (OutputSnapshot, error) {
	const metadataBytes = 53
	if len(payload) != metadataBytes+floor.PressureByteCount {
		return OutputSnapshot{}, fmt.Errorf("output payload is %d bytes, want %d", len(payload), metadataBytes+floor.PressureByteCount)
	}
	var snapshot OutputSnapshot
	snapshot.FramesSent = binary.BigEndian.Uint64(payload[0:8])
	snapshot.DesiredSequence = binary.BigEndian.Uint64(payload[8:16])
	snapshot.ObservedAt = time.Unix(0, int64(binary.BigEndian.Uint64(payload[16:24])))
	snapshot.DesiredFrameAge = time.Duration(int64(binary.BigEndian.Uint64(payload[24:32]))) * time.Millisecond
	snapshot.FadeRatio = math.Float32frombits(binary.BigEndian.Uint32(payload[32:36]))
	snapshot.UDPWriteAvailable = payload[36]&(1<<0) != 0
	snapshot.FloorSeenRecently = payload[36]&(1<<1) != 0
	snapshot.PhysicalFrameWasSent = payload[36]&(1<<2) != 0
	snapshot.UDPWriteErrors = binary.BigEndian.Uint64(payload[37:45])
	snapshot.PressureSequence = binary.BigEndian.Uint64(payload[45:53])
	copy(snapshot.PressureBits[:], payload[metadataBytes:])
	return snapshot, nil
}
