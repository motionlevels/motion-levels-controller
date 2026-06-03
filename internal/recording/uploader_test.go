package recording

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestSegmentUploaderUploadsAndCompletes(t *testing.T) {
	dir := t.TempDir()
	segmentPath := filepath.Join(dir, "segment.frames.pbstream.zst")
	payload := []byte("compressed segment")
	if err := os.WriteFile(segmentPath, payload, 0o644); err != nil {
		t.Fatal(err)
	}
	expectedHash := sha256.Sum256(payload)
	expectedSHA := hex.EncodeToString(expectedHash[:])

	var uploaded []byte
	var completed uploadCompleteRequest
	var platform *httptest.Server
	platform = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/upload":
			if r.Method != http.MethodPut {
				t.Fatalf("upload method = %s, want PUT", r.Method)
			}
			if r.ContentLength != int64(len(payload)) {
				t.Fatalf("upload content length = %d, want %d", r.ContentLength, len(payload))
			}
			uploaded = readAll(t, r)
			w.WriteHeader(http.StatusOK)
		case "/api/recording-uploads/init":
			if got := r.Header.Get("authorization"); got != "Bearer test-token" {
				t.Fatalf("authorization = %q", got)
			}
			var request uploadInitRequest
			if err := json.NewDecoder(r.Body).Decode(&request); err != nil {
				t.Fatal(err)
			}
			if request.ControllerID != "controller-1" || request.FrameCount != 3 {
				t.Fatalf("unexpected init request: %+v", request)
			}
			if request.SessionID != "segment-session" {
				t.Fatalf("session id = %q, want segment-session", request.SessionID)
			}
			_ = json.NewEncoder(w).Encode(uploadInitResponse{
				OK:        true,
				UploadID:  "upload-1",
				Bucket:    "recordings",
				ObjectKey: "recordings/controller-1/segment.frames.pbstream.zst",
				UploadURL: platform.URL + "/upload",
			})
		case "/api/recording-uploads/complete":
			if err := json.NewDecoder(r.Body).Decode(&completed); err != nil {
				t.Fatal(err)
			}
			_ = json.NewEncoder(w).Encode(uploadCompleteResponse{OK: true})
		default:
			http.NotFound(w, r)
		}
	}))
	defer platform.Close()

	uploader, err := NewSegmentUploader(UploadOptions{
		PlatformURL:  platform.URL,
		APIToken:     "test-token",
		ControllerID: "controller-1",
		SessionID:    "session-1",
		HTTPTimeout:  time.Second,
	})
	if err != nil {
		t.Fatal(err)
	}
	uploader.Enqueue(uploadJob{
		Path:        segmentPath,
		Compression: "zstd",
		ContentType: "application/zstd",
		Metadata: segmentMetadata{
			SessionID:     "segment-session",
			FrameCount:    3,
			FirstSequence: 10,
			LastSequence:  12,
			StartedAt:     time.Unix(100, 0),
			EndedAt:       time.Unix(103, 0),
		},
	})
	uploader.Close()

	if string(uploaded) != string(payload) {
		t.Fatalf("uploaded payload = %q, want %q", uploaded, payload)
	}
	if completed.UploadID != "upload-1" || completed.SHA256 != expectedSHA || completed.ByteSize != int64(len(payload)) {
		t.Fatalf("unexpected completion: %+v want sha %s size %d", completed, expectedSHA, len(payload))
	}
	if stats := uploader.Stats(); stats.UploadedSegments != 1 || stats.FailedSegments != 0 {
		t.Fatalf("unexpected stats: %+v", stats)
	}
}

func TestSegmentUploaderRequeuesFailedUploads(t *testing.T) {
	dir := t.TempDir()
	segmentPath := filepath.Join(dir, "retry.frames.pbstream.zst")
	if err := os.WriteFile(segmentPath, []byte("retry segment"), 0o644); err != nil {
		t.Fatal(err)
	}

	var initAttempts int
	var completed bool
	var platform *httptest.Server
	platform = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/upload":
			w.WriteHeader(http.StatusOK)
		case "/api/recording-uploads/init":
			initAttempts++
			if initAttempts == 1 {
				http.Error(w, "temporary platform outage", http.StatusBadGateway)
				return
			}
			_ = json.NewEncoder(w).Encode(uploadInitResponse{
				OK:        true,
				UploadID:  "upload-1",
				Bucket:    "recordings",
				ObjectKey: "recordings/controller-1/retry.frames.pbstream.zst",
				UploadURL: platform.URL + "/upload",
			})
		case "/api/recording-uploads/complete":
			completed = true
			_ = json.NewEncoder(w).Encode(uploadCompleteResponse{OK: true})
		default:
			http.NotFound(w, r)
		}
	}))
	defer platform.Close()

	uploader, err := NewSegmentUploader(UploadOptions{
		PlatformURL:  platform.URL,
		ControllerID: "controller-1",
		HTTPTimeout:  time.Second,
		MaxAttempts:  1,
		RetryDelay:   time.Millisecond,
	})
	if err != nil {
		t.Fatal(err)
	}
	defer uploader.Close()

	uploader.Enqueue(uploadJob{
		Path:        segmentPath,
		Compression: "zstd",
		ContentType: "application/zstd",
		Metadata:    segmentMetadata{SessionID: "session-1"},
	})

	deadline := time.Now().Add(time.Second)
	for time.Now().Before(deadline) {
		stats := uploader.Stats()
		if completed && stats.UploadedSegments == 1 && stats.FailedSegments == 1 {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("upload was not retried: attempts=%d completed=%t stats=%+v", initAttempts, completed, uploader.Stats())
}

func readAll(t *testing.T, r *http.Request) []byte {
	t.Helper()
	data, err := io.ReadAll(r.Body)
	if err != nil {
		t.Fatal(err)
	}
	return data
}
