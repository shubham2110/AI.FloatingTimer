# LG WebOS TV Remote Control CLI (`lgtv_remote.exe`)

A lightweight, single binary CLI tool written in Go using [`github.com/snabb/webostv`](https://github.com/snabb/webostv) to control LG WebOS Smart TVs over local network WebSocket.

---

## 🛠️ Features & Key Storage

- **Auto-Pairing Key Storage**: On the first run, the tool prompts your LG TV to pair. Once accepted, the pairing key is saved directly to `lgtv_key.txt` in the **same directory as `lgtv_remote.exe`**. Subsequent runs load this key automatically so no further TV prompts appear.
- **Active App & URL Launcher**: Query currently active app/input, open any HTML URL on TV in full-screen browser, and switch back to previous inputs/apps.
- **Single / Sequential Commands**: Supports executing single or multiple commands in one invocation.
- **Custom Ports**: Connects over default port `3001` (WSS / TLS) or `3000` (WS).

---

## 🚀 How to Build

Run the following command inside `extensions/lgtv`:

```powershell
.\build.ps1
```

---

## 📋 Usage Examples

### 1. Query Currently Active App or Input
```powershell
.\lgtv_remote.exe -ip 192.168.1.4 -port 3000 active-app
```
*Output:*
```text
=== Active Foreground App ===
App ID     : com.webos.app.hdmi1
Process ID : 1842
```

### 2. Open any HTML Webpage / Timeout Screen on TV
```powershell
.\lgtv_remote.exe -ip 192.168.1.4 -port 3000 open-url http://192.168.1.10:18082/timeout.html
```

### 3. Switch Back to Active App or Input
```powershell
# Switch back to HDMI 1 input:
.\lgtv_remote.exe -ip 192.168.1.4 -port 3000 launch com.webos.app.hdmi1

# Or switch back to YouTube:
.\lgtv_remote.exe -ip 192.168.1.4 -port 3000 launch youtube.leanback.v4
```

### 4. Show Toast Notification
```powershell
.\lgtv_remote.exe -ip 192.168.1.4 -port 3000 toast "Timer started on PC!"
```

### 5. Mute / Unmute Audio
```powershell
# Toggle mute:
.\lgtv_remote.exe -ip 192.168.1.4 -port 3000 mute

# Force mute on / off:
.\lgtv_remote.exe -ip 192.168.1.4 -port 3000 mute true
.\lgtv_remote.exe -ip 192.168.1.4 -port 3000 mute false
```

### 6. List All Installed Applications
```powershell
.\lgtv_remote.exe -ip 192.168.1.4 -port 3000 list-apps
```

### 7. List All External Input Sources
```powershell
.\lgtv_remote.exe -ip 192.168.1.4 -port 3000 list-inputs
```

### 8. Multiple Commands in One Call (Timeout Workflow)
```powershell
# Check current active app and open timeout screen in one go:
.\lgtv_remote.exe -ip 192.168.1.4 -port 3000 active-app open-url http://192.168.1.10:18082/timeout.html
```

---

## ⚙️ Command Line Options

| Flag | Description | Default |
|------|-------------|---------|
| `-ip <IP>` | Target LG TV IP Address | Required |
| `-port <PORT>` | TV WebSocket port (3001 = WSS, 3000 = WS) | `3001` |
| `-keyfile <PATH>` | Custom pairing key file path | `lgtv_key.txt` in executable directory |
| `-help`, `-h` | Print help & command documentation | - |
