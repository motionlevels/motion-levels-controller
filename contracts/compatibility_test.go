package contracts_test

import (
	"encoding/hex"
	"testing"

	"github.com/motionlevels/motion-levels-controller/contracts/floorpb"
	"github.com/motionlevels/motion-levels-controller/contracts/inputpb"
	"github.com/motionlevels/motion-levels-controller/contracts/recordingpb"
	"google.golang.org/protobuf/proto"
)

func TestProtocolV1GoldenWirePayloads(t *testing.T) {
	tests := []struct {
		name    string
		message proto.Message
		wantHex string
	}{
		{
			name: "pressure-event",
			message: &inputpb.PressureEvent{
				Sequence:   42,
				UnixNanos:  1_700_000_000_000_000_000,
				X:          3,
				Y:          7,
				Pressed:    true,
				Source:     "floor",
				Controller: 1,
				Channel:    2,
				Position:   4,
			},
			wantHex: "082a108080a8b1e39fe7cb171803200728013205666c6f6f72380140024804",
		},
		{
			name: "frame-record",
			message: &recordingpb.FrameRecord{
				Sequence:  9,
				UnixNanos: 1_700,
				Width:     16,
				Height:    32,
				Tiles: []*recordingpb.TileState{{
					X: 1, Y: 2, R: 3, G: 4, B: 5, Pressed: true,
				}},
				SessionId:                    "session-v1",
				GameFrameSequence:            8,
				GameUnixNanos:                1_600,
				ControllerReceivedUnixNanos:  1_650,
				ControllerPresentedUnixNanos: 1_700,
				VenueSessionId:               "venue-v1",
			},
			wantHex: "080910a40d181020202a0c080110021803200428053001320a73657373696f6e2d7631380840c00c48f20c50a40d5a0876656e75652d7631",
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			payload, err := proto.Marshal(test.message)
			if err != nil {
				t.Fatal(err)
			}
			if got := hex.EncodeToString(payload); got != test.wantHex {
				t.Fatalf("protocol v1 payload changed: got %s", got)
			}
		})
	}
}

func TestProtocolV2GoldenWirePayload(t *testing.T) {
	message := &floorpb.Envelope{Payload: &floorpb.Envelope_DesiredFrame{
		DesiredFrame: &floorpb.DesiredFrame{
			Sequence: 9, UnixNanos: 1_700, Width: 16, Height: 32,
			Rgb: []byte{1, 2, 3, 4, 5, 6},
		},
	}}
	payload, err := proto.Marshal(message)
	if err != nil {
		t.Fatal(err)
	}
	const wantHex = "1a11080910a40d181020202a06010203040506"
	if got := hex.EncodeToString(payload); got != wantHex {
		t.Fatalf("protocol v2 payload changed: got %s", got)
	}
}
