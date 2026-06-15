package recording

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"
)

const defaultUploadQueueSize = 256

type UploadOptions struct {
	PlatformURL  string
	APIToken     string
	ControllerID string
	SessionID    string
	QueueSize    int
	HTTPTimeout  time.Duration
	MaxAttempts  int
	RetryDelay   time.Duration
}

type UploadStats struct {
	Enabled          bool   `json:"enabled"`
	PlatformURL      string `json:"platformUrl,omitempty"`
	SessionID        string `json:"sessionId,omitempty"`
	QueueDepth       int    `json:"queueDepth"`
	QueueCapacity    int    `json:"queueCapacity"`
	UploadedSegments uint64 `json:"uploadedSegments"`
	FailedSegments   uint64 `json:"failedSegments"`
	DroppedSegments  uint64 `json:"droppedSegments"`
	LastError        string `json:"lastError,omitempty"`
	LastObjectKey    string `json:"lastObjectKey,omitempty"`
}

type segmentMetadata struct {
	SessionID      string
	VenueSessionID string
	FrameCount     uint64
	FirstSequence  uint64
	LastSequence   uint64
	StartedAt      time.Time
	EndedAt        time.Time
}

type uploadJob struct {
	Path        string
	Metadata    segmentMetadata
	Compression string
	ContentType string
}

type SegmentUploader struct {
	options UploadOptions
	client  *http.Client
	jobs    chan uploadJob
	done    chan struct{}

	mu               sync.Mutex
	uploadedSegments uint64
	failedSegments   uint64
	droppedSegments  uint64
	lastError        string
	lastObjectKey    string
	closed           bool
}

func NewSegmentUploader(options UploadOptions) (*SegmentUploader, error) {
	platformURL := strings.TrimSpace(options.PlatformURL)
	if platformURL == "" {
		return nil, nil
	}
	parsed, err := url.Parse(platformURL)
	if err != nil || parsed.Scheme == "" || parsed.Host == "" {
		return nil, fmt.Errorf("record-upload-platform-url must be an absolute URL")
	}
	if options.QueueSize <= 0 {
		options.QueueSize = defaultUploadQueueSize
	}
	if options.HTTPTimeout <= 0 {
		options.HTTPTimeout = 5 * time.Minute
	}
	if options.MaxAttempts <= 0 {
		options.MaxAttempts = 5
	}
	if options.RetryDelay <= 0 {
		options.RetryDelay = time.Second
	}
	options.PlatformURL = strings.TrimRight(platformURL, "/")
	options.ControllerID = strings.TrimSpace(options.ControllerID)
	options.SessionID = strings.TrimSpace(options.SessionID)

	uploader := &SegmentUploader{
		options: options,
		client:  &http.Client{Timeout: options.HTTPTimeout},
		jobs:    make(chan uploadJob, options.QueueSize),
		done:    make(chan struct{}),
	}
	go uploader.run()
	return uploader, nil
}

func (u *SegmentUploader) Enqueue(job uploadJob) {
	if u == nil || strings.TrimSpace(job.Path) == "" {
		return
	}
	u.mu.Lock()
	if u.closed {
		u.mu.Unlock()
		return
	}
	u.mu.Unlock()

	select {
	case u.jobs <- job:
	default:
		u.mu.Lock()
		u.droppedSegments++
		u.lastError = fmt.Sprintf("recording upload queue full; dropped %s", job.Path)
		u.mu.Unlock()
	}
}

func (u *SegmentUploader) Stats() UploadStats {
	if u == nil {
		return UploadStats{}
	}
	u.mu.Lock()
	defer u.mu.Unlock()
	return UploadStats{
		Enabled:          true,
		PlatformURL:      u.options.PlatformURL,
		SessionID:        u.options.SessionID,
		QueueDepth:       len(u.jobs),
		QueueCapacity:    cap(u.jobs),
		UploadedSegments: u.uploadedSegments,
		FailedSegments:   u.failedSegments,
		DroppedSegments:  u.droppedSegments,
		LastError:        u.lastError,
		LastObjectKey:    u.lastObjectKey,
	}
}

func (u *SegmentUploader) Close() {
	if u == nil {
		return
	}
	u.mu.Lock()
	if !u.closed {
		u.closed = true
		close(u.jobs)
	}
	u.mu.Unlock()
	<-u.done
}

func (u *SegmentUploader) run() {
	defer close(u.done)
	for job := range u.jobs {
		if err := u.uploadWithRetry(job); err != nil {
			u.mu.Lock()
			u.failedSegments++
			u.lastError = err.Error()
			u.mu.Unlock()
			u.requeueAfterDelay(job)
			continue
		}
		u.mu.Lock()
		u.uploadedSegments++
		u.lastError = ""
		u.mu.Unlock()
	}
}

func (u *SegmentUploader) requeueAfterDelay(job uploadJob) {
	if u == nil {
		return
	}
	delay := u.retryDelay(u.options.MaxAttempts + 1)
	go func() {
		time.Sleep(delay)
		if _, err := os.Stat(job.Path); err != nil {
			return
		}
		u.Enqueue(job)
	}()
}

func (u *SegmentUploader) uploadWithRetry(job uploadJob) error {
	var lastErr error
	for attempt := 1; attempt <= u.options.MaxAttempts; attempt++ {
		if err := u.upload(job); err != nil {
			lastErr = err
			if attempt < u.options.MaxAttempts {
				time.Sleep(u.retryDelay(attempt))
			}
			continue
		}
		return nil
	}
	return lastErr
}

func (u *SegmentUploader) retryDelay(attempt int) time.Duration {
	delay := u.options.RetryDelay
	for i := 1; i < attempt; i++ {
		delay *= 2
	}
	maxDelay := 30 * time.Second
	if delay > maxDelay {
		return maxDelay
	}
	return delay
}

func (u *SegmentUploader) upload(job uploadJob) error {
	info, err := os.Stat(job.Path)
	if err != nil {
		return err
	}
	checksum, err := sha256File(job.Path)
	if err != nil {
		return err
	}

	initResp, err := u.initUpload(job, info.Size())
	if err != nil {
		return err
	}
	if err := u.putObject(initResp.UploadURL, job.Path, job.ContentType); err != nil {
		return err
	}
	if err := u.completeUpload(initResp.UploadID, info.Size(), checksum, job.Metadata); err != nil {
		return err
	}
	removeUploadedSegment(job.Path)
	u.mu.Lock()
	u.lastObjectKey = initResp.ObjectKey
	u.mu.Unlock()
	return nil
}

func removeUploadedSegment(path string) {
	if err := os.Remove(path); err != nil && !errors.Is(err, os.ErrNotExist) {
		fmt.Fprintf(os.Stderr, "recording upload cleanup failed: %v\n", err)
	}
	if strings.HasSuffix(path, ".zst") {
		rawPath := strings.TrimSuffix(path, ".zst")
		if err := os.Remove(rawPath); err != nil && !errors.Is(err, os.ErrNotExist) {
			fmt.Fprintf(os.Stderr, "recording raw cleanup failed: %v\n", err)
		}
	}
}

type uploadInitRequest struct {
	SessionID      string `json:"sessionId,omitempty"`
	VenueSessionID string `json:"venueSessionId,omitempty"`
	ControllerID   string `json:"controllerId,omitempty"`
	FileName       string `json:"fileName,omitempty"`
	ContentType    string `json:"contentType,omitempty"`
	Compression    string `json:"compression,omitempty"`
	ByteSize       int64  `json:"byteSize,omitempty"`
	FrameCount     uint64 `json:"frameCount,omitempty"`
	FirstSequence  uint64 `json:"firstSequence,omitempty"`
	LastSequence   uint64 `json:"lastSequence,omitempty"`
	StartedAt      string `json:"startedAt,omitempty"`
	EndedAt        string `json:"endedAt,omitempty"`
}

type uploadInitResponse struct {
	OK        bool   `json:"ok"`
	UploadID  string `json:"uploadId"`
	Bucket    string `json:"bucket"`
	ObjectKey string `json:"objectKey"`
	UploadURL string `json:"uploadUrl"`
	Error     string `json:"error"`
}

func (u *SegmentUploader) initUpload(job uploadJob, byteSize int64) (uploadInitResponse, error) {
	sessionID := strings.TrimSpace(job.Metadata.SessionID)
	if sessionID == "" {
		sessionID = u.options.SessionID
	}
	payload := uploadInitRequest{
		SessionID:      sessionID,
		VenueSessionID: job.Metadata.VenueSessionID,
		ControllerID:   u.options.ControllerID,
		FileName:       filepath.Base(job.Path),
		ContentType:    job.ContentType,
		Compression:    job.Compression,
		ByteSize:       byteSize,
		FrameCount:     job.Metadata.FrameCount,
		FirstSequence:  job.Metadata.FirstSequence,
		LastSequence:   job.Metadata.LastSequence,
		StartedAt:      formatOptionalTime(job.Metadata.StartedAt),
		EndedAt:        formatOptionalTime(job.Metadata.EndedAt),
	}
	var response uploadInitResponse
	if err := u.postJSON("/api/recording-uploads/init", payload, &response); err != nil {
		return response, err
	}
	if !response.OK || response.UploadID == "" || response.UploadURL == "" {
		if response.Error != "" {
			return response, errors.New(response.Error)
		}
		return response, errors.New("platform upload init returned an incomplete response")
	}
	return response, nil
}

type uploadCompleteRequest struct {
	UploadID      string `json:"uploadId"`
	ByteSize      int64  `json:"byteSize"`
	SHA256        string `json:"sha256"`
	FrameCount    uint64 `json:"frameCount,omitempty"`
	FirstSequence uint64 `json:"firstSequence,omitempty"`
	LastSequence  uint64 `json:"lastSequence,omitempty"`
	StartedAt     string `json:"startedAt,omitempty"`
	EndedAt       string `json:"endedAt,omitempty"`
}

type uploadCompleteResponse struct {
	OK    bool   `json:"ok"`
	Error string `json:"error"`
}

func (u *SegmentUploader) completeUpload(uploadID string, byteSize int64, checksum string, metadata segmentMetadata) error {
	payload := uploadCompleteRequest{
		UploadID:      uploadID,
		ByteSize:      byteSize,
		SHA256:        checksum,
		FrameCount:    metadata.FrameCount,
		FirstSequence: metadata.FirstSequence,
		LastSequence:  metadata.LastSequence,
		StartedAt:     formatOptionalTime(metadata.StartedAt),
		EndedAt:       formatOptionalTime(metadata.EndedAt),
	}
	var response uploadCompleteResponse
	if err := u.postJSON("/api/recording-uploads/complete", payload, &response); err != nil {
		return err
	}
	if !response.OK {
		if response.Error != "" {
			return errors.New(response.Error)
		}
		return errors.New("platform upload completion failed")
	}
	return nil
}

func (u *SegmentUploader) putObject(uploadURL, path, contentType string) error {
	file, err := os.Open(path)
	if err != nil {
		return err
	}
	defer file.Close()
	info, err := file.Stat()
	if err != nil {
		return err
	}
	request, err := http.NewRequestWithContext(context.Background(), http.MethodPut, uploadURL, file)
	if err != nil {
		return err
	}
	request.ContentLength = info.Size()
	if contentType != "" {
		request.Header.Set("Content-Type", contentType)
	}
	response, err := u.client.Do(request)
	if err != nil {
		return err
	}
	defer response.Body.Close()
	if response.StatusCode < 200 || response.StatusCode >= 300 {
		body, _ := io.ReadAll(io.LimitReader(response.Body, 1024))
		return fmt.Errorf("rustfs upload failed: status=%d body=%s", response.StatusCode, strings.TrimSpace(string(body)))
	}
	return nil
}

func (u *SegmentUploader) postJSON(path string, payload any, output any) error {
	body, err := json.Marshal(payload)
	if err != nil {
		return err
	}
	request, err := http.NewRequestWithContext(context.Background(), http.MethodPost, u.options.PlatformURL+path, bytes.NewReader(body))
	if err != nil {
		return err
	}
	request.Header.Set("content-type", "application/json")
	if u.options.APIToken != "" {
		request.Header.Set("authorization", "Bearer "+u.options.APIToken)
	}
	response, err := u.client.Do(request)
	if err != nil {
		return err
	}
	defer response.Body.Close()
	if response.StatusCode < 200 || response.StatusCode >= 300 {
		body, _ := io.ReadAll(io.LimitReader(response.Body, 1024))
		return fmt.Errorf("platform %s failed: status=%d body=%s", path, response.StatusCode, strings.TrimSpace(string(body)))
	}
	if err := json.NewDecoder(response.Body).Decode(output); err != nil {
		return err
	}
	return nil
}

func sha256File(path string) (string, error) {
	file, err := os.Open(path)
	if err != nil {
		return "", err
	}
	defer file.Close()
	hash := sha256.New()
	if _, err := io.Copy(hash, file); err != nil {
		return "", err
	}
	return hex.EncodeToString(hash.Sum(nil)), nil
}

func formatOptionalTime(value time.Time) string {
	if value.IsZero() {
		return ""
	}
	return value.UTC().Format(time.RFC3339Nano)
}
