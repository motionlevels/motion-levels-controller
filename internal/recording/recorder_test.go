package recording

import (
	"bufio"
	"bytes"
	"compress/gzip"
	"encoding/binary"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/motionlevels/motion-levels-controller/contracts/recordingpb"
	"github.com/motionlevels/motion-levels-controller/internal/floor"
	"google.golang.org/protobuf/proto"
)

func TestFrameRecorderWritesLengthPrefixedFrameRecord(t *testing.T) {
	path := t.TempDir() + "/frames.pbstream"
	recorder, err := NewFrameRecorder(path)
	if err != nil {
		t.Fatal(err)
	}

	tiles := []floor.Tile{
		{X: 0, Y: 0, R: 1, G: 2, B: 3, Pressed: true},
		{X: 1, Y: 0, R: 4, G: 5, B: 6, Pressed: false},
	}
	now := time.Now().UnixNano()
	if err := recorder.RecordFrame(42, now, 16, 32, tiles); err != nil {
		t.Fatal(err)
	}
	if err := recorder.Close(); err != nil {
		t.Fatal(err)
	}

	record := readFrameRecord(t, path, false)
	if record.Sequence != 42 || record.UnixNanos != now || record.Width != 16 || record.Height != 32 {
		t.Fatalf("unexpected frame metadata: %+v", record)
	}
	if len(record.Tiles) != 2 || !record.Tiles[0].Pressed || record.Tiles[1].Pressed {
		t.Fatalf("unexpected tiles: %+v", record.Tiles)
	}
	stats := recorder.Stats()
	if stats.WrittenFrames != 1 {
		t.Fatalf("written frames = %d, want 1", stats.WrittenFrames)
	}
	if stats.Compression != "none" {
		t.Fatalf("compression = %q, want none", stats.Compression)
	}
}

func TestFrameRecorderPersistsFrameLineage(t *testing.T) {
	path := t.TempDir() + "/frames.pbstream"
	recorder, err := NewFrameRecorder(path)
	if err != nil {
		t.Fatal(err)
	}

	presentedAt := time.Now().UnixNano()
	lineage := FrameLineage{
		SessionID:                    "session-1",
		GameFrameSequence:            99,
		GameUnixNanos:                presentedAt - int64(8*time.Millisecond),
		ControllerReceivedUnixNanos:  presentedAt - int64(3*time.Millisecond),
		ControllerPresentedUnixNanos: presentedAt,
	}
	if err := recorder.RecordFrameWithLineage(42, presentedAt, 16, 32, []floor.Tile{{X: 1, Y: 2, R: 3}}, lineage); err != nil {
		t.Fatal(err)
	}
	if err := recorder.Close(); err != nil {
		t.Fatal(err)
	}

	record := readFrameRecord(t, path, false)
	if record.Sequence != 42 || record.UnixNanos != presentedAt {
		t.Fatalf("controller presentation fields = %d/%d, want 42/%d", record.Sequence, record.UnixNanos, presentedAt)
	}
	if record.SessionId != lineage.SessionID || record.GameFrameSequence != lineage.GameFrameSequence || record.GameUnixNanos != lineage.GameUnixNanos {
		t.Fatalf("missing game lineage: %+v", record)
	}
	if record.ControllerReceivedUnixNanos != lineage.ControllerReceivedUnixNanos || record.ControllerPresentedUnixNanos != lineage.ControllerPresentedUnixNanos {
		t.Fatalf("missing controller lineage: %+v", record)
	}
}

func TestFrameRecorderWritesGzipCompressedFrameRecord(t *testing.T) {
	path := t.TempDir() + "/frames.pbstream"
	recorder, err := NewFrameRecorderWithOptions(path, Options{Compression: "gzip"})
	if err != nil {
		t.Fatal(err)
	}

	tiles := make([]floor.Tile, 0, floor.GridWidth*floor.GridHeight)
	for y := 0; y < floor.GridHeight; y++ {
		for x := 0; x < floor.GridWidth; x++ {
			tiles = append(tiles, floor.Tile{X: x, Y: y, R: 20, G: 40, B: 60})
		}
	}
	now := time.Now().UnixNano()
	if err := recorder.RecordFrame(7, now, floor.GridWidth, floor.GridHeight, tiles); err != nil {
		t.Fatal(err)
	}
	if err := recorder.Close(); err != nil {
		t.Fatal(err)
	}

	compressedPath := path + ".gz"
	record := readFrameRecord(t, compressedPath, true)
	if record.Sequence != 7 || len(record.Tiles) != floor.GridWidth*floor.GridHeight {
		t.Fatalf("unexpected gzip record: sequence=%d tiles=%d", record.Sequence, len(record.Tiles))
	}
	if stats := recorder.Stats(); stats.Compression != "gzip" {
		t.Fatalf("compression = %q, want gzip", stats.Compression)
	}
	compressedInfo, err := os.Stat(compressedPath)
	if err != nil {
		t.Fatal(err)
	}
	if compressedInfo.Size() >= 6*1024 {
		t.Fatalf("compressed single-frame file is unexpectedly large: %d bytes", compressedInfo.Size())
	}
}

func TestFrameRecorderRotatesSegmentsByCompressedBytes(t *testing.T) {
	path := filepath.Join(t.TempDir(), "frames.pbstream")
	recorder, err := NewFrameRecorderWithOptions(path, Options{
		Compression:     "gzip",
		MaxSegmentBytes: 1,
	})
	if err != nil {
		t.Fatal(err)
	}

	for sequence := uint64(1); sequence <= 3; sequence++ {
		if err := recorder.RecordFrame(sequence, time.Now().UnixNano(), 1, 1, []floor.Tile{{X: 0, Y: 0, R: byte(sequence)}}); err != nil {
			t.Fatal(err)
		}
	}
	if err := recorder.Close(); err != nil {
		t.Fatal(err)
	}

	paths := []string{
		path + ".gz",
		filepath.Join(filepath.Dir(path), "frames-000002.pbstream.gz"),
		filepath.Join(filepath.Dir(path), "frames-000003.pbstream.gz"),
	}
	if len(paths) != 3 {
		t.Fatalf("segment count = %d, want 3: %v", len(paths), paths)
	}

	var sequences []uint64
	for _, segmentPath := range paths {
		_, err := ReadRecoverableFrameRecords(segmentPath, "gzip", func(record *recordingpb.FrameRecord) error {
			sequences = append(sequences, record.Sequence)
			return nil
		})
		if err != nil {
			t.Fatal(err)
		}
		info, err := os.Stat(segmentPath)
		if err != nil {
			t.Fatal(err)
		}
		if info.Size() == 0 {
			t.Fatalf("segment %s is empty", segmentPath)
		}
	}
	if len(sequences) != 3 || sequences[0] != 1 || sequences[1] != 2 || sequences[2] != 3 {
		t.Fatalf("rotated sequences = %v, want [1 2 3]", sequences)
	}
	if stats := recorder.Stats(); stats.SegmentIndex != 3 || stats.WrittenFrames != 3 {
		t.Fatalf("stats after rotation = %+v, want segment 3 and 3 frames", stats)
	}
}

func TestFrameRecorderRotatesSegmentsBySessionID(t *testing.T) {
	path := filepath.Join(t.TempDir(), "frames.pbstream")
	recorder, err := NewFrameRecorderWithOptions(path, Options{
		MaxSegmentBytes: 1 << 20,
	})
	if err != nil {
		t.Fatal(err)
	}

	now := time.Now()
	for sequence, sessionID := range []string{"session-a", "session-a", "session-b"} {
		if err := recorder.RecordFrameWithLineage(
			uint64(sequence+1),
			now.Add(time.Duration(sequence)*time.Millisecond).UnixNano(),
			1,
			1,
			[]floor.Tile{{X: 0, Y: 0}},
			FrameLineage{SessionID: sessionID},
		); err != nil {
			t.Fatal(err)
		}
	}
	if err := recorder.Close(); err != nil {
		t.Fatal(err)
	}

	var sessionIDs []string
	paths := []string{
		path,
		filepath.Join(filepath.Dir(path), "frames-000002.pbstream"),
	}
	for _, segmentPath := range paths {
		count, err := ReadRecoverableFrameRecords(segmentPath, "none", func(record *recordingpb.FrameRecord) error {
			sessionIDs = append(sessionIDs, record.SessionId)
			return nil
		})
		if err != nil {
			t.Fatal(err)
		}
		if count == 0 {
			t.Fatalf("segment %s is empty", segmentPath)
		}
	}
	if got := strings.Join(sessionIDs, ","); got != "session-a,session-a,session-b" {
		t.Fatalf("session ids = %s, want session-a,session-a,session-b", got)
	}
	if stats := recorder.Stats(); stats.SegmentIndex != 2 || stats.WrittenFrames != 3 {
		t.Fatalf("stats after session rotation = %+v, want segment 2 and 3 frames", stats)
	}
}

func TestFrameRecorderPostCompressesClosedRawSegmentWithZstd(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "frames.pbstream")
	zstd := fakeZstd(t)
	recorder, err := NewFrameRecorderWithOptions(path, Options{
		Compression:     "none",
		PostCompression: "zstd",
		ZstdPath:        zstd,
		DeleteRawAfter:  time.Hour,
	})
	if err != nil {
		t.Fatal(err)
	}

	if err := recorder.RecordFrame(1, time.Now().UnixNano(), 1, 1, []floor.Tile{{X: 0, Y: 0, R: 1}}); err != nil {
		t.Fatal(err)
	}
	if err := recorder.Close(); err != nil {
		t.Fatal(err)
	}

	if _, err := os.Stat(path); err != nil {
		t.Fatalf("raw segment should remain until retention expires: %v", err)
	}
	if _, err := os.Stat(path + ".zst"); err != nil {
		t.Fatalf("compressed segment missing: %v", err)
	}
	if stats := recorder.Stats(); stats.CompressedSegments != 1 || stats.PostCompression != "zstd" {
		t.Fatalf("unexpected compression stats: %+v", stats)
	}
}

func TestFrameRecorderDeletesRawAfterVerifiedZstdRetention(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "frames.pbstream")
	zstd := fakeZstd(t)
	recorder, err := NewFrameRecorderWithOptions(path, Options{
		Compression:     "none",
		PostCompression: "zstd",
		ZstdPath:        zstd,
		DeleteRawAfter:  time.Nanosecond,
	})
	if err != nil {
		t.Fatal(err)
	}

	if err := recorder.RecordFrame(1, time.Now().UnixNano(), 1, 1, []floor.Tile{{X: 0, Y: 0, R: 1}}); err != nil {
		t.Fatal(err)
	}
	if err := recorder.Close(); err != nil {
		t.Fatal(err)
	}
	time.Sleep(time.Millisecond)
	recorder.cleanupRawSegments(time.Now())

	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Fatalf("raw segment should be deleted after verified compressed retention, stat err=%v", err)
	}
	if _, err := os.Stat(path + ".zst"); err != nil {
		t.Fatalf("compressed segment should remain: %v", err)
	}
}

func TestRecoverRecordingDirectoryFinalizesOpenSegmentsAndDropsTmp(t *testing.T) {
	dir := t.TempDir()
	openPath := filepath.Join(dir, "frames.pbstream.open")
	tmpPath := filepath.Join(dir, "frames.pbstream.zst.tmp")
	if err := os.WriteFile(openPath, []byte("raw"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(tmpPath, []byte("tmp"), 0o644); err != nil {
		t.Fatal(err)
	}

	if err := recoverRecordingDirectory(dir); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(dir, "frames.pbstream")); err != nil {
		t.Fatalf("open segment should be finalized: %v", err)
	}
	if _, err := os.Stat(openPath); !os.IsNotExist(err) {
		t.Fatalf("open segment should be gone, stat err=%v", err)
	}
	if _, err := os.Stat(tmpPath); !os.IsNotExist(err) {
		t.Fatalf("tmp compressed segment should be removed, stat err=%v", err)
	}
}

func TestFrameRecorderStopsBeforeRawBacklogExceedsLimit(t *testing.T) {
	path := filepath.Join(t.TempDir(), "frames.pbstream")
	recorder, err := NewFrameRecorderWithOptions(path, Options{
		Compression:        "none",
		PostCompression:    "zstd",
		ZstdPath:           filepath.Join(t.TempDir(), "missing-zstd"),
		MaxPendingRawBytes: 1,
	})
	if err != nil {
		t.Fatal(err)
	}

	if err := recorder.RecordFrame(1, time.Now().UnixNano(), 1, 1, []floor.Tile{{X: 0, Y: 0, R: 1}}); err != nil {
		t.Fatal(err)
	}
	if err := recorder.Close(); err == nil {
		t.Fatal("expected recorder close to report raw backlog limit error")
	}
	if stats := recorder.Stats(); stats.Error == "" {
		t.Fatalf("expected raw backlog error in stats: %+v", stats)
	}
}

func TestRecoverableReaderStopsBeforeCrashTruncatedGzipMember(t *testing.T) {
	path := filepath.Join(t.TempDir(), "crash.pbstream.gz")
	var file bytes.Buffer
	for sequence := uint64(1); sequence <= 3; sequence++ {
		payload := marshalTestRecord(t, sequence)
		frame, err := encodeFrame(payload, "gzip")
		if err != nil {
			t.Fatal(err)
		}
		if sequence == 3 {
			file.Write(frame[:len(frame)/2])
			continue
		}
		file.Write(frame)
	}
	if err := os.WriteFile(path, file.Bytes(), 0o644); err != nil {
		t.Fatal(err)
	}

	var sequences []uint64
	count, err := ReadRecoverableFrameRecords(path, "gzip", func(record *recordingpb.FrameRecord) error {
		sequences = append(sequences, record.Sequence)
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if count != 2 || len(sequences) != 2 || sequences[0] != 1 || sequences[1] != 2 {
		t.Fatalf("recovered sequences = %v count=%d, want [1 2]", sequences, count)
	}
}

func marshalTestRecord(t *testing.T, sequence uint64) []byte {
	t.Helper()

	payload, err := proto.Marshal(&recordingpb.FrameRecord{
		Sequence:  sequence,
		UnixNanos: time.Now().UnixNano(),
		Width:     1,
		Height:    1,
		Tiles: []*recordingpb.TileState{
			{X: 0, Y: 0, R: uint32(sequence)},
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	return payload
}

func readFrameRecord(t *testing.T, path string, compressed bool) *recordingpb.FrameRecord {
	t.Helper()

	file, err := os.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer file.Close()

	source := io.Reader(file)
	if compressed {
		gzipReader, err := gzip.NewReader(file)
		if err != nil {
			t.Fatal(err)
		}
		defer gzipReader.Close()
		source = gzipReader
	}

	reader := bufio.NewReader(source)
	length, err := binary.ReadUvarint(reader)
	if err != nil {
		t.Fatal(err)
	}
	payload := make([]byte, length)
	if _, err := io.ReadFull(reader, payload); err != nil {
		t.Fatal(err)
	}

	record := &recordingpb.FrameRecord{}
	if err := proto.Unmarshal(payload, record); err != nil {
		t.Fatal(err)
	}
	return record
}

func TestResolveFramePathUsesSessionFilesForDefaultTargets(t *testing.T) {
	now := time.Date(2026, 6, 2, 15, 45, 30, 0, time.UTC)

	if got, want := ResolveFramePath("recordings", now, "none"), filepath.Join("recordings", "20260602T154530Z.frames.pbstream"); got != want {
		t.Fatalf("directory recording path = %q, want %q", got, want)
	}
	if got, want := ResolveFramePath("recordings/live.frames.pbstream", now, "none"), filepath.Join("recordings", "20260602T154530Z.frames.pbstream"); got != want {
		t.Fatalf("legacy live recording path = %q, want %q", got, want)
	}
	if got := ResolveFramePath("recordings/manual.pbstream", now, "none"); got != "recordings/manual.pbstream" {
		t.Fatalf("explicit recording path = %q, want recordings/manual.pbstream", got)
	}
	if got, want := ResolveFramePath("recordings", now, "gzip"), filepath.Join("recordings", "20260602T154530Z.frames.pbstream.gz"); got != want {
		t.Fatalf("gzip recording path = %q, want %q", got, want)
	}
	if got := ResolveFramePath("recordings/manual.pbstream.gz", now, "gzip"); got != "recordings/manual.pbstream.gz" {
		t.Fatalf("explicit gzip path = %q, want recordings/manual.pbstream.gz", got)
	}
}

func TestResolveFramePathForControllerUsesControllerDirectory(t *testing.T) {
	now := time.Date(2026, 6, 2, 15, 45, 30, 0, time.UTC)
	controllerID := "01234567-89ab-4def-8123-456789abcdef"

	if got, want := ResolveFramePathForController("recordings", now, "gzip", controllerID), filepath.Join("recordings", controllerID, "20260602T154530Z.frames.pbstream.gz"); got != want {
		t.Fatalf("controller directory recording path = %q, want %q", got, want)
	}
	if got, want := ResolveFramePathForController("recordings/live.frames.pbstream", now, "none", controllerID), filepath.Join("recordings", controllerID, "20260602T154530Z.frames.pbstream"); got != want {
		t.Fatalf("legacy live controller recording path = %q, want %q", got, want)
	}
	if got := ResolveFramePathForController("recordings/manual.pbstream", now, "none", controllerID); got != "recordings/manual.pbstream" {
		t.Fatalf("explicit controller recording path = %q, want recordings/manual.pbstream", got)
	}
}

func TestUnsupportedCompressionIsRejected(t *testing.T) {
	if _, err := NewFrameRecorderWithOptions(t.TempDir()+"/frames.pbstream", Options{Compression: "brotli"}); err == nil {
		t.Fatal("expected unsupported compression error")
	}
}

func fakeZstd(t *testing.T) string {
	t.Helper()

	path := filepath.Join(t.TempDir(), "zstd")
	script := `#!/bin/sh
if [ "$1" = "-t" ]; then
  exit 0
fi
input=""
output=""
while [ $# -gt 0 ]; do
  case "$1" in
    -o)
      shift
      output="$1"
      ;;
    -*)
      ;;
    *)
      input="$1"
      ;;
  esac
  shift
done
cp "$input" "$output"
`
	if err := os.WriteFile(path, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	return path
}
