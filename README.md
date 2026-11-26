# Bridge: Autonomous Multi-Device Mesh Sync, Virtual Printing & System Tray

A lightweight, single-binary Windows application (`bridge.exe`) built with Go and native Win32 APIs providing **multi-device mesh folder synchronization**, **instant multi-hop file deletion propagation (with tombstones)**, **native Windows virtual printer integration (`Ctrl+P`)**, **persistent offline print queuing (`OutboundQueue`)**, **autonomous Tailscale onboarding**, and a **silent GUI-subsystem system tray experience** across Windows 10/11 PCs over Tailscale.

Designed for low-end Windows 10/11 machines with low idle resource usage and zero console window flash at boot.

---

## Architecture & Core Capabilities

```text
       [ Device 1: U1 ]                      [ Device 2: U2 ]                      [ Device 3: U3 ]
+----------------------------+        +----------------------------+        +----------------------------+
| Tray: Printer Green (2/2)  |        | Tray: Printer Green (2/2)  |        | Tray: Printer Green (2/2)  |
|                            |        |                            |        |                            |
| Shared Mesh Folder         |<======>| Shared Mesh Folder         |<======>| Shared Mesh Folder         |
| (C:\Bridge\Shared)         |  Relay | (C:\Bridge\Shared)         |  Relay | (C:\Bridge\Shared)         |
|  * Conflict Copies         |  Sync  |  * Conflict Copies         |  Sync  |  * Conflict Copies         |
|  * Tombstone Deletions     |        |  * Tombstone Deletions     |        |  * Tombstone Deletions     |
|                            |        |                            |        |                            |
| Print on U2 (Port 9101)    |=======>| Physical Printer (Win32)   |        |                            |
| Print on U3 (Port 9102)    |=============================================>| Physical Printer (Win32)   |
| Physical Printer (Win32)   |<=======| Print on U1 (Port 9101)    |        |                            |
|                            |<=============================================| Print on U1 (Port 9101)    |
+----------------------------+        +----------------------------+        +----------------------------+
```

### 1. Multi-Device Mesh File Sync & Multi-Hop Deletion
- **Global Shared Folder (`C:\Bridge\Shared`):** Accessible via a single canonical `Bridge Shared Folder.lnk` on the active user's Desktop (duplicate shortcuts on secondary/Public desktops are automatically cleaned up).
- **Multi-Device Relay (>2 Peers):** Incoming file uploads (`POST /api/sync`) and deletions (`DELETE /api/sync`) are relayed by receiving nodes to all other peers except `senderURL`, keeping 3+ node meshes synchronized even without direct point-to-point reachability between every pair.
- **Safe-Save Stabilization & Debounce:** Files are opened with `FILE_SHARE_READ | FILE_SHARE_WRITE | FILE_SHARE_DELETE` (`OpenSharedFileForRead`) and waited on (`WaitForStableFile`, 500 ms size stability) before hashing/uploading; `.part` / `.tmp` staging files are ignored; local deletions are debounced (350 ms) to prevent atomic-save races.
- **Conflict Copy Preservation:** If a local file is modified concurrently or within 60 seconds of a newer remote version during reconciliation, Bridge preserves `<stem> (conflict <NodeName> <YYYYMMDD-HHMMSS>).<ext>` before applying the remote file.
- **Tombstones & Lock-Safe Deletion (`RobustRemove`):** Deleted paths record timestamped tombstones in `C:\BridgeApp\state.json` (pruned after 30 days). `RobustRemove` strips `FILE_ATTRIBUTE_READONLY` and retries with backoff against Windows Explorer/antivirus locks. Recreating a deleted filename with a newer timestamp syncs cleanly across the mesh.

### 2. Zero-Flash Boot, Setup, Upgrades & Peer Lifecycle
- **GUI-Subsystem Build (`-ldflags "-H windowsgui"`):** Compiled as a Windows GUI-subsystem binary so `-autostart`, `-tray`, `-service`, and `-send-print` never allocate or flash a terminal window. Interactive commands (`-setup`, `-pair`, `-upgrade`, `-uninstall`, `-sync-printers`, `-status`, `-version`, or default Control Dashboard) attach or allocate a console cleanly when needed.
- **Elevated Session 1 Logon Task:** Registers `BridgeAppLogon` (`"C:\BridgeApp\bridge.exe" -autostart` with `RunLevel Highest`, battery-safe, unlimited execution time) and cleans up legacy duplicate startup entries, falling back to `HKCU\Software\Microsoft\Windows\CurrentVersion\Run` only if Task Scheduler is blocked.
- **First-Time Setup Wizard (`installer/wizard.go`):**
  - Verifies/installs Tailscale (`winget` or `tailscale-setup-latest.exe`) and opens browser login if disconnected.
  - Verifies or downloads standalone `SumatraPDF.exe` (`v3.5.2` portable zip) at runtime into `C:\BridgeApp\SumatraPDF.exe` (with `winget` fallback) and sets `PrintScale = none` in `SumatraPDF-settings.txt`.
  - Probes active Tailscale peers via read-only `GET /api/health` (`X-Bridge-Probe: 1`), suggests collision-free node names (`U1`, `U2`, `U3`, ...), and prompts:
    - **Device 1 (0 peers):** `Enter peer IP address or device name if connecting to a specific peer, or press Enter if this is Device 1 (or setting up other devices later):`
    - **Device 2 (1 peer):** `Connect to '<Name>' (<IP>)? [Y = Connect, N = Skip, or enter another IP/name]:`
    - **Device 3+ (2+ peers):** `Connect to these devices? [Y = Connect all, 1-N = Pick one, N = Skip, or enter IP/name]:`
  - Confirms each peer's friendly name (`Confirm name for <IP> [Enter to keep '<Name>', or type custom name]:`) with automatic self-name and duplicate-name collision avoidance.
- **Upgrades & New Device Discovery (`[3] Upgrade` / `-upgrade`):**
  - Explicitly triggered by the user (never silent/unsolicited).
  - Repairs missing virtual printers, prunes obsolete printers/ports, scans Tailscale for unlinked Bridge nodes (excluding `ignored_peers`), and prompts:
    `Connect to discovered device(s)? [Y = Connect all / 1-N = Pick / S = Skip this time / I = Ignore permanently]:`
  - Permanently ignored peers can be restored later via **`[1] Connect / Pair a New Device`** using **`U`** (Un-ignore).
- **Clean Peer Removal & Uninstallation (`DELETE /api/peers`):**
  - **Single peer (`[5] Remove a Paired Device`):** Notifies the remote peer via `DELETE /api/peers`, calls `DELETE /api/peers` locally to remove config/spooler listener/sync target, and removes the peer's SendTo shortcut and virtual printer/port.
  - **Full uninstall (`[4]` or `-uninstall` / `-yes`):** Notifies all paired peers via `DELETE /api/peers`, removes `BridgeAppLogon`/legacy tasks, kills `bridge.exe`/`SumatraPDF.exe`, purges all Run keys & Startup shortcuts, removes all Bridge virtual printers & ports, deletes firewall rule `BridgeSystemService_8765`, and removes `C:\BridgeApp` + internal queue/log dirs while preserving `C:\Bridge\Shared`.

### 3. System Tray Icons & Menu (`tray/icons.go`, `tray/tray.go`, `tray/tray_windows.go`)
- **Multi-Resolution Printer & Cloud Icons:** `EnsureIcons` generates 16x16, 32x32, and 48x48 32-bit RGBA `.ico` assets in `C:\BridgeApp\icons\` (`GeneratePrinterIcon` and `GenerateCloudIcon`; `GenerateBridgeIcon`/`GenerateDotIcon` remain for backward compatibility):
  - **Slate Printer with Red Slash (`printer_disconnected.ico`):** `StateDisconnected` — 0 peers connected / all offline.
  - **Gray Printer (`printer_gray.ico`):** `StatePartial` — some paired peers online.
  - **Emerald Green Printer (`printer_green.ico`):** `StateConnected` — all paired peers online & ready.
  - **Amber Printer (`printer_loading.ico`):** `StatePrinting` — active print transmission or printing.
  - **Blue Printer (`printer_blue.ico`):** `StatePrintCompleted` — print job dispatched/completed (4s hold).
  - **Amber Cloud (`cloud_loading.ico`):** `StateSyncing` — uploading/syncing shared files.
  - **Blue Cloud (`cloud_blue.ico`):** `StateSyncCompleted` — sync complete (4s hold; rapid bursts debounced over a 2s window / 6s max wait).
- **Tray Menu Actions:**
  - Status header (`Bridge: <Node> (<online>/<total> Online)` or `(Standby)`), plus `Queued Prints (<N>)` when offline jobs are waiting.
  - `Connect New Device: <Name>...` entries near the top when unlinked Bridge nodes are detected on Tailscale.
  - `Open Shared Folder` (also on double-click), `Open Control Dashboard`, `Pair New Device...`
  - Uniform per-peer quick print items: `Print on <Name> (Online)...`, `Print on <Name> (Online - No Printer)...` (warns `"Printer Not Connected"` before opening the file picker), and `Print on <Name> (Offline)...` (with filtered file picker for `PDF, XPS & Images`)
  - `Copy My Tailscale IP`, `Device Network Status`, `Sync Now`, `Open Service Logs`, `Exit Bridge`

### 4. High-Fidelity Native Virtual Printing & Offline Queue
- **Native `Ctrl+P` Virtual Printers:** Registers `Print on <Peer>` bound to `Bridge - <Peer> (<Port>)` on `127.0.0.1:9101+` (SNMP disabled, `WorkOffline = False`, `LegacyDefaultPrinterMode = 1` default-printer protection) using `Microsoft XPS Document Writer v4` (with DISM/INF repair or `Microsoft Print To PDF` fallback).
- **Instant Spool Handshake:** `SpoolerListener` streams the RAW TCP job to disk and immediately closes the local socket before WAN dispatch, preventing Windows Spooler timeouts and false "Printer is offline" toasts.
- **Geometry & Format Fidelity (`printer/geometry.go`, `printer/printer.go`):**
  - Strips PJL wrappers (`@PJL` / UEL) and detects `.pdf`, `.xps`, `.oxps` (including OpenXPS -> XPS conversion for native `.NET` XPS fallback), and images (`.png`, `.jpg`, `.jpeg`, `.tif`, `.tiff`, `.bmp`, `.gif`).
  - Inspects PDF `/CropBox` (falling back to `/MediaBox`), `/UserUnit`, `/Rotate`, full catalogs up to 32 MB, XPS `FixedPage` + `*_PT.xml` `PageOrientation`, and JPEG EXIF / TIFF IFD0 orientation.
  - Prints PDF/XPS at `100%` `noscale` (`shrink` for oversized > A2), images with `fit`, and forwards `X-Bridge-Paper-Size`, `X-Bridge-Orientation`, `X-Bridge-Page-Count`, `X-Bridge-Copies`, and `X-Bridge-Color`.
- **Persistent Offline Print Queue (`OutboundQueue`):** Jobs printed while a peer is offline (via `Ctrl+P` or confirmed via Tray `Print on <Peer> (Offline)...`) are persisted in `C:\Bridge\OutboundQueue` (`.meta.json` + payload) and automatically flushed in FIFO order when the peer recovers.
- **Multi-Device Printer Awareness & Readiness:**
  - `enumPrintersWin32` (`winspool.drv` `EnumPrintersW`, 5s cache TTL) detects physical printers in <1 ms, filters virtual drivers via `IsVirtualPrinterName`, and flags disconnected or `WorkOffline` (`PRINTER_STATUS_OFFLINE` / `PRINTER_STATUS_NOT_AVAILABLE`) printers as unavailable.
  - Devices with a ready physical printer report `[ONLINE | Printer: <Name>]` in the Dashboard / Device Network Status and print natively; devices without a physical printer (or with only a disconnected/`WorkOffline` printer) report `[ONLINE | No Printer]` in the Dashboard and `Print on <Name> (Online - No Printer)...` in the System Tray.
  - Sending a print to an online device with no physical printer immediately returns `412 Precondition Failed` and notifies `"Printer Not Connected"` on the sender (and `"Print Rejected — No Printer"` on the receiver) without queuing it as an offline job. Shared folder sync (`C:\Bridge\Shared`) and printing *from* a printerless device *to* printer-equipped peers continue to work seamlessly.

---

## Building from Source (Windows)

```powershell
# Always build with -H windowsgui so background/autostart launches never flash a console window
go build -ldflags "-H windowsgui" -o bridge.exe .

# Run full test suite
go vet ./...
go test -count=1 ./...
```

You can also use [install.ps1](file:///C:/Users/ADMIN/Downloads/Bridge-main/install.ps1) (run as Administrator) to build/deploy and register the service headlessly.

---

## CLI Flags & Control Dashboard

Running `bridge.exe` without flags opens the **First-Time Setup Wizard** (if unconfigured) or the **Control Dashboard** (`[1]` Pair, `[2]` Start/Restart Service, `[3]` Upgrade/Refresh, `[4]` Uninstall, `[5]` Remove a Paired Device, `[6]` Open Shared Folder, `[0]` Exit).

| Flag | Purpose |
| :--- | :--- |
| `-autostart` / `-tray` | Run silently in background with Windows System Tray icon (used by `BridgeAppLogon`) |
| `-service` | Run silently in background without tray icon |
| `-setup` | Launch interactive first-time setup wizard (auto-elevates) |
| `-pair` | Launch interactive device pairing and virtual printer setup (auto-elevates) |
| `-sync-printers` | Sync/repair desktop shortcut, SendTo links, and Windows virtual printers for configured peers (auto-elevates) |
| `-upgrade <path>` | Upgrade `C:\BridgeApp\bridge.exe` from `<path>` and scan for new devices/printers (auto-elevates) |
| `-uninstall` (`-yes` / `-y`) | Completely uninstall Bridge, notify peers via `DELETE /api/peers`, and remove printers/ports/tasks (auto-elevates; `-yes`/`-y` skips confirmation) |
| `-install -name <n> -peer <ip> [-token <t>]` | Headless automated installation |
| `-send-print [-target <peer>] <file>` | Direct-print a PDF/XPS/image file to a peer (used by Explorer `Send to`) |
| `-status` | Print local Bridge service and peer status |
| `-version` | Print application version |
| `-config <path>` | Override `config.json` path (default `C:\BridgeApp\config.json`) |

---

## Technical Specifications

| Component | Specification |
| :--- | :--- |
| **Executable** | Standalone Go Windows GUI binary (`bridge.exe`, built with `-ldflags "-H windowsgui"`) |
| **Print Engine** | Runtime-provisioned portable `SumatraPDF.exe` (`v3.5.2` downloaded to `C:\BridgeApp\SumatraPDF.exe`, `PrintScale = none`; native `.NET` XPS/OXPS fallback) |
| **Supported Formats** | Direct / SendTo: `.pdf`, `.xps`, `.oxps`, `.png`, `.jpg`, `.jpeg`, `.tif`, `.tiff`, `.bmp`, `.gif` · `Ctrl+P` virtual printer: any Windows app |
| **Spooler Ports** | Local RAW TCP listeners (`127.0.0.1:9101+`, Windows ports named `Bridge - <Peer> (<Port>)`) |
| **Offline Queue** | `C:\Bridge\OutboundQueue` (`OutboundQueue` with `.meta.json` + FIFO `FlushPeer`) |
| **HTTP API & Auth** | `:8765` (`/api/health`, `/api/peers` GET/POST/DELETE, `/api/sync`, `/api/sync/inventory`, `/api/print`, `/api/print-local`) secured by `X-Bridge-Token` over Tailscale |
| **Default Printer Guard** | `LegacyDefaultPrinterMode = 1` + physical-only default preservation |

For a complete user guide and troubleshooting matrix, see [WALKTHROUGH.md](file:///C:/Users/ADMIN/Downloads/Bridge-main/WALKTHROUGH.md).
