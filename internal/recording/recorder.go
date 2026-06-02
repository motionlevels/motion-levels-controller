package recording

import (
	"bufio"
	"bytes"
	"compress/gzip"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/lobis/motion-levels/floor-controller/internal/floor"
	"github.com/lobis/motion-levels/packages/contracts/recordingpb"
	"google.golang.org/protobuf/proto"
)

const defaultFrameQueueSize = 900
const DefaultMaxSegmentBytes int64 = 1 << 30

type FrameRecorder struct {
	stateMu        sync.Mutex
	file           *os.File
	writer         *bufio.Writer
	basePath       string
	path           string
	codec          string
	maxSegmentSize int64
	segmentIndex   int
	segmentBytes   int64
	jobs           chan frameJob
	done           chan struct{}
	closed         bool
	writeErr       error
	dropped        uint64
	written        uint64
}

type frameJob struct {
	sequence  uint64
	unixNanos int64
	width     uint32
	height    uint32
	tiles     []floor.Tile
}

type Stats struct {
	Path            string `json:"path"`
	Compression     string `json:"compression"`
	SegmentIndex    int    `json:"segmentIndex"`
	SegmentBytes    int64  `json:"segmentBytes"`
	MaxSegmentBytes int64  `json:"maxSegmentBytes"`
	QueueDepth      int    `json:"queueDepth"`
	QueueCapacity   int    `json:"queueCapacity"`
	WrittenFrames   uint64 `json:"writtenFrames"`
	DroppedFrames   uint64 `json:"droppedFrames"`
	Error           string `json:"error,omitempty"`
}

type Options struct {
	Compression     string
	MaxSegmentBytes int64
}

func NewFrameRecorder(path string) (*FrameRecorder, error) {
	return NewFrameRecorderWithOptions(path, Options{Compression: "none"})
}

func NewFrameRecorderWithOptions(path string, options Options) (*FrameRecorder, error) {
	compression := NormalizeCompression(options.Compression)
	if !SupportedCompression(compression) {
		return nil, fmt.Errorf("unsupported recording compression %q", options.Compression)
	}
	return newFrameRecorderAt(path, time.Now().UTC(), compression, normalizeMaxSegmentBytes(options.MaxSegmentBytes))
}

func newFrameRecorderAt(path string, now time.Time, compression string, maxSegmentBytes int64) (*FrameRecorder, error) {
	if path == "" {
		return nil, nil
	}
	compression = NormalizeCompression(compression)
	if !SupportedCompression(compression) {
		return nil, fmt.Errorf("unsupported recording compression %q", compression)
	}
	maxSegmentBytes = normalizeMaxSegmentBytes(maxSegmentBytes)
	resolvedPath := ResolveFramePath(path, now, compression)
	if err := os.MkdirAll(filepath.Dir(resolvedPath), 0o755); err != nil {
		return nil, err
	}
	file, actualPath, err := openUniqueFile(resolvedPath)
	if err != nil {
		return nil, err
	}
	recorder := &FrameRecorder{
		file:           file,
		writer:         bufio.NewWriterSize(file, 1<<20),
		basePath:       actualPath,
		path:           actualPath,
		codec:          compression,
		maxSegmentSize: maxSegmentBytes,
		segmentIndex:   1,
		jobs:           make(chan frameJob, defaultFrameQueueSize),
		done:           make(chan struct{}),
	}
	go recorder.run()
	return recorder, nil
}

func normalizeMaxSegmentBytes(size int64) int64 {
	if size <= 0 {
		return DefaultMaxSegmentBytes
	}
	return size
}

func NormalizeCompression(compression string) string {
	switch strings.ToLower(strings.TrimSpace(compression)) {
	case "", "none", "off", "false":
		return "none"
	case "gzip", "gz":
		return "gzip"
	default:
		return strings.ToLower(strings.TrimSpace(compression))
	}
}

func SupportedCompression(compression string) bool {
	switch NormalizeCompression(compression) {
	case "none", "gzip":
		return true
	default:
		return false
	}
}

func ResolveFramePath(path string, now time.Time, compression string) string {
	cleanPath := filepath.Clean(path)
	if filepath.Ext(cleanPath) == "" || strings.HasSuffix(path, string(os.PathSeparator)) || filepath.Base(cleanPath) == "live.frames.pbstream" {
		dir := cleanPath
		if filepath.Ext(cleanPath) != "" {
			dir = filepath.Dir(cleanPath)
		}
		return withCompressionExtension(filepath.Join(dir, now.Format("20060102T150405Z")+".frames.pbstream"), compression)
	}
	return withCompressionExtension(cleanPath, compression)
}

func withCompressionExtension(path, compression string) string {
	if NormalizeCompression(compression) != "gzip" || strings.HasSuffix(path, ".gz") {
		return path
	}
	return path + ".gz"
}

func openUniqueFile(path string) (*os.File, string, error) {
	for attempt := 0; ; attempt++ {
		candidate := path
		if attempt > 0 {
			ext := filepath.Ext(path)
			base := strings.TrimSuffix(path, ext)
			candidate = fmt.Sprintf("%s-%d%s", base, attempt, ext)
		}
		file, err := os.OpenFile(candidate, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o644)
		if err == nil {
			return file, candidate, nil
		}
		if !errors.Is(err, os.ErrExist) {
			return nil, "", err
		}
	}
}

func (r *FrameRecorder) RecordFrame(sequence uint64, unixNanos int64, width, height uint32, tiles []floor.Tile) error {
	if r == nil {
		return nil
	}

	copiedTiles := append([]floor.Tile(nil), tiles...)
	job := frameJob{
		sequence:  sequence,
		unixNanos: unixNanos,
		width:     width,
		height:    height,
		tiles:     copiedTiles,
	}

	r.stateMu.Lock()
	defer r.stateMu.Unlock()
	if r.writeErr != nil {
		return r.writeErr
	}
	if r.closed {
		return errors.New("frame recorder is closed")
	}
	select {
	case r.jobs <- job:
	default:
		r.dropped++
	}
	return nil
}

func (r *FrameRecorder) DroppedFrames() uint64 {
	return r.Stats().DroppedFrames
}

func (r *FrameRecorder) Path() string {
	if r == nil {
		return ""
	}
	r.stateMu.Lock()
	defer r.stateMu.Unlock()
	return r.path
}

func (r *FrameRecorder) Stats() Stats {
	if r == nil {
		return Stats{}
	}

	r.stateMu.Lock()
	defer r.stateMu.Unlock()
	stats := Stats{
		Path:            r.path,
		Compression:     r.codec,
		SegmentIndex:    r.segmentIndex,
		SegmentBytes:    r.segmentBytes,
		MaxSegmentBytes: r.maxSegmentSize,
		QueueDepth:      len(r.jobs),
		QueueCapacity:   cap(r.jobs),
		WrittenFrames:   r.written,
		DroppedFrames:   r.dropped,
	}
	if r.writeErr != nil {
		stats.Error = r.writeErr.Error()
	}
	return stats
}

func (r *FrameRecorder) run() {
	defer close(r.done)
	ticker := time.NewTicker(time.Second)
	defer ticker.Stop()

	for {
		select {
		case job, ok := <-r.jobs:
			if !ok {
				r.setWriteErr(r.flushAndClose())
				return
			}
			if err := r.writeFrame(job); err != nil {
				r.setWriteErr(err)
			} else {
				r.markWritten()
			}
		case <-ticker.C:
			r.setWriteErr(r.flush())
		}
	}
}

func (r *FrameRecorder) writeFrame(job frameJob) error {
	record := &recordingpb.FrameRecord{
		Sequence:  job.sequence,
		UnixNanos: job.unixNanos,
		Width:     job.width,
		Height:    job.height,
		Tiles:     make([]*recordingpb.TileState, 0, len(job.tiles)),
	}
	for _, tile := range job.tiles {
		record.Tiles = append(record.Tiles, &recordingpb.TileState{
			X:       uint32(tile.X),
			Y:       uint32(tile.Y),
			R:       uint32(tile.R),
			G:       uint32(tile.G),
			B:       uint32(tile.B),
			Pressed: tile.Pressed,
		})
	}

	payload, err := proto.Marshal(record)
	if err != nil {
		return err
	}

	data, err := encodeFrame(payload, r.codec)
	if err != nil {
		return err
	}
	if err := r.rotateIfNeeded(int64(len(data))); err != nil {
		return err
	}
	if _, err := r.writer.Write(data); err != nil {
		return err
	}
	if err := r.writer.Flush(); err != nil {
		return err
	}
	r.stateMu.Lock()
	r.segmentBytes += int64(len(data))
	r.stateMu.Unlock()
	return nil
}

func encodeFrame(payload []byte, compression string) ([]byte, error) {
	var frame bytes.Buffer
	var length [binary.MaxVarintLen64]byte
	n := binary.PutUvarint(length[:], uint64(len(payload)))

	switch NormalizeCompression(compression) {
	case "none":
		frame.Write(length[:n])
		frame.Write(payload)
	case "gzip":
		gzipWriter := gzip.NewWriter(&frame)
		if _, err := gzipWriter.Write(length[:n]); err != nil {
			_ = gzipWriter.Close()
			return nil, err
		}
		if _, err := gzipWriter.Write(payload); err != nil {
			_ = gzipWriter.Close()
			return nil, err
		}
		if err := gzipWriter.Close(); err != nil {
			return nil, err
		}
	default:
		return nil, fmt.Errorf("unsupported recording compression %q", compression)
	}
	return frame.Bytes(), nil
}

func (r *FrameRecorder) rotateIfNeeded(nextFrameBytes int64) error {
	r.stateMu.Lock()
	shouldRotate := r.maxSegmentSize > 0 && r.segmentBytes > 0 && r.segmentBytes+nextFrameBytes > r.maxSegmentSize
	r.stateMu.Unlock()
	if !shouldRotate {
		return nil
	}
	if err := r.closeCurrentSegment(); err != nil {
		return err
	}

	nextIndex := r.segmentIndex + 1
	nextPath := segmentPath(r.basePath, nextIndex)
	if err := os.MkdirAll(filepath.Dir(nextPath), 0o755); err != nil {
		return err
	}
	file, actualPath, err := openUniqueFile(nextPath)
	if err != nil {
		return err
	}

	r.stateMu.Lock()
	r.file = file
	r.writer = bufio.NewWriterSize(file, 1<<20)
	r.path = actualPath
	r.segmentIndex = nextIndex
	r.segmentBytes = 0
	r.stateMu.Unlock()
	return nil
}

func segmentPath(path string, index int) string {
	if index <= 1 {
		return path
	}
	if strings.HasSuffix(path, ".frames.pbstream.gz") {
		base := strings.TrimSuffix(path, ".frames.pbstream.gz")
		return fmt.Sprintf("%s-%06d.frames.pbstream.gz", base, index)
	}
	if strings.HasSuffix(path, ".pbstream.gz") {
		base := strings.TrimSuffix(path, ".pbstream.gz")
		return fmt.Sprintf("%s-%06d.pbstream.gz", base, index)
	}
	if strings.HasSuffix(path, ".frames.pbstream") {
		base := strings.TrimSuffix(path, ".frames.pbstream")
		return fmt.Sprintf("%s-%06d.frames.pbstream", base, index)
	}
	if strings.HasSuffix(path, ".pbstream") {
		base := strings.TrimSuffix(path, ".pbstream")
		return fmt.Sprintf("%s-%06d.pbstream", base, index)
	}
	ext := filepath.Ext(path)
	base := strings.TrimSuffix(path, ext)
	return fmt.Sprintf("%s-%06d%s", base, index, ext)
}

func ReadRecoverableFrameRecords(path string, compression string, onFrame func(*recordingpb.FrameRecord) error) (int, error) {
	file, err := os.Open(path)
	if err != nil {
		return 0, err
	}
	defer file.Close()

	var source io.Reader = file
	if NormalizeCompression(compression) == "gzip" {
		gzipReader, err := gzip.NewReader(file)
		if err != nil {
			return 0, err
		}
		defer gzipReader.Close()
		source = gzipReader
	}

	reader := bufio.NewReader(source)
	var count int
	for {
		length, err := binary.ReadUvarint(reader)
		if errors.Is(err, io.EOF) || errors.Is(err, io.ErrUnexpectedEOF) {
			return count, nil
		}
		if err != nil {
			return count, err
		}
		payload := make([]byte, length)
		if _, err := io.ReadFull(reader, payload); err != nil {
			if errors.Is(err, io.EOF) || errors.Is(err, io.ErrUnexpectedEOF) {
				return count, nil
			}
			return count, err
		}

		var record recordingpb.FrameRecord
		if err := proto.Unmarshal(payload, &record); err != nil {
			return count, err
		}
		if err := onFrame(&record); err != nil {
			return count, err
		}
		count++
	}
}

func (r *FrameRecorder) flush() error {
	if r == nil || r.writer == nil {
		return nil
	}
	return r.writer.Flush()
}

func (r *FrameRecorder) flushAndClose() error {
	return r.closeCurrentSegment()
}

func (r *FrameRecorder) closeCurrentSegment() error {
	var errs []error
	errs = append(errs, r.flush())
	if r.file != nil {
		errs = append(errs, r.file.Close())
	}
	return errors.Join(errs...)
}

func (r *FrameRecorder) setWriteErr(err error) {
	if err == nil {
		return
	}
	r.stateMu.Lock()
	defer r.stateMu.Unlock()
	if r.writeErr == nil {
		r.writeErr = err
	}
}

func (r *FrameRecorder) markWritten() {
	r.stateMu.Lock()
	defer r.stateMu.Unlock()
	r.written++
}

func (r *FrameRecorder) Close() error {
	if r == nil {
		return nil
	}

	r.stateMu.Lock()
	if r.closed {
		err := r.writeErr
		r.stateMu.Unlock()
		return err
	}
	r.closed = true
	close(r.jobs)
	r.stateMu.Unlock()

	<-r.done

	r.stateMu.Lock()
	defer r.stateMu.Unlock()
	return r.writeErr
}
