# DropSend

A local-network file-sharing tool written in Go. Run one binary, open the web page in a browser, and send files to other devices on the same Wi-Fi or LAN. No accounts, no cloud, no internet connection needed.

## Features

- **Peer discovery:** every instance broadcasts a UDP beacon every 3 seconds. Peers not heard from for 10 seconds drop off the list.
- **Network scan:** every 30 seconds it probes the local subnet and reads the ARP table to find devices, and marks the ones running DropSend.
- **Upload and share:** upload files up to 200 MB. Each file gets a random 128-bit token download link and a QR code, so a phone can download by scanning.
- **Send to a peer:** send a file straight to another DropSend device. Received files appear in an inbox.
- **Rate limiting:** 10 uploads per minute per IP.
- **Single binary:** the web UI is embedded with go:embed, so there is nothing else to deploy.

## Run it

Requires Go (see go.mod for the version).

```
git clone https://github.com/Dev-Deee/localnetworkfilesharing.git
cd localnetworkfilesharing
go run .
```

The app prints two addresses on start: http://localhost:8080 for this machine, and http://YOUR-LAN-IP:8080 for other devices on the network. Uploaded files are stored in ./uploads.

On Linux it also opens your browser automatically (xdg-open). On other systems, open the address yourself.

## HTTP endpoints

| Endpoint | Purpose |
|---|---|
| POST /upload | Upload a file, returns a download link, QR code link and token |
| GET /download/ | Download a shared file by token |
| GET /qr/ | QR code image for a download link |
| GET /network | List devices found on the network |
| GET /scan | Trigger a network scan |
| GET /me | This device's name and IP |
| POST /setname | Change this device's display name |
| POST /send | Send a file to a peer |
| POST /receive | Receive a file from a peer |
| GET /inbox | List received files |

## Tech

Go standard library (net/http, embed, crypto/rand), plus go-qrcode for QR generation. Plain HTML, CSS and JavaScript frontend.

## Notes

Anyone on your local network can reach the app while it is running, so use it on networks you trust.
