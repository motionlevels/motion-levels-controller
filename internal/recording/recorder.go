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
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/lobis/motion-levels/floor-controller/internal/floor"
	"github.com/lobis/motion-levels/packages/contracts/recordingpb"
	"google.golang.org/protobuf/proto"
)

const defaultFrameQueueSize = 900
const defaultCompressionQueueSize = 1024

const DefaultMaxSegmentBytes int64 = 256 << 20
const DefaultMaxPendingRawBytes int64 = 4 << 30

var DefaultMaxSegmentDuration = 10 * time.Minute
var DefaultDeleteRawAfter = time.Hour

type FrameRecorder struct {
	stateMu sync.Mutex

	file             *os.File
	writer           *bufio.Writer
	controllerID     string
	recordingDir     string
	basePath         string
	path             string
	activePath       string
	codec            string
	postCompression  string
	zstdPath         string
	maxSegmentSize   int64
	maxSegmentAge    time.Duration
	maxPendingRaw    int64
	deleteRawAfter   time.Duration
	segmentIndex     int
	segmentBytes     int64
	segmentStartedAt time.Time

	jobs            chan frameJob
	done            chan struct{}
	compressionJobs chan segmentJob
	compressionDone chan struct{}

	closed                bool
	writeErr              error
	compressionErr        string
	dropped               uint64
	written               uint64
	compressedSegments    uint64
	rawDeleted            uint64
	pendingRawBytes       int64
	compressionQueueDrops uint64
	queuedCompression     map[string]bool
}

type frameJob struct {
	sequence  uint64
	unixNanos int64
	width     uint32
	height    uint32
	tiles     []floor.Tile
}

type segmentJob struct {
	path string
}

type Stats struct {
	ControllerID          string `json:"controllerId,omitempty"`
	Path                  string `json:"path"`
	ActivePath            string `json:"activePath,omitempty"`
	Compression           string `json:"compression"`
	PostCompression       string `json:"postCompression"`
	SegmentIndex          int    `json:"segmentIndex"`
	SegmentBytes          int64  `json:"segmentBytes"`
	MaxSegmentBytes       int64  `json:"maxSegmentBytes"`
	MaxSegmentSeconds     int64  `json:"maxSegmentSeconds"`
	PendingRawBytes       int64  `json:"pendingRawBytes"`
	MaxPendingRawBytes    int64  `json:"maxPendingRawBytes"`
	DeleteRawAfterSeconds int64  `json:"deleteRawAfterSeconds"`
	QueueDepth            int    `json:"queueDepth"`
	QueueCapacity         int    `json:"queueCapacity"`
	CompressionQueueDepth int    `json:"compressionQueueDepth"`
	WrittenFrames         uint64 `json:"writtenFrames"`
	DroppedFrames         uint64 `json:"droppedFrames"`
	CompressedSegments    uint64 `json:"compressedSegments"`
	DeletedRawSegments    uint64 `json:"deletedRawSegments"`
	Error                 string `json:"error,omitempty"`
	CompressionError      string `json:"compressionError,omitempty"`
}

type Options struct {
	Compression        string
	PostCompression    string
	ZstdPath           string
	MaxSegmentBytes    int64
	MaxSegmentDuration time.Duration
	MaxPendingRawBytes int64
	DeleteRawAfter     time.Duration
	ControllerID       string
}

func NewFrameRecorder(path string) (*FrameRecorder, error) {
	return NewFrameRecorderWithOptions(path, Options{Compression: "none"})
}

func NewFrameRecorderWithOptions(path string, options Options) (*FrameRecorder, error) {
	compression := NormalizeCompression(options.Compression)
	if !SupportedCompression(compression) {
		return nil, fmt.Errorf("unsupported recording compression %q", options.Compression)
	}
	postCompression := NormalizePostCompression(options.PostCompression)
	if !SupportedPostCompression(postCompression) {
		return nil, fmt.Errorf("unsupported post recording compression %q", options.PostCompression)
	}
	return newFrameRecorderAt(path, time.Now().UTC(), compression, postCompression, normalizeOptions(options))
}

func normalizeOptions(options Options) Options {
	options.ControllerID = strings.TrimSpace(options.ControllerID)
	options.MaxSegmentBytes = normalizeMaxSegmentBytes(options.MaxSegmentBytes)
	if options.MaxSegmentDuration <= 0 {
		options.MaxSegmentDuration = DefaultMaxSegmentDuration
	}
	if options.MaxPendingRawBytes <= 0 {
		options.MaxPendingRawBytes = DefaultMaxPendingRawBytes
	}
	if options.DeleteRawAfter < 0 {
		options.DeleteRawAfter = 0
	}
	if options.DeleteRawAfter == 0 {
		options.DeleteRawAfter = DefaultDeleteRawAfter
	}
	if strings.TrimSpace(options.ZstdPath) == "" {
		options.ZstdPath = "zstd"
	}
	return options
}

func newFrameRecorderAt(path string, now time.Time, compression string, postCompression string, options Options) (*FrameRecorder, error) {
	if path == "" {
		return nil, nil
	}
	resolvedPath := ResolveFramePathForController(path, now, compression, options.ControllerID)
	recordingDir := filepath.Dir(resolvedPath)
	if err := os.MkdirAll(recordingDir, 0o755); err != nil {
		return nil, err
	}
	if err := recoverRecordingDirectory(recordingDir); err != nil {
		return nil, err
	}

	file, actualPath, activePath, err := openSegment(resolvedPath)
	if err != nil {
		return nil, err
	}

	recorder := &FrameRecorder{
		file:              file,
		writer:            bufio.NewWriterSize(file, 1<<20),
		controllerID:      options.ControllerID,
		recordingDir:      recordingDir,
		basePath:          actualPath,
		path:              actualPath,
		activePath:        activePath,
		codec:             compression,
		postCompression:   postCompression,
		zstdPath:          options.ZstdPath,
		maxSegmentSize:    options.MaxSegmentBytes,
		maxSegmentAge:     options.MaxSegmentDuration,
		maxPendingRaw:     options.MaxPendingRawBytes,
		deleteRawAfter:    options.DeleteRawAfter,
		segmentIndex:      1,
		segmentStartedAt:  now,
		jobs:              make(chan frameJob, defaultFrameQueueSize),
		done:              make(chan struct{}),
		compressionJobs:   make(chan segmentJob, defaultCompressionQueueSize),
		compressionDone:   make(chan struct{}),
		queuedCompression: make(map[string]bool),
	}
	recorder.pendingRawBytes = recorder.scanPendingRawBytes()
	recorder.queueExistingRawSegments()
	go recorder.run()
	go recorder.runCompressor()
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

func NormalizePostCompression(compression string) string {
	switch strings.ToLower(strings.TrimSpace(compression)) {
	case "", "none", "off", "false":
		return "none"
	case "zstd", "zst":
		return "zstd"
	default:
		return strings.ToLower(strings.TrimSpace(compression))
	}
}

func SupportedPostCompression(compression string) bool {
	switch NormalizePostCompression(compression) {
	case "none", "zstd":
		return true
	default:
		return false
	}
}

func ResolveFramePath(path string, now time.Time, compression string) string {
	return ResolveFramePathForController(path, now, compression, "")
}

func ResolveFramePathForController(path string, now time.Time, compression string, controllerID string) string {
	cleanPath := filepath.Clean(path)
	if filepath.Ext(cleanPath) == "" || strings.HasSuffix(path, string(os.PathSeparator)) || filepath.Base(cleanPath) == "live.frames.pbstream" {
		dir := cleanPath
		if filepath.Ext(cleanPath) != "" {
			dir = filepath.Dir(cleanPath)
		}
		if strings.TrimSpace(controllerID) != "" {
			dir = filepath.Join(dir, strings.TrimSpace(controllerID))
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

func openSegment(path string) (*os.File, string, string, error) {
	for attempt := 0; ; attempt++ {
		candidate := path
		if attempt > 0 {
			ext := filepath.Ext(path)
			base := strings.TrimSuffix(path, ext)
			candidate = fmt.Sprintf("%s-%d%s", base, attempt, ext)
		}
		if _, err := os.Stat(candidate); err == nil {
			continue
		} else if err != nil && !errors.Is(err, os.ErrNotExist) {
			return nil, "", "", err
		}
		activePath := candidate + ".open"
		file, err := os.OpenFile(activePath, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o644)
		if err == nil {
			return file, candidate, activePath, nil
		}
		if !errors.Is(err, os.ErrExist) {
			return nil, "", "", err
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
		ControllerID:          r.controllerID,
		Path:                  r.path,
		ActivePath:            r.activePath,
		Compression:           r.codec,
		PostCompression:       r.postCompression,
		SegmentIndex:          r.segmentIndex,
		SegmentBytes:          r.segmentBytes,
		MaxSegmentBytes:       r.maxSegmentSize,
		MaxSegmentSeconds:     int64(r.maxSegmentAge.Seconds()),
		PendingRawBytes:       r.pendingRawBytes + r.segmentBytes,
		MaxPendingRawBytes:    r.maxPendingRaw,
		DeleteRawAfterSeconds: int64(r.deleteRawAfter.Seconds()),
		QueueDepth:            len(r.jobs),
		QueueCapacity:         cap(r.jobs),
		CompressionQueueDepth: len(r.compressionJobs),
		WrittenFrames:         r.written,
		DroppedFrames:         r.dropped,
		CompressedSegments:    r.compressedSegments,
		DeletedRawSegments:    r.rawDeleted,
	}
	if r.writeErr != nil {
		stats.Error = r.writeErr.Error()
	}
	if r.compressionErr != "" {
		stats.CompressionError = r.compressionErr
	}
	return stats
}

func (r *FrameRecorder) run() {
	defer close(r.done)
	flushTicker := time.NewTicker(time.Second)
	defer flushTicker.Stop()
	maintenanceTicker := time.NewTicker(time.Minute)
	defer maintenanceTicker.Stop()

	for {
		select {
		case job, ok := <-r.jobs:
			if !ok {
				r.setWriteErr(r.flushAndClose())
				close(r.compressionJobs)
				return
			}
			if err := r.writeFrame(job); err != nil {
				r.setWriteErr(err)
			} else {
				r.markWritten()
			}
		case <-flushTicker.C:
			r.setWriteErr(r.flush())
		case <-maintenanceTicker.C:
			r.cleanupRawSegments(time.Now())
			r.queueExistingRawSegments()
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
	now := time.Unix(0, job.unixNanos)
	if now.IsZero() {
		now = time.Now()
	}
	if err := r.rotateIfNeeded(int64(len(data)), now); err != nil {
		return err
	}
	if err := r.checkRawBudget(int64(len(data))); err != nil {
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

func (r *FrameRecorder) checkRawBudget(nextFrameBytes int64) error {
	r.stateMu.Lock()
	defer r.stateMu.Unlock()
	if r.maxPendingRaw <= 0 {
		return nil
	}
	if r.pendingRawBytes+r.segmentBytes+nextFrameBytes <= r.maxPendingRaw {
		return nil
	}
	return fmt.Errorf("pending raw recording bytes exceeded limit: pending=%d active=%d next=%d max=%d", r.pendingRawBytes, r.segmentBytes, nextFrameBytes, r.maxPendingRaw)
}

func (r *FrameRecorder) rotateIfNeeded(nextFrameBytes int64, now time.Time) error {
	r.stateMu.Lock()
	bySize := r.maxSegmentSize > 0 && r.segmentBytes > 0 && r.segmentBytes+nextFrameBytes > r.maxSegmentSize
	byAge := r.maxSegmentAge > 0 && r.segmentBytes > 0 && !now.Before(r.segmentStartedAt.Add(r.maxSegmentAge))
	shouldRotate := bySize || byAge
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
	file, actualPath, activePath, err := openSegment(nextPath)
	if err != nil {
		return err
	}

	r.stateMu.Lock()
	r.file = file
	r.writer = bufio.NewWriterSize(file, 1<<20)
	r.path = actualPath
	r.activePath = activePath
	r.segmentIndex = nextIndex
	r.segmentBytes = 0
	r.segmentStartedAt = now
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
	if r.file == nil {
		return nil
	}
	r.stateMu.Lock()
	activePath := r.activePath
	finalPath := r.path
	segmentBytes := r.segmentBytes
	r.stateMu.Unlock()

	var errs []error
	errs = append(errs, r.flush())
	errs = append(errs, r.file.Close())
	if err := errors.Join(errs...); err != nil {
		return err
	}
	if segmentBytes == 0 {
		if err := os.Remove(activePath); err != nil && !errors.Is(err, os.ErrNotExist) {
			return err
		}
		return nil
	}
	if err := os.Rename(activePath, finalPath); err != nil {
		return err
	}
	r.addPendingRaw(segmentBytes)
	r.enqueueCompression(finalPath)
	return nil
}

func (r *FrameRecorder) runCompressor() {
	defer close(r.compressionDone)
	for job := range r.compressionJobs {
		if r.postCompression == "zstd" {
			if err := compressWithZstd(r.zstdPath, job.path); err != nil {
				r.clearCompressionTarget(job.path)
				r.setCompressionErr(err)
				continue
			}
			r.markCompressed()
		}
		r.clearCompressionTarget(job.path)
		r.cleanupRawSegments(time.Now())
	}
}

func (r *FrameRecorder) enqueueCompression(path string) {
	if r == nil || r.postCompression == "none" || path == "" {
		return
	}
	r.stateMu.Lock()
	if r.queuedCompression[path] {
		r.stateMu.Unlock()
		return
	}
	r.queuedCompression[path] = true
	r.stateMu.Unlock()

	select {
	case r.compressionJobs <- segmentJob{path: path}:
	default:
		r.stateMu.Lock()
		delete(r.queuedCompression, path)
		r.compressionQueueDrops++
		r.compressionErr = fmt.Sprintf("compression queue full; will retry during maintenance scan: %s", path)
		r.stateMu.Unlock()
	}
}

func (r *FrameRecorder) clearCompressionTarget(path string) {
	r.stateMu.Lock()
	defer r.stateMu.Unlock()
	delete(r.queuedCompression, path)
}

func (r *FrameRecorder) queueExistingRawSegments() {
	if r == nil || r.postCompression == "none" || r.recordingDir == "" {
		return
	}
	_ = filepath.WalkDir(r.recordingDir, func(path string, entry os.DirEntry, err error) error {
		if err != nil || entry.IsDir() || !isClosedRecordingSegment(path) {
			return nil
		}
		if zstdVerified(r.zstdPath, zstdPathFor(path)) {
			return nil
		}
		r.enqueueCompression(path)
		return nil
	})
}

func (r *FrameRecorder) cleanupRawSegments(now time.Time) {
	if r == nil || r.postCompression != "zstd" || r.recordingDir == "" {
		return
	}
	_ = filepath.WalkDir(r.recordingDir, func(path string, entry os.DirEntry, err error) error {
		if err != nil || entry.IsDir() || !isClosedRecordingSegment(path) {
			return nil
		}
		zst := zstdPathFor(path)
		info, err := os.Stat(zst)
		if err != nil || now.Sub(info.ModTime()) < r.deleteRawAfter {
			return nil
		}
		if !zstdVerified(r.zstdPath, zst) {
			return nil
		}
		rawInfo, err := os.Stat(path)
		if err != nil {
			return nil
		}
		if err := os.Remove(path); err != nil {
			r.setCompressionErr(err)
			return nil
		}
		r.subtractPendingRaw(rawInfo.Size())
		r.markRawDeleted()
		return nil
	})
}

func recoverRecordingDirectory(dir string) error {
	if dir == "" {
		return nil
	}
	return filepath.WalkDir(dir, func(path string, entry os.DirEntry, err error) error {
		if err != nil || entry.IsDir() {
			return nil
		}
		if strings.HasSuffix(path, ".zst.tmp") {
			return os.Remove(path)
		}
		if !strings.HasSuffix(path, ".open") {
			return nil
		}
		finalPath := strings.TrimSuffix(path, ".open")
		if _, err := os.Stat(finalPath); err == nil {
			return os.Remove(path)
		}
		return os.Rename(path, finalPath)
	})
}

func compressWithZstd(zstdPath, rawPath string) error {
	finalPath := zstdPathFor(rawPath)
	if zstdVerified(zstdPath, finalPath) {
		return nil
	}
	tmpPath := finalPath + ".tmp"
	_ = os.Remove(tmpPath)
	cmd := exec.Command(zstdPath, "-19", "--long=27", "-T1", "-f", "-q", rawPath, "-o", tmpPath)
	if output, err := cmd.CombinedOutput(); err != nil {
		_ = os.Remove(tmpPath)
		return fmt.Errorf("zstd compress %s: %w: %s", rawPath, err, strings.TrimSpace(string(output)))
	}
	if !zstdVerified(zstdPath, tmpPath) {
		_ = os.Remove(tmpPath)
		return fmt.Errorf("zstd verification failed for %s", tmpPath)
	}
	if err := os.Rename(tmpPath, finalPath); err != nil {
		_ = os.Remove(tmpPath)
		return err
	}
	return nil
}

func zstdVerified(zstdPath, path string) bool {
	if path == "" {
		return false
	}
	if _, err := os.Stat(path); err != nil {
		return false
	}
	cmd := exec.Command(zstdPath, "-t", "-q", path)
	return cmd.Run() == nil
}

func zstdPathFor(path string) string {
	return path + ".zst"
}

func isClosedRecordingSegment(path string) bool {
	base := filepath.Base(path)
	if !strings.Contains(base, ".pbstream") {
		return false
	}
	if strings.HasSuffix(base, ".open") || strings.HasSuffix(base, ".tmp") || strings.HasSuffix(base, ".zst") {
		return false
	}
	return true
}

func (r *FrameRecorder) scanPendingRawBytes() int64 {
	var total int64
	_ = filepath.WalkDir(r.recordingDir, func(path string, entry os.DirEntry, err error) error {
		if err != nil || entry.IsDir() || !isClosedRecordingSegment(path) {
			return nil
		}
		info, err := entry.Info()
		if err != nil {
			return nil
		}
		total += info.Size()
		return nil
	})
	return total
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

func (r *FrameRecorder) setCompressionErr(err error) {
	if err == nil {
		return
	}
	r.stateMu.Lock()
	defer r.stateMu.Unlock()
	r.compressionErr = err.Error()
}

func (r *FrameRecorder) addPendingRaw(size int64) {
	r.stateMu.Lock()
	defer r.stateMu.Unlock()
	r.pendingRawBytes += size
}

func (r *FrameRecorder) subtractPendingRaw(size int64) {
	r.stateMu.Lock()
	defer r.stateMu.Unlock()
	r.pendingRawBytes -= size
	if r.pendingRawBytes < 0 {
		r.pendingRawBytes = 0
	}
}

func (r *FrameRecorder) markWritten() {
	r.stateMu.Lock()
	defer r.stateMu.Unlock()
	r.written++
}

func (r *FrameRecorder) markCompressed() {
	r.stateMu.Lock()
	defer r.stateMu.Unlock()
	r.compressedSegments++
	r.compressionErr = ""
}

func (r *FrameRecorder) markRawDeleted() {
	r.stateMu.Lock()
	defer r.stateMu.Unlock()
	r.rawDeleted++
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
	<-r.compressionDone

	r.stateMu.Lock()
	defer r.stateMu.Unlock()
	return r.writeErr
}
