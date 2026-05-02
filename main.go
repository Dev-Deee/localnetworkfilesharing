package main

import (
	"bufio"
	"bytes"
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
	"strings"
	"sync"
	"time"

	qrcode "github.com/skip2/go-qrcode"
)

//go:embed static/*
var staticFiles embed.FS

const (
	port           = ":8080"
	uploadDir      = "./uploads"
	maxFileSize    = 200 << 20
	beaconInterval = 3 * time.Second
	peerTimeout    = 10 * time.Second
	scanInterval   = 30 * time.Second
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

type NetworkDevice struct {
	IP          string `json:"ip"`
	Hostname    string `json:"hostname"`
	MAC         string `json:"mac"`
	HasDropSend bool   `json:"hasDropSend"`
	Name        string `json:"name"`
}

type BeaconMessage struct {
	Name string `json:"name"`
	IP   string `json:"ip"`
}

var (
	fileStore      = make(map[string]FileRecord)
	fileStoreMu    sync.RWMutex
	peers          = make(map[string]Peer)
	peersMu        sync.RWMutex
	networkDevices = make(map[string]NetworkDevice)
	networkMu      sync.RWMutex
	displayName    string
	localIP        string
	uploadTimes    = make(map[string][]time.Time)
	uploadTimesMu  sync.Mutex
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

func getSubnetPrefix(ip string) string {
	parts := strings.Split(ip, ".")
	if len(parts) != 4 {
		return ""
	}
	return strings.Join(parts[:3], ".")
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

// ── NETWORK SCANNER ──────────────────────────────────────────

func pingIP(ip string) {
	exec.Command("ping", "-c", "1", "-W", "1", ip).Run()
}

// checkDropSend probes a device's port 8080 to see if DropSend is running
func checkDropSend(ip string) (bool, string) {
	client := &http.Client{Timeout: 1 * time.Second}
	resp, err := client.Get(fmt.Sprintf("http://%s:8080/me", ip))
	if err != nil {
		return false, ""
	}
	defer resp.Body.Close()
	var me struct {
		Name string `json:"name"`
		IP   string `json:"ip"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&me); err != nil {
		return false, ""
	}
	return true, me.Name
}

func readARPTable(subnetPrefix string) []NetworkDevice {
	out, err := exec.Command("arp", "-a").Output()
	if err != nil {
		return nil
	}
	var devices []NetworkDevice
	scanner := bufio.NewScanner(bytes.NewReader(out))
	for scanner.Scan() {
		line := scanner.Text()
		if !strings.Contains(line, subnetPrefix) {
			continue
		}
		start := strings.Index(line, "(")
		end := strings.Index(line, ")")
		if start == -1 || end == -1 {
			continue
		}
		ip := line[start+1 : end]
		if ip == localIP {
			continue
		}
		hostname := "Unknown Device"
		if start > 0 {
			h := strings.TrimSpace(line[:start])
			if h != "" && h != "?" {
				hostname = h
			}
		}
		mac := ""
		parts := strings.Fields(line)
		for i, p := range parts {
			if p == "at" && i+1 < len(parts) {
				mac = parts[i+1]
				break
			}
		}
		if mac == "" || mac == "<incomplete>" {
			continue
		}
		devices = append(devices, NetworkDevice{
			IP:       ip,
			Hostname: hostname,
			MAC:      mac,
			Name:     hostname,
		})
	}
	return devices
}

func scanNetwork() {
	subnet := getSubnetPrefix(localIP)
	if subnet == "" {
		return
	}
	log.Printf("Scanning network %s.0/24...", subnet)

	// Ping sweep
	var wg sync.WaitGroup
	for i := 1; i <= 254; i++ {
		ip := fmt.Sprintf("%s.%d", subnet, i)
		if ip == localIP {
			continue
		}
		wg.Add(1)
		go func(target string) {
			defer wg.Done()
			pingIP(target)
		}(ip)
	}
	wg.Wait()
	time.Sleep(2 * time.Second)

	// Read ARP table
	devices := readARPTable(subnet)

	// Get current DropSend peers
	peersMu.RLock()
	currentPeers := make(map[string]Peer)
	for k, v := range peers {
		currentPeers[k] = v
	}
	peersMu.RUnlock()

	// For each device, check DropSend status concurrently
	var mu sync.Mutex
	var checkWg sync.WaitGroup
	for i := range devices {
		checkWg.Add(1)
		go func(idx int) {
			defer checkWg.Done()
			d := devices[idx]

			// First check if we already know from UDP beacon
			if peer, ok := currentPeers[d.IP]; ok {
				mu.Lock()
				devices[idx].HasDropSend = true
				devices[idx].Name = peer.Name
				mu.Unlock()
				return
			}

			// Otherwise probe port 8080
			hasDS, name := checkDropSend(d.IP)
			mu.Lock()
			devices[idx].HasDropSend = hasDS
			if hasDS && name != "" {
				devices[idx].Name = name
				// Register as peer
				peersMu.Lock()
				peers[d.IP] = Peer{Name: name, IP: d.IP, LastSeen: time.Now()}
				peersMu.Unlock()
			}
			mu.Unlock()
		}(i)
	}
	checkWg.Wait()

	// Update network map
	networkMu.Lock()
	networkDevices = make(map[string]NetworkDevice)
	for _, d := range devices {
		networkDevices[d.IP] = d
	}
	networkMu.Unlock()

	log.Printf("Scan complete: %d devices found", len(devices))
}

func startNetworkScanner() {
	time.Sleep(5 * time.Second)
	scanNetwork()
	for {
		time.Sleep(scanInterval)
		scanNetwork()
	}
}

// ── HANDLERS ─────────────────────────────────────────────────

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
		http.Error(w, "File too large. Maximum is 200MB.", http.StatusBadRequest)
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
		FileName: fileName, OriginalName: handler.Filename,
		Token: token, ExpiresAt: time.Now().Add(1 * time.Hour),
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
		DownloadURL: downloadURL, QRCodeURL: qrURL,
		FileName: handler.Filename, FileSize: size, Token: token,
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
	if !exists || time.Now().After(record.ExpiresAt) || record.FileName != fileName {
		http.Error(w, "Invalid or expired link", http.StatusUnauthorized)
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
	http.ServeFile(w, r, filepath.Join(uploadDir, fileName))
}

func networkHandler(w http.ResponseWriter, r *http.Request) {
	peersMu.RLock()
	livePeers := make(map[string]Peer)
	for k, v := range peers {
		if time.Since(v.LastSeen) < peerTimeout {
			livePeers[k] = v
		}
	}
	peersMu.RUnlock()

	networkMu.RLock()
	result := []NetworkDevice{}
	seen := make(map[string]bool)
	for _, d := range networkDevices {
		if peer, ok := livePeers[d.IP]; ok {
			d.HasDropSend = true
			d.Name = peer.Name
		}
		result = append(result, d)
		seen[d.IP] = true
	}
	networkMu.RUnlock()

	// Add beacon-discovered peers not yet in ARP
	for ip, peer := range livePeers {
		if !seen[ip] {
			result = append(result, NetworkDevice{
				IP: ip, Hostname: peer.Name,
				Name: peer.Name, HasDropSend: true,
			})
		}
	}

	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(result)
}

func scanHandler(w http.ResponseWriter, r *http.Request) {
	go scanNetwork()
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]string{"status": "scanning"})
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
	pr, pw := io.Pipe()
	go func() { defer pw.Close(); io.Copy(pw, file) }()
	req, err := http.NewRequest("POST", fmt.Sprintf("http://%s:8080/receive", peerIP), pr)
	if err != nil {
		http.Error(w, "Failed to create request", http.StatusInternalServerError)
		return
	}
	req.Header.Set("Content-Type", "application/octet-stream")
	req.Header.Set("X-Sender-Name", displayName)
	req.Header.Set("X-File-Name", handler.Filename)
	resp, err := (&http.Client{Timeout: 5 * time.Minute}).Do(req)
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
		FileName: fileName, OriginalName: originalName,
		Token: token, ExpiresAt: time.Now().Add(1 * time.Hour),
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

// ── PEER DISCOVERY ───────────────────────────────────────────

func startBeacon() {
	for {
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
		return
	}
	conn, err := net.ListenUDP("udp4", addr)
	if err != nil {
		return
	}
	defer conn.Close()
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
		peers[msg.IP] = Peer{Name: msg.Name, IP: msg.IP, LastSeen: time.Now()}
		peersMu.Unlock()
		// Update network device immediately when beacon received
		networkMu.Lock()
		if d, ok := networkDevices[msg.IP]; ok {
			d.HasDropSend = true
			d.Name = msg.Name
			networkDevices[msg.IP] = d
		} else {
			// New peer not yet in ARP table — add it
			networkDevices[msg.IP] = NetworkDevice{
				IP: msg.IP, Hostname: msg.Name,
				Name: msg.Name, HasDropSend: true,
			}
		}
		networkMu.Unlock()
	}
}

func startPeerCleaner() {
	for {
		time.Sleep(15 * time.Second)
		peersMu.Lock()
		for ip, p := range peers {
			if time.Since(p.LastSeen) > peerTimeout {
				delete(peers, ip)
				// Mark device as no longer having DropSend
				networkMu.Lock()
				if d, ok := networkDevices[ip]; ok {
					d.HasDropSend = false
					networkDevices[ip] = d
				}
				networkMu.Unlock()
			}
		}
		peersMu.Unlock()
	}
}

// ── MAIN ─────────────────────────────────────────────────────

func main() {
	localIP = getLocalIP()
	displayName = getHostname()
	os.MkdirAll(uploadDir, os.ModePerm)

	go startBeacon()
	go startListener()
	go startPeerCleaner()
	go startNetworkScanner()

	mux := http.NewServeMux()
	mux.HandleFunc("/upload", uploadHandler)
	mux.HandleFunc("/download/", downloadHandler)
	mux.HandleFunc("/qr/", qrHandler)
	mux.HandleFunc("/network", networkHandler)
	mux.HandleFunc("/scan", scanHandler)
	mux.HandleFunc("/me", meHandler)
	mux.HandleFunc("/setname", setNameHandler)
	mux.HandleFunc("/send", sendToPeerHandler)
	mux.HandleFunc("/receive", receiveHandler)
	mux.HandleFunc("/inbox", inboxHandler)

	stripped, _ := fs.Sub(staticFiles, "static")
	mux.Handle("/", http.FileServer(http.FS(stripped)))

	fmt.Println("DropSend is running!")
	fmt.Printf("Open:    http://localhost:8080\n")
	fmt.Printf("Network: http://%s:8080\n", localIP)
	fmt.Printf("Name:    %s\n", displayName)

	go func() {
		time.Sleep(500 * time.Millisecond)
		exec.Command("xdg-open", "http://localhost:8080").Start()
	}()

	log.Fatal(http.ListenAndServe(port, corsMiddleware(mux)))
}
