package main

import (
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"os"
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

func main() {
	http.HandleFunc("/append", handlePost)
	http.HandleFunc("/latest", handleGetLatest)
	http.HandleFunc("/status", handleStatus)
	log.Println("Server starting on :8080")
	log.Fatal(http.ListenAndServe(":8080", nil))
}

func checkAuth(r *http.Request) bool {
	username, password, ok := r.BasicAuth()
	if !ok {
		return false
	}
	return username == os.Getenv("AUTH_USER") && password == os.Getenv("AUTH_PASS")
}

func readOrCreateClipboardData(filePath string) (*ClipboardData, error) {
	fileData, err := os.ReadFile(filePath)
	if err != nil {
		// File doesn't exist, create new structure
		return &ClipboardData{ClipboardHistory: []ResponseData{}}, nil
	}

	var clipboardData ClipboardData
	if err := json.Unmarshal(fileData, &clipboardData); err != nil {
		return nil, err
	}
	return &clipboardData, nil
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

	body, err := io.ReadAll(r.Body)
	if err != nil {
		http.Error(w, "Failed to read body", http.StatusBadRequest)
		return
	}

	log.Printf("POST /append from %s - %d bytes", getClientIP(r), len(body))

	filePath := os.Getenv("CLIPBOARD_JSON_FILE")
	if filePath == "" {
		http.Error(w, "CLIPBOARD_JSON_FILE env var not set", http.StatusInternalServerError)
		return
	}

	clipboardData, err := readOrCreateClipboardData(filePath)
	if err != nil {
		http.Error(w, "Invalid JSON in file", http.StatusInternalServerError)
		return
	}

	respData := ResponseData{
		Value:    string(body),
		Recorded: time.Now().Format("2006-01-02 15:04:05.000000"),
		FilePath: "null",
		Pinned:   false,
	}
	clipboardData.ClipboardHistory = append([]ResponseData{respData}, clipboardData.ClipboardHistory...)

	updatedData, _ := json.Marshal(clipboardData)
	if err := os.WriteFile(filePath, updatedData, 0644); err != nil {
		http.Error(w, "Failed to write to file", http.StatusInternalServerError)
		return
	}

	w.WriteHeader(http.StatusOK)
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

	filePath := os.Getenv("CLIPBOARD_JSON_FILE")
	if filePath == "" {
		http.Error(w, "CLIPBOARD_JSON_FILE env var not set", http.StatusInternalServerError)
		return
	}

	clipboardData, err := readOrCreateClipboardData(filePath)
	if err != nil {
		http.Error(w, "Invalid JSON in file", http.StatusInternalServerError)
		return
	}
	if len(clipboardData.ClipboardHistory) == 0 {
		http.Error(w, "No entries", http.StatusNotFound)
		return
	}

	latest := clipboardData.ClipboardHistory[0]

	// Parse recorded time and check if within 30 minutes
	recordedTime, err := time.ParseInLocation("2006-01-02 15:04:05", latest.Recorded, time.Local)
	if err != nil {
		http.Error(w, "Invalid time format", http.StatusInternalServerError)
		return
	}
	if time.Since(recordedTime) > 30*time.Minute {
		http.Error(w, "No recent entries", http.StatusNotFound)
		return
	}

	fmt.Fprint(w, latest.Value)
}

func handleStatus(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
		return
	}

	fmt.Fprint(w, "ok")
}
