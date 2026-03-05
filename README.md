# 🎬 TLDV Downloader — Go Edition

A fast, concurrent HLS video downloader for [tldv.io](https://tldv.io) meetings.  
Built in Go — ships as a **single binary with zero dependencies**.

> ⚡ Downloads a 1-hour meeting in ~2–5 minutes instead of ~60 minutes by fetching 32 HLS segments simultaneously.

---

## 📋 Requirements

| Tool | Purpose | Download |
|---|---|---|
| **Go 1.20+** | Build & run the script | https://go.dev/dl/ |
| **ffmpeg** | Merge video segments at the end | https://ffmpeg.org/download.html |

### Installing Go
- **Windows** → Download the `.msi` from https://go.dev/dl/ and run it
- **macOS** → `brew install go`
- **Linux** → `sudo apt install golang-go`

Verify: `go version`

### Installing ffmpeg
- **Windows** → Download a build from https://ffmpeg.org/download.html → extract → add the `bin/` folder to your system PATH
- **macOS** → `brew install ffmpeg`
- **Linux** → `sudo apt install ffmpeg`

Verify: `ffmpeg -version`

---

## 🚀 Running the Script

### Development mode (no build step)
```cmd
go run tldv_downloader.go
```

### Production (compile to a single .exe first)
```cmd
go build -o tldv_downloader.exe tldv_downloader.go
tldv_downloader.exe
```

### Help / how to get auth token
```cmd
go run tldv_downloader.go --help
```

---

## 🔑 Getting Your Authorization Token

The downloader needs your TLDV session token to access the meeting API.

1. Go to [https://tldv.io](https://tldv.io) and **log in**
2. Open the meeting you want to download
3. Open **DevTools** → press `F12` (or right-click → Inspect)
4. Click the **Network** tab
5. **Refresh** the page (`F5`)
6. In the filter box type: `me`
8. Go to **Headers → Request Headers**
9. Copy the full `Authorization` value (starts with `Bearer …`) ( copy only the value no need to Bearer word ) 

> ⚠️ The token is session-specific and expires when you log out. If downloads start failing, grab a fresh token by refreshing the page.

---

## 📖 Usage

When you run the script it will prompt you interactively:

```
📋 Batch download mode? (y/N):
🔐 Enter your Authorization token:
📁 Output directory (Enter = current dir):
📎 Enter the TLDV meeting URL:
❓ Proceed with download? (y/N):
```

### Single video
```
Batch mode? → N
Token       → Bearer eyJhbGc...
Output dir  → C:\Videos   (or just press Enter for current folder)
URL         → https://tldv.io/app/meetings/abc123xyz
Proceed?    → y
```

### Batch mode (multiple videos)
```
Batch mode?         → y
Token               → Bearer eyJhbGc...
Output dir          → C:\Videos
URLs                → paste one per line, empty line to finish
Parallel workers    → 4  (how many meetings to download at the same time)
Proceed?            → y
```

---

## ⚡ How It's Fast

A 1-hour TLDV video is an HLS stream made up of ~1500–2000 small `.ts` chunk files.

| | Python version | Go version |
|---|---|---|
| Segment downloads | Sequential (1 at a time) | **32 goroutines in parallel** |
| 1-hour video | ~60 minutes | ~2–5 minutes |
| Deployment | Needs Python + pip installed | **Single .exe, no dependencies** |

**The process:**
1. Call TLDV API → get the m3u8 playlist URL
2. Parse the playlist → get all segment URLs
3. Download all segments **concurrently** (32 workers)
4. Merge everything into one `.mp4` with ffmpeg (takes seconds)

---

## 📁 Output Files

For each downloaded meeting the tool saves:

```
2024-03-15_10-30-00_My_Meeting_Title.mp4    ← the video
2024-03-15_10-30-00_My_Meeting_Title.json   ← meeting metadata
```

---

## ⚙️ Configuration

You can change these constants at the top of `tldv_downloader.go`:

| Constant | Default | Description |
|---|---|---|
| `segmentWorkers` | `32` | Parallel HLS segment downloads |
| `batchWorkers` | `4` | Parallel meeting downloads (batch mode) |
| `maxRetries` | `3` | Retry attempts per failed segment |
| `requestTimeout` | `30` | API request timeout (seconds) |

---

## 🛠️ Troubleshooting

**`ffmpeg not found`**
→ Make sure ffmpeg is installed and its `bin/` folder is in your system PATH. Run `ffmpeg -version` to verify.

**`Unauthorized – invalid auth token`**
→ Your token has expired. Log in to tldv.io, open a meeting, and grab a fresh token from DevTools.

**`Meeting not found`**
→ Double-check the meeting URL. The ID is the last part of the URL path.

**Segments failing / partial download**
→ Try reducing `segmentWorkers` to `16` if your network can't handle 32 concurrent connections.

---

## 📄 License

MIT — use freely, modify as needed.
