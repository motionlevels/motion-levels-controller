package pbstream

import (
	"bufio"
	"encoding/binary"
	"fmt"
	"io"

	"google.golang.org/protobuf/proto"
)

func Write(writer io.Writer, message proto.Message) error {
	data, err := proto.Marshal(message)
	if err != nil {
		return err
	}

	var length [binary.MaxVarintLen64]byte
	n := binary.PutUvarint(length[:], uint64(len(data)))
	if _, err := writer.Write(length[:n]); err != nil {
		return err
	}
	_, err = writer.Write(data)
	return err
}

func Read(reader *bufio.Reader, message proto.Message) error {
	return ReadLimit(reader, message, 0)
}

// ReadLimit reads one length-prefixed protobuf message and rejects payloads
// larger than maxBytes before allocating them. A non-positive limit preserves
// the legacy unbounded behavior for protocol-v1 callers.
func ReadLimit(reader *bufio.Reader, message proto.Message, maxBytes int) error {
	length, err := binary.ReadUvarint(reader)
	if err != nil {
		return err
	}
	if maxBytes > 0 && length > uint64(maxBytes) {
		return fmt.Errorf("protobuf payload is %d bytes, limit is %d", length, maxBytes)
	}
	data := make([]byte, length)
	if _, err := io.ReadFull(reader, data); err != nil {
		return err
	}
	return proto.Unmarshal(data, message)
}
