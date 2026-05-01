package main

import (
	"crypto/rand"
	"embed"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"io/fs"
	"log"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"sync"
	"syscall"
	"time"

	qrcode "github.com/skip2/go-qrcode"
)

//go:embed static/*
var staticFiles embed.FS

const (
	port           = ":8080"
	uploadDir      = "./uploads"
	maxFileSize    = 200 << 20 // 200MB
	multicastAddr  = "224.0.0.167:9999"
	beaconInterval = 3 * time.Second
	peerTimeout    = 10 * time.Second
)

type UploadResponse struct {
	DownloadURL string `json:"downloadUrl"`
	QRCodeURL   string `json:"qrCodeUrl"`
	FileName    string `json:"fileName"`
	FileSize    int64  `json:"fileSize"`
	Token       string `json:"token"`
}

type FileRecord struct {
	FileName     string
	OriginalName string
	Token        string
	ExpiresAt    time.Time
}

type Peer struct {
	Name     string    `json:"name"`
	IP       string    `json:"ip"`
	LastSeen time.Time `json:"lastSeen"`
}

type BeaconMessage struct {
	Name string `json:"name"`
	IP   string `json:"ip"`
}

var (
	fileStore     = make(map[string]FileRecord)
	fileStoreMu   sync.RWMutex
	peers         = make(map[string]Peer)
	peersMu       sync.RWMutex
	displayName   string
	localIP       string
	uploadTimes   = make(map[string][]time.Time)
	uploadTimesMu sync.Mutex
)

func generateToken() string {
	b := make([]byte, 16)
	rand.Read(b)
	return hex.EncodeToString(b)
}

func getLocalIP() string {
	interfaces, err := net.Interfaces()
	if err != nil {
		return "localhost"
	}
	for _, iface := range interfaces {
		if iface.Flags&net.FlagUp == 0 || iface.Flags&net.FlagLoopback != 0 {
			continue
		}
		addrs, err := iface.Addrs()
		if err != nil {
			continue
		}
		for _, addr := range addrs {
			var ip net.IP
			switch v := addr.(type) {
			case *net.IPNet:
				ip = v.IP
			case *net.IPAddr:
				ip = v.IP
			}
			if ip == nil || ip.IsLoopback() {
				continue
			}
			if ip.To4() != nil {
				return ip.String()
			}
		}
	}
	return "localhost"
}

func getHostname() string {
	name, err := os.Hostname()
	if err != nil {
		return "Unknown Device"
	}
	return name
}

func isRateLimited(ip string) bool {
	uploadTimesMu.Lock()
	defer uploadTimesMu.Unlock()
	now := time.Now()
	window := now.Add(-1 * time.Minute)
	times := uploadTimes[ip]
	var recent []time.Time
	for _, t := range times {
		if t.After(window) {
			recent = append(recent, t)
		}
	}
	uploadTimes[ip] = recent
	if len(recent) >= 10 {
		return true
	}
	uploadTimes[ip] = append(uploadTimes[ip], now)
	return false
}

func corsMiddleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Access-Control-Allow-Origin", "*")
		w.Header().Set("Access-Control-Allow-Methods", "GET, POST, OPTIONS")
		w.Header().Set("Access-Control-Allow-Headers", "Content-Type")
		if r.Method == http.MethodOptions {
			w.WriteHeader(http.StatusOK)
			return
		}
		next.ServeHTTP(w, r)
	})
}

func uploadHandler(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
		return
	}
	clientIP := r.RemoteAddr
	if isRateLimited(clientIP) {
		http.Error(w, "Too many uploads. Please wait.", http.StatusTooManyRequests)
		return
	}
	r.Body = http.MaxBytesReader(w, r.Body, maxFileSize)
	if err := r.ParseMultipartForm(maxFileSize); err != nil {
		http.Error(w, "File too large. Maximum size is 200MB.", http.StatusBadRequest)
		return
	}
	file, handler, err := r.FormFile("file")
	if err != nil {
		http.Error(w, "Error retrieving file", http.StatusBadRequest)
		return
	}
	defer file.Close()
	os.MkdirAll(uploadDir, os.ModePerm)
	token := generateToken()
	fileName := fmt.Sprintf("%d_%s", time.Now().UnixNano(), handler.Filename)
	destPath := filepath.Join(uploadDir, fileName)
	dst, err := os.Create(destPath)
	if err != nil {
		http.Error(w, "Error saving file", http.StatusInternalServerError)
		return
	}
	defer dst.Close()
	size, err := io.Copy(dst, file)
	if err != nil {
		http.Error(w, "Error writing file", http.StatusInternalServerError)
		return
	}
	record := FileRecord{
		FileName:     fileName,
		OriginalName: handler.Filename,
		Token:        token,
		ExpiresAt:    time.Now().Add(1 * time.Hour),
	}
	fileStoreMu.Lock()
	fileStore[token] = record
	fileStoreMu.Unlock()
	downloadURL := fmt.Sprintf("http://%s:8080/download/%s?token=%s", localIP, fileName, token)
	qrFileName := token + "_qr.png"
	qrPath := filepath.Join(uploadDir, qrFileName)
	if err := qrcode.WriteFile(downloadURL, qrcode.Medium, 256, qrPath); err != nil {
		http.Error(w, "Error generating QR code", http.StatusInternalServerError)
		return
	}
	qrURL := fmt.Sprintf("http://%s:8080/qr/%s", localIP, qrFileName)
	resp := UploadResponse{
		DownloadURL: downloadURL,
		QRCodeURL:   qrURL,
		FileName:    handler.Filename,
		FileSize:    size,
		Token:       token,
	}
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(resp)
	go func() {
		time.Sleep(1 * time.Hour)
		os.Remove(destPath)
		os.Remove(qrPath)
		fileStoreMu.Lock()
		delete(fileStore, token)
		fileStoreMu.Unlock()
	}()
}

func downloadHandler(w http.ResponseWriter, r *http.Request) {
	fileName := filepath.Base(r.URL.Path)
	token := r.URL.Query().Get("token")
	if token == "" {
		http.Error(w, "Missing token", http.StatusUnauthorized)
		return
	}
	fileStoreMu.RLock()
	record, exists := fileStore[token]
	fileStoreMu.RUnlock()
	if !exists {
		http.Error(w, "Invalid or expired token", http.StatusUnauthorized)
		return
	}
	if time.Now().After(record.ExpiresAt) {
		http.Error(w, "Link has expired", http.StatusGone)
		return
	}
	if record.FileName != fileName {
		http.Error(w, "Token mismatch", http.StatusUnauthorized)
		return
	}
	filePath := filepath.Join(uploadDir, fileName)
	file, err := os.Open(filePath)
	if err != nil {
		http.Error(w, "File not found", http.StatusNotFound)
		return
	}
	defer file.Close()
	w.Header().Set("Content-Disposition", fmt.Sprintf(`attachment; filename="%s"`, record.OriginalName))
	w.Header().Set("Content-Type", "application/octet-stream")
	io.Copy(w, file)
}

func qrHandler(w http.ResponseWriter, r *http.Request) {
	fileName := filepath.Base(r.URL.Path)
	filePath := filepath.Join(uploadDir, fileName)
	http.ServeFile(w, r, filePath)
}

func peersHandler(w http.ResponseWriter, r *http.Request) {
	peersMu.RLock()
	defer peersMu.RUnlock()
	list := []Peer{}
	for _, p := range peers {
		if time.Since(p.LastSeen) < peerTimeout {
			list = append(list, p)
		}
	}
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(list)
}

func meHandler(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]string{"name": displayName, "ip": localIP})
}

func setNameHandler(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
		return
	}
	var body struct {
		Name string `json:"name"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil || body.Name == "" {
		http.Error(w, "Invalid name", http.StatusBadRequest)
		return
	}
	displayName = body.Name
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]string{"name": displayName})
}

func sendToPeerHandler(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
		return
	}
	peerIP := r.URL.Query().Get("to")
	if peerIP == "" {
		http.Error(w, "Missing target peer IP", http.StatusBadRequest)
		return
	}
	r.Body = http.MaxBytesReader(w, r.Body, maxFileSize)
	if err := r.ParseMultipartForm(maxFileSize); err != nil {
		http.Error(w, "File too large", http.StatusBadRequest)
		return
	}
	file, handler, err := r.FormFile("file")
	if err != nil {
		http.Error(w, "Error retrieving file", http.StatusBadRequest)
		return
	}
	defer file.Close()
	peerURL := fmt.Sprintf("http://%s:8080/receive", peerIP)
	pr, pw := io.Pipe()
	go func() {
		defer pw.Close()
		io.Copy(pw, file)
	}()
	req, err := http.NewRequest("POST", peerURL, pr)
	if err != nil {
		http.Error(w, "Failed to create request to peer", http.StatusInternalServerError)
		return
	}
	req.Header.Set("Content-Type", "application/octet-stream")
	req.Header.Set("X-Sender-Name", displayName)
	req.Header.Set("X-File-Name", handler.Filename)
	client := &http.Client{Timeout: 5 * time.Minute}
	resp, err := client.Do(req)
	if err != nil {
		http.Error(w, "Failed to reach peer: "+err.Error(), http.StatusBadGateway)
		return
	}
	defer resp.Body.Close()
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]string{"status": "sent", "to": peerIP})
}

func receiveHandler(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
		return
	}
	senderName := r.Header.Get("X-Sender-Name")
	originalName := r.Header.Get("X-File-Name")
	if originalName == "" {
		originalName = "received_file"
	}
	os.MkdirAll(uploadDir, os.ModePerm)
	token := generateToken()
	fileName := fmt.Sprintf("%d_%s", time.Now().UnixNano(), originalName)
	destPath := filepath.Join(uploadDir, fileName)
	dst, err := os.Create(destPath)
	if err != nil {
		http.Error(w, "Error saving file", http.StatusInternalServerError)
		return
	}
	defer dst.Close()
	size, err := io.Copy(dst, r.Body)
	if err != nil {
		http.Error(w, "Error writing file", http.StatusInternalServerError)
		return
	}
	record := FileRecord{
		FileName:     fileName,
		OriginalName: originalName,
		Token:        token,
		ExpiresAt:    time.Now().Add(1 * time.Hour),
	}
	fileStoreMu.Lock()
	fileStore[token] = record
	fileStoreMu.Unlock()
	log.Printf("Received '%s' (%d bytes) from %s", originalName, size, senderName)
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]interface{}{
		"status": "received", "fileName": originalName,
		"size": size, "from": senderName, "token": token,
	})
	go func() {
		time.Sleep(1 * time.Hour)
		os.Remove(destPath)
		fileStoreMu.Lock()
		delete(fileStore, token)
		fileStoreMu.Unlock()
	}()
}

func inboxHandler(w http.ResponseWriter, r *http.Request) {
	fileStoreMu.RLock()
	defer fileStoreMu.RUnlock()
	type InboxItem struct {
		FileName    string `json:"fileName"`
		DownloadURL string `json:"downloadUrl"`
		ExpiresAt   string `json:"expiresAt"`
	}
	items := []InboxItem{}
	for token, record := range fileStore {
		if time.Now().Before(record.ExpiresAt) {
			items = append(items, InboxItem{
				FileName:    record.OriginalName,
				DownloadURL: fmt.Sprintf("http://%s:8080/download/%s?token=%s", localIP, record.FileName, token),
				ExpiresAt:   record.ExpiresAt.Format(time.RFC3339),
			})
		}
	}
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(items)
}

func startBeacon() {
	for {
		// Broadcast to entire subnet
		addr, err := net.ResolveUDPAddr("udp", "255.255.255.255:9999")
		if err != nil {
			time.Sleep(beaconInterval)
			continue
		}
		conn, err := net.DialUDP("udp", nil, addr)
		if err != nil {
			time.Sleep(beaconInterval)
			continue
		}
		msg := BeaconMessage{Name: displayName, IP: localIP}
		data, _ := json.Marshal(msg)
		conn.Write(data)
		conn.Close()
		time.Sleep(beaconInterval)
	}
}

func startListener() {
	addr, err := net.ResolveUDPAddr("udp4", "0.0.0.0:9999")
	if err != nil {
		log.Println("Listener error:", err)
		return
	}
	conn, err := net.ListenUDP("udp4", addr)
	if err != nil {
		log.Println("UDP listen error:", err)
		return
	}
	defer conn.Close()

	// Allow receiving broadcast packets
	rawConn, err := conn.SyscallConn()
	if err == nil {
		rawConn.Control(func(fd uintptr) {
			syscall.SetsockoptInt(int(fd), syscall.SOL_SOCKET, syscall.SO_BROADCAST, 1)
		})
	}

	buf := make([]byte, 1024)
	for {
		n, _, err := conn.ReadFromUDP(buf)
		if err != nil {
			continue
		}
		var msg BeaconMessage
		if err := json.Unmarshal(buf[:n], &msg); err != nil {
			continue
		}
		if msg.IP == localIP {
			continue
		}
		peersMu.Lock()
		peers[msg.IP] = Peer{
			Name:     msg.Name,
			IP:       msg.IP,
			LastSeen: time.Now(),
		}
		peersMu.Unlock()
	}
}

func startPeerCleaner() {
	for {
		time.Sleep(15 * time.Second)
		peersMu.Lock()
		for ip, p := range peers {
			if time.Since(p.LastSeen) > peerTimeout {
				delete(peers, ip)
			}
		}
		peersMu.Unlock()
	}
}

func main() {
	localIP = getLocalIP()
	displayName = getHostname()
	os.MkdirAll(uploadDir, os.ModePerm)

	go startBeacon()
	go startListener()
	go startPeerCleaner()

	mux := http.NewServeMux()
	mux.HandleFunc("/upload", uploadHandler)
	mux.HandleFunc("/download/", downloadHandler)
	mux.HandleFunc("/qr/", qrHandler)
	mux.HandleFunc("/peers", peersHandler)
	mux.HandleFunc("/me", meHandler)
	mux.HandleFunc("/setname", setNameHandler)
	mux.HandleFunc("/send", sendToPeerHandler)
	mux.HandleFunc("/receive", receiveHandler)
	mux.HandleFunc("/inbox", inboxHandler)

	stripped, _ := fs.Sub(staticFiles, "static")
	mux.Handle("/", http.FileServer(http.FS(stripped)))

	handler := corsMiddleware(mux)

	fmt.Println("DropSend is running!")
	fmt.Printf("Open:    http://localhost:8080\n")
	fmt.Printf("Network: http://%s:8080\n", localIP)
	fmt.Printf("Name:    %s\n", displayName)

	go func() {
		time.Sleep(500 * time.Millisecond)
		exec.Command("xdg-open", "http://localhost:8080").Start()
	}()

	log.Fatal(http.ListenAndServe(port, handler))
}
