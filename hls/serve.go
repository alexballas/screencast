package hls

import (
	"fmt"
	"net/http"
	"os"
	"path"
	"path/filepath"
	"strings"
	"time"
)

type DirectoryHandlerOptions struct {
	Debug     bool
	Logf      func(format string, args ...any)
	LogPrefix string
}

type statusRecorder struct {
	http.ResponseWriter
	status int
	bytes  int64
}

func (r *statusRecorder) WriteHeader(status int) {
	r.status = status
	r.ResponseWriter.WriteHeader(status)
}

func (r *statusRecorder) Write(p []byte) (int, error) {
	n, err := r.ResponseWriter.Write(p)
	r.bytes += int64(n)
	return n, err
}

func NewDirectoryHandler(dir string, options *DirectoryHandlerOptions) http.Handler {
	opts := DirectoryHandlerOptions{}
	if options != nil {
		opts = *options
	}

	fileServer := http.FileServer(http.Dir(dir))

	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		rec := &statusRecorder{ResponseWriter: w, status: http.StatusOK}
		var fsPath string
		if p, ok := resolvePath(dir, r.URL.Path); ok {
			fsPath = p
		}

		w.Header().Set("Access-Control-Allow-Origin", "*")
		w.Header().Set("Access-Control-Allow-Methods", "GET, OPTIONS")

		if r.Method == http.MethodOptions {
			rec.WriteHeader(http.StatusOK)
			debugLogf(opts, "method=%s path=%s status=%d bytes=%d", r.Method, r.URL.Path, rec.status, rec.bytes)
			return
		}

		if strings.HasSuffix(r.URL.Path, ".m3u8") {
			w.Header().Set("Content-Type", "application/vnd.apple.mpegurl")
			w.Header().Set("Cache-Control", "no-cache, no-store, must-revalidate")
			w.Header().Set("Pragma", "no-cache")
			w.Header().Set("Expires", "0")
		} else if strings.HasSuffix(r.URL.Path, ".ts") {
			w.Header().Set("Content-Type", "video/MP2T")
			w.Header().Set("Cache-Control", "no-cache, no-store, must-revalidate")
			w.Header().Set("Pragma", "no-cache")
			w.Header().Set("Expires", "0")
		} else if strings.HasSuffix(r.URL.Path, ".mp4") || strings.HasSuffix(r.URL.Path, ".m4s") {
			w.Header().Set("Content-Type", "video/mp4")
		}

		fileServer.ServeHTTP(rec, r)
		if opts.Debug {
			extra := ""
			switch {
			case fsPath == "":
				extra = "resolved_path=invalid"
			case strings.HasSuffix(r.URL.Path, ".m3u8"):
				extra = summarizePlaylist(fsPath)
			case strings.HasSuffix(r.URL.Path, ".ts"):
				if fi, err := os.Stat(fsPath); err == nil {
					extra = fmt.Sprintf("segment_size=%d segment_mtime=%s", fi.Size(), fi.ModTime().Format(time.RFC3339Nano))
				} else {
					extra = fmt.Sprintf("segment_stat_err=%q", err)
				}
			}
			debugLogf(opts, "method=%s path=%s status=%d bytes=%d %s", r.Method, r.URL.Path, rec.status, rec.bytes, extra)
		}
	})
}

func resolvePath(baseDir, reqPath string) (string, bool) {
	clean := path.Clean("/" + reqPath)
	rel := strings.TrimPrefix(clean, "/")
	full := filepath.Join(baseDir, rel)
	checkRel, err := filepath.Rel(baseDir, full)
	if err != nil {
		return "", false
	}
	if strings.HasPrefix(checkRel, "..") {
		return "", false
	}
	return full, true
}

func summarizePlaylist(path string) string {
	b, err := os.ReadFile(path)
	if err != nil {
		return fmt.Sprintf("playlist_read_err=%q", err)
	}

	seq := "na"
	entryCount := 0
	lastEntry := ""
	lines := strings.Split(string(b), "\n")
	for _, line := range lines {
		l := strings.TrimSpace(line)
		if l == "" {
			continue
		}
		if strings.HasPrefix(l, "#EXT-X-MEDIA-SEQUENCE:") {
			seq = strings.TrimSpace(strings.TrimPrefix(l, "#EXT-X-MEDIA-SEQUENCE:"))
			continue
		}
		if strings.HasPrefix(l, "#") {
			continue
		}
		entryCount++
		lastEntry = l
	}

	if fi, err := os.Stat(path); err == nil {
		return fmt.Sprintf("playlist_seq=%s entries=%d last=%q file_bytes=%d mtime=%s", seq, entryCount, lastEntry, fi.Size(), fi.ModTime().Format(time.RFC3339Nano))
	}
	return fmt.Sprintf("playlist_seq=%s entries=%d last=%q file_bytes=%d", seq, entryCount, lastEntry, len(b))
}

func debugLogf(opts DirectoryHandlerOptions, format string, args ...any) {
	if !opts.Debug || opts.Logf == nil {
		return
	}
	if opts.LogPrefix != "" {
		format = opts.LogPrefix + " " + format
	}
	opts.Logf(format, args...)
}
