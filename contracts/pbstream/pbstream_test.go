package pbstream

import (
	"bufio"
	"bytes"
	"encoding/binary"
	"strings"
	"testing"

	"google.golang.org/protobuf/types/known/wrapperspb"
)

func TestReadLimitRejectsOversizedPayloadBeforeAllocation(t *testing.T) {
	var wire bytes.Buffer
	var prefix [binary.MaxVarintLen64]byte
	n := binary.PutUvarint(prefix[:], 1024)
	if _, err := wire.Write(prefix[:n]); err != nil {
		t.Fatal(err)
	}

	var message wrapperspb.StringValue
	err := ReadLimit(bufio.NewReader(&wire), &message, 16)
	if err == nil || !strings.Contains(err.Error(), "limit is 16") {
		t.Fatalf("ReadLimit error = %v", err)
	}
}

func TestReadLimitAcceptsBoundedPayload(t *testing.T) {
	var wire bytes.Buffer
	want := wrapperspb.String("floor")
	if err := Write(&wire, want); err != nil {
		t.Fatal(err)
	}

	var got wrapperspb.StringValue
	if err := ReadLimit(bufio.NewReader(&wire), &got, 64); err != nil {
		t.Fatal(err)
	}
	if got.Value != want.Value {
		t.Fatalf("value = %q, want %q", got.Value, want.Value)
	}
}
