package main

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"log"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"
)

type ResponseData struct {
	Value    string `json:"value"`
	Recorded string `json:"recorded"`
	FilePath string `json:"filePath"`
	Pinned   bool   `json:"pinned"`
}

type ClipboardData struct {
	ClipboardHistory []ResponseData `json:"clipboardHistory"`
}

type Config struct {
	AuthUser string
	AuthPass string
	FilePath string
	MaxBody  int64
}

const (
	latestMaxAge   = 30 * time.Minute
	defaultMaxBody = 64 << 10 // 64 KiB
)

var (
	config Config

	// Serialises the read-modify-write of the history file done by /append
	writeMu sync.Mutex
)

func main() {
	config = loadConfig()

	mux := http.NewServeMux()
	mux.HandleFunc("/append", handlePost)
	mux.HandleFunc("/latest", handleGetLatest)
	mux.HandleFunc("/status", handleStatus)

	server := &http.Server{
		Addr:              ":8080",
		Handler:           mux,
		ReadHeaderTimeout: 10 * time.Second,
		ReadTimeout:       30 * time.Second,
		WriteTimeout:      30 * time.Second,
		IdleTimeout:       60 * time.Second,
		MaxHeaderBytes:    8 << 10,
	}

	log.Println("Server starting on :8080")
	log.Fatal(server.ListenAndServe())
}

// loadConfig reads the environment once, and refuses to start if anything
// required is missing. An empty AUTH_USER/AUTH_PASS would otherwise let any
// request through, as basic auth with empty credentials would match.
func loadConfig() Config {
	c := Config{
		AuthUser: os.Getenv("AUTH_USER"),
		AuthPass: os.Getenv("AUTH_PASS"),
		FilePath: os.Getenv("CLIPBOARD_JSON_FILE"),
		MaxBody:  defaultMaxBody,
	}

	var missing []string
	if c.AuthUser == "" {
		missing = append(missing, "AUTH_USER")
	}
	if c.AuthPass == "" {
		missing = append(missing, "AUTH_PASS")
	}
	if c.FilePath == "" {
		missing = append(missing, "CLIPBOARD_JSON_FILE")
	}
	if len(missing) > 0 {
		log.Fatalf("Missing required env var(s): %s", strings.Join(missing, ", "))
	}

	if v := os.Getenv("MAX_BODY_BYTES"); v != "" {
		n, err := strconv.ParseInt(v, 10, 64)
		if err != nil || n < 1 {
			log.Fatalf("MAX_BODY_BYTES must be a positive integer, got %q", v)
		}
		c.MaxBody = n
	}

	return c
}

func checkAuth(r *http.Request) bool {
	username, password, ok := r.BasicAuth()
	if !ok {
		return false
	}
	return username == config.AuthUser && password == config.AuthPass
}

// plainText makes sure a stored clipboard entry is never served as html, which
// Go would otherwise sniff from its content.
func plainText(w http.ResponseWriter) {
	w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	w.Header().Set("X-Content-Type-Options", "nosniff")
}

// readClipboardData returns an empty history if the file doesn't exist yet, but
// fails on any other read error: treating an unreadable file as an empty one
// would make the next /append truncate the whole history.
func readClipboardData(path string) (*ClipboardData, error) {
	fileData, err := os.ReadFile(path)
	if errors.Is(err, fs.ErrNotExist) {
		return &ClipboardData{ClipboardHistory: []ResponseData{}}, nil
	}
	if err != nil {
		return nil, err
	}

	var clipboardData ClipboardData
	if err := json.Unmarshal(fileData, &clipboardData); err != nil {
		return nil, err
	}
	return &clipboardData, nil
}

// writeClipboardData replaces the history file atomically, so that neither a
// crash nor a concurrent reader (clipse, Syncthing) can see a half-written file.
func writeClipboardData(path string, data *ClipboardData) error {
	encoded, err := json.Marshal(data)
	if err != nil {
		return err
	}

	// The ".#" prefix matches the .#* line of .stglobalignore, so that Syncthing
	// ignores the temp file in case it scans the folder mid-write.
	tmp, err := os.CreateTemp(filepath.Dir(path), ".#clipboard-*.json")
	if err != nil {
		return err
	}
	defer func() {
		tmp.Close()           // no-op if already closed below
		os.Remove(tmp.Name()) // no-op once the rename succeeded
	}()

	if _, err := tmp.Write(encoded); err != nil {
		return err
	}
	if err := tmp.Sync(); err != nil {
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}

	// A rename puts a new inode in place, so mode and owner have to be carried
	// over explicitly – else Syncthing could lose write access to the file.
	mode := os.FileMode(0644)
	if fi, err := os.Stat(path); err == nil {
		mode = fi.Mode().Perm()
		if st, ok := fi.Sys().(*syscall.Stat_t); ok {
			if err := os.Chown(tmp.Name(), int(st.Uid), int(st.Gid)); err != nil {
				log.Printf("WARN | could not preserve ownership of %s: %v", path, err)
			}
		}
	}
	if err := os.Chmod(tmp.Name(), mode); err != nil {
		return err
	}

	return os.Rename(tmp.Name(), path)
}

func getClientIP(r *http.Request) string {
	if ip := r.Header.Get("Cf-Connecting-Ip"); ip != "" {
		return ip
	}
	return r.Header.Get("X-Real-Ip")
}

func handlePost(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
		return
	}
	if !checkAuth(r) {
		http.Error(w, "Invalid credentials", http.StatusUnauthorized)
		return
	}

	r.Body = http.MaxBytesReader(w, r.Body, config.MaxBody)
	body, err := io.ReadAll(r.Body)
	if err != nil {
		http.Error(w, fmt.Sprintf("Body unreadable or over %d bytes", config.MaxBody), http.StatusRequestEntityTooLarge)
		return
	}

	log.Printf("POST /append from %s - %d bytes", getClientIP(r), len(body))

	writeMu.Lock()
	defer writeMu.Unlock()

	clipboardData, err := readClipboardData(config.FilePath)
	if err != nil {
		log.Printf("ERROR | reading %s: %v", config.FilePath, err)
		http.Error(w, "Could not read the clipboard history", http.StatusInternalServerError)
		return
	}

	respData := ResponseData{
		Value:    string(body),
		Recorded: time.Now().Format("2006-01-02 15:04:05.000000"),
		FilePath: "null",
		Pinned:   false,
	}
	clipboardData.ClipboardHistory = append([]ResponseData{respData}, clipboardData.ClipboardHistory...)

	if err := writeClipboardData(config.FilePath, clipboardData); err != nil {
		log.Printf("ERROR | writing %s: %v", config.FilePath, err)
		http.Error(w, "Could not write to the clipboard history", http.StatusInternalServerError)
		return
	}

	plainText(w)
	fmt.Fprint(w, "Success")
}

func handleGetLatest(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
		return
	}
	if !checkAuth(r) {
		http.Error(w, "Invalid credentials", http.StatusUnauthorized)
		return
	}

	log.Printf("GET /latest from %s", r.RemoteAddr)

	clipboardData, err := readClipboardData(config.FilePath)
	if err != nil {
		log.Printf("ERROR | reading %s: %v", config.FilePath, err)
		http.Error(w, "Could not read the clipboard history", http.StatusInternalServerError)
		return
	}
	if len(clipboardData.ClipboardHistory) == 0 {
		http.Error(w, "No entries", http.StatusNotFound)
		return
	}

	latest := clipboardData.ClipboardHistory[0]

	// Parse recorded time and check if within latestMaxAge
	recordedTime, err := time.ParseInLocation("2006-01-02 15:04:05", latest.Recorded, time.Local)
	if err != nil {
		http.Error(w, "Invalid time format", http.StatusInternalServerError)
		return
	}
	if time.Since(recordedTime) > latestMaxAge {
		http.Error(w, "No recent entries", http.StatusNotFound)
		return
	}

	plainText(w)
	fmt.Fprint(w, latest.Value)
}

func handleStatus(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
		return
	}

	plainText(w)
	fmt.Fprint(w, "ok")
}
