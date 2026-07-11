package pbstream

import (
	"bufio"
	"encoding/binary"
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
	length, err := binary.ReadUvarint(reader)
	if err != nil {
		return err
	}
	data := make([]byte, length)
	if _, err := io.ReadFull(reader, data); err != nil {
		return err
	}
	return proto.Unmarshal(data, message)
}
