package main

import (
	"fmt"
	"io"
	"log"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"sync"
	"time"
)

// constants for video chunks for the video streaming
// it will be 2 mb chunks

const (
	ChunkSize           = 1024 * 1024 * 2
	MaxConcurrentSteams = 100
	VideoStoragePath    = "./videos"
)

// stream manager will manage the video streaming
type StreamManager struct {
	activeStreams  sync.Map
	uploadSessions sync.Map
}

// upload session to tracks a video upload session
type UploadSession struct {
	FileID       string
	FileName     string
	File         *os.File
	FileSize     int64
	UploadedSize int64
	LastUpdated  time.Time
	mu           sync.Mutex
}

// stramsSession will track active viewing sessions
type StreamSession struct {
	FileID       string
	ViewerCount  int
	LastAccessed time.Time
	mu           sync.Mutex
}

// NewStreamManager will create a new stream manager
func NewStreamManager() *StreamManager {

	sm := &StreamManager{}

	// ** create vidoes dir if not created
	if err := os.MkdirAll(VideoStoragePath, 0755); err != nil {
		log.Fatal("failed to create video storage dir", err)
	}
	return sm
}

func (sm *StreamManager) cleanupRoutine() {
	ticker := time.NewTicker(15 * time.Minute)
	for range ticker.C {
		now := time.Now()

		// clean up the upload session
		sm.uploadSessions.Range(func(key, value interface{}) bool {
			session := value.(*UploadSession)
			if now.Sub(session.LastUpdated) > 1*time.Hour {
				session.mu.Lock()
				session.File.Close()
				session.mu.Unlock()
				sm.uploadSessions.Delete(key)
			}
			return true
		})

		// clean up the stream session
		sm.activeStreams.Range(func(key, value interface{}) bool {
			session := value.(*StreamSession)
			session.mu.Lock()
			if now.Sub(session.LastAccessed) > 1*time.Hour && session.ViewerCount == 0 {
				sm.activeStreams.Delete(key)
			}
			session.mu.Unlock()
			return true
		})
	}

}

func main() {

	streamManager := NewStreamManager()

	// handle file upload
	http.HandleFunc("/api/upload", func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			http.Error(w, "Method not allowed", http.StatusBadRequest)
			return
		}

		fileID := r.URL.Query().Get("id")
		if fileID == "" {
			http.Error(w, "fileid is missing", http.StatusBadRequest)
			return
		}

		contentLength := r.ContentLength
		if contentLength <= 0 {
			http.Error(w, "Content-length required", http.StatusBadRequest)
			return
		}

		// create a upload session
		session, _ := streamManager.uploadSessions.LoadOrStore(fileID, &UploadSession{
			FileID:      fileID,
			FileName:    filepath.Join(VideoStoragePath, fileID+".mp4"),
			LastUpdated: time.Now(),
			FileSize:    contentLength,
		})

		uploadedSession := session.(*UploadSession)
		uploadedSession.mu.Lock()
		defer uploadedSession.mu.Unlock()

		// craete a file
		if uploadedSession.File == nil {
			file, err := os.OpenFile(uploadedSession.FileName, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0644)
			if err != nil {
				http.Error(w, "failed to save video file", http.StatusInternalServerError)
				return
			}
			uploadedSession.File = file
		}

		// copy the data from r.body to file in chuncks

		buffer := make([]byte, ChunkSize)
		for {
			n, err := r.Body.Read(buffer)
			if n > 0 {
				if _, writeErr := uploadedSession.File.Write(buffer[:n]); writeErr != nil {
					http.Error(w, "failed to write video file", http.StatusInternalServerError)
					return
				}
				uploadedSession.UploadedSize += int64(n)
			}

			if err == io.EOF {
				break
			}

			if err != nil {
				http.Error(w, "failed to read video file", http.StatusInternalServerError)
				return
			}
		}

		uploadedSession.LastUpdated = time.Now()

		if uploadedSession.UploadedSize >= uploadedSession.FileSize {
			uploadedSession.File.Close()
			uploadedSession.File = nil
			streamManager.uploadSessions.Delete(fileID)

		}

		w.WriteHeader(http.StatusOK)

	})

	// this will handle the video streaming
	http.HandleFunc("/api/watch", func(w http.ResponseWriter, r *http.Request) {
		fileID := r.URL.Query().Get("id")
		if fileID == "" {
			http.Error(w, "fileid is missing", http.StatusBadRequest)
			return
		}

		filePath := filepath.Join(VideoStoragePath, fileID+".mp4")
		file, err := os.Open(filePath)
		if err != nil {
			http.Error(w, "file not found", http.StatusNotFound)
			return
		}
		defer file.Close()

		// get file info
		fileInfo, err := file.Stat()
		if err != nil {
			http.Error(w, "failed to get file info", http.StatusInternalServerError)
			return
		}
		fileSize := fileInfo.Size()

		// handle video range request
		rangeHeader := r.Header.Get("Range")
		if rangeHeader != "" {
			var start, end int64
			if _, err := fmt.Sscanf(rangeHeader, "bytes=%d-%d", &start, &end); err != nil {
				start = 0
				end = fileSize - 1
			}
			if end == 0 {
				end = fileSize - 1
			}

			if start >= fileSize {
				http.Error(w, "invalid range", http.StatusBadRequest)
				return
			}

			if end >= fileSize {
				end = fileSize - 1
			}

			w.Header().Set("Content-Range", fmt.Sprintf("bytes %d-%d/%d", start, end, fileSize))
			w.Header().Set("Accept-Ranges", "bytes")
			w.Header().Set("Content-Length", strconv.FormatInt(end-start+1, 10))
			w.Header().Set("Content-Type", "video/mp4")
			w.WriteHeader(http.StatusPartialContent)

			file.Seek(start, 0)

			// stream the range

			remaining := end - start + 1
			buf := make([]byte, min(ChunkSize, remaining))
			for remaining > 0 {
				readSize := min(int64(len(buf)), remaining)
				n, err := file.Read(buf[:readSize])
				if err != nil && err != io.EOF {
					return
				}
				if n > 0 {
					w.Write(buf[:n])
					remaining -= int64(n)
				}
				if err == io.EOF {
					break
				}
			}

		} else {
			w.Header().Set("Content-Length", strconv.FormatInt(fileSize, 10))
			w.Header().Set("Content-Type", "video/mp4")
			w.Header().Set("Accept-Ranges", "bytes")
			io.Copy(w, file)
		}
	})

	port := ":8080"
	fmt.Printf("Starting Streaming server on %s\n ", port)
	log.Fatal(http.ListenAndServe(port, nil))

}

func min(a, b int64) int64 {
	if a < b {
		return a
	}
	return b
}


