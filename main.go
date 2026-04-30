package main

import (
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"time"

	qrcode "github.com/skip2/go-qrcode"
)

var uploadDir = "./uploads"

type UploadResponse struct {
	DownloadURL string `json:"downloadUrl"`
	QRCodeURL   string `json:"qrCodeUrl"`
	FileName    string `json:"fileName"`
	FileSize    int64  `json:"fileSize"`
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

	r.ParseMultipartForm(500 << 20)

	file, handler, err := r.FormFile("file")
	if err != nil {
		http.Error(w, "Error retrieving file", http.StatusBadRequest)
		return
	}
	defer file.Close()

	os.MkdirAll(uploadDir, os.ModePerm)

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

	localIP := getLocalIP()
	downloadURL := fmt.Sprintf("http://%s:8080/download/%s", localIP, fileName)

	qrFileName := fileName + "_qr.png"
	qrPath := filepath.Join(uploadDir, qrFileName)
	err = qrcode.WriteFile(downloadURL, qrcode.Medium, 256, qrPath)
	if err != nil {
		http.Error(w, "Error generating QR code", http.StatusInternalServerError)
		return
	}

	qrURL := fmt.Sprintf("http://%s:8080/qr/%s", localIP, qrFileName)

	resp := UploadResponse{
		DownloadURL: downloadURL,
		QRCodeURL:   qrURL,
		FileName:    handler.Filename,
		FileSize:    size,
	}

	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(resp)

	go func() {
		time.Sleep(1 * time.Hour)
		os.Remove(destPath)
		os.Remove(qrPath)
	}()
}

func downloadHandler(w http.ResponseWriter, r *http.Request) {
	fileName := filepath.Base(r.URL.Path)
	filePath := filepath.Join(uploadDir, fileName)

	file, err := os.Open(filePath)
	if err != nil {
		http.Error(w, "File not found", http.StatusNotFound)
		return
	}
	defer file.Close()

	stat, _ := file.Stat()
	originalName := fileName
	for i, c := range fileName {
		if c == '_' && i > 0 {
			originalName = fileName[i+1:]
			break
		}
	}

	w.Header().Set("Content-Disposition", fmt.Sprintf(`attachment; filename="%s"`, originalName))
	w.Header().Set("Content-Length", fmt.Sprintf("%d", stat.Size()))
	http.ServeContent(w, r, originalName, stat.ModTime(), file)
}

func qrHandler(w http.ResponseWriter, r *http.Request) {
	fileName := filepath.Base(r.URL.Path)
	filePath := filepath.Join(uploadDir, fileName)
	http.ServeFile(w, r, filePath)
}

func main() {
	mux := http.NewServeMux()
	mux.HandleFunc("/upload", uploadHandler)
	mux.HandleFunc("/download/", downloadHandler)
	mux.HandleFunc("/qr/", qrHandler)

	handler := corsMiddleware(mux)

	localIP := getLocalIP()
	fmt.Printf("FileShare backend running\n")
	fmt.Printf("Local:   http://localhost:8080\n")
	fmt.Printf("Network: http://%s:8080\n", localIP)

	log.Fatal(http.ListenAndServe(":8080", handler))
}
