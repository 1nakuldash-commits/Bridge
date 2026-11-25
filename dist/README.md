# Bridge: Autonomous Multi-Device Mesh Sync, Virtual Printing & Modern Taskbar Icon

A lightweight, zero-dependency Windows application (`bridge.exe`) providing automatic **multi-device mesh folder synchronization**, **instant multi-hop file deletion propagation (with tombstones)**, **native virtual printer integration (Ctrl+P)**, **isolated parallel silent remote printing**, **autonomous Tailscale onboarding**, and a **modern multifunctional taskbar Bridge icon** across any number of Windows PCs (U1, U2, U3, Unit 4, Unit 5...) over Tailscale.

Designed for low-end Windows 10/11 machines with **idle RAM < 25 MB** and **idle CPU near 0%**.

---

## 🌟 Architecture & Core Capabilities

```
       [ Device 1: U1 ]                      [ Device 2: U2 ]                      [ Device 3: U3 ]
+----------------------------+        +----------------------------+        +----------------------------+
| 🟢 Bridge Icon (2/2 online)|        | 🟢 Bridge Icon (2/2 online)|        | 🟢 Bridge Icon (2/2 online)|
|                            |        |                            |        |                            |
| 📁 Global Shared Folder    |<======>| 📁 Global Shared Folder    |<======>| 📁 Global Shared Folder    |
|    (C:\Bridge\Shared)      |  Mesh  |    (C:\Bridge\Shared)      |  Mesh  |    (C:\Bridge\Shared)      |
|    * Multi-Hop Deletion    | Relay  |    * Multi-Hop Deletion    | Relay  |    * Multi-Hop Deletion    |
|                            |        |                            |        |                            |
| 🖨️ Print on U2 (Port 9101)  |=======>| 🖨️ Physical HP Printer     |        |                            |
| 🖨️ Print on U3 (Port 9102)  |=============================================>| 🖨️ Physical HP Printer     |
| 🖨️ Physical HP Printer     |<=======| 🖨️ Print on U1 (Port 9101)  |        |                            |
|                            |<=============================================| 🖨️ Print on U1 (Port 9101)  |
+----------------------------+        +----------------------------+        +----------------------------+
```

### 1. Robust File Sharing & Instant Multi-Hop Deletion Synchronization
- **True Mesh Synchronization:** All devices share a single folder at `C:\Bridge\Shared` (accessible via Desktop shortcut).
- **Multi-Hop Deletion Routing (Relay):** When a file is deleted from any device, every intermediate node immediately relays the deletion across all other peers in the mesh. Even across hub-and-spoke or transitive networks, deletions propagate to every device.
- **Tombstone State & Zombie File Defense:** Deleted files are tracked with high-precision timestamped tombstones (`deleted: true`). If a file with an older timestamp is detected on disk, Bridge immediately purges it and strictly suppresses re-uploading, eliminating resurrection loops.
- **Non-Blocking Windows File Handles (`FILE_SHARE_DELETE`):** All file operations open files with full `FILE_SHARE_READ | FILE_SHARE_WRITE | FILE_SHARE_DELETE`. Bridge never locks files against Windows Explorer deletion or creates "File In Use" errors.
- **Robust Read-Only & Lock Recovery (`RobustRemove`):** When deleting files on Windows, Bridge strips `FILE_ATTRIBUTE_READONLY` flags and retries with backoff against temporary locks (antivirus, thumbnail generators).
- **Offline Catch-Up Reconciliation:** When an offline device reconnects, it reconciles tombstones bidirectionally, applies missing deletions, and safely synchronizes new files.
- **Re-creation Support:** Creating a new file with the same name after deletion is immediately detected via timestamp comparison and synced cleanly across the network.

### 2. Streamlined Multi-Device First-Time Setup UX
- **Autonomous Setup:** When running `bridge.exe` for the first time, it automatically verifies Tailscale and SumatraPDF, installing them silently if missing.
- **Step 1 — Name THIS Device:** Prompts for this computer's friendly name first (e.g. `U1`, `U2`, `Office-PC`), defaulting intelligently to `U1`.
- **Step 2 — Parallel Tailscale Active Device Discovery:**
  - **Device 1 Setup:** Scans Tailscale. If no other Bridge peers are online yet, marks Device 1 as ready and displays its IP.
  - **Device 2 Setup:** Automatically detects active device `U1` with its IP (`100.x.y.1`). Prompts: `Connect to 'U1' (100.x.y.1)? [Y = Connect, or enter another IP]: `. Simply pressing `Y` or `Enter` links Device 2 to U1!
  - **Device 3 Setup:** Automatically detects both `U1` and `U2` with their IPs. Prompts: `Connect to these devices? [Y = Connect all, or enter another IP]: `. Pressing `Y` or `Enter` establishes a complete 3-way mesh!
- **Clean Background Autostart & Zero Startup Terminals:**
  - After a one-time setup, Bridge starts automatically at Windows logon in Session 1 via an elevated interactive Task Scheduler logon task (`BridgeAppLogon`, with automatic fallback to `HKCU\Run`) using `-autostart`, allowing dynamic virtual printers to register silently without UAC popups after reboots.
  - Console windows are suppressed immediately upon boot (`CREATE_NO_WINDOW` and early window hiding).
  - Legacy duplicate startup locations (HKLM, User Startup, Common Startup, old scheduled tasks) are purged during setup, install, upgrade, and uninstall, guaranteeing that exactly ONE background service instance runs with the taskbar icon, and ZERO terminal windows open on boot.
- **Single-Instance Protection:**
  - Double-clicking `bridge.exe` while another dashboard is already running brings the existing window to the front, guaranteeing that only 1 terminal window can ever be opened.
- **No Unsolicited Auto-Updates:**
  - Launching `bridge.exe` presents an intuitive Control Dashboard with clear action options. It never auto-updates, kills processes, or overwrites files behind the user's back.
  - Upgrading Bridge is an explicit user choice available in the dashboard menu (`Option [3]`).

### 3. Modern Multi-Function Taskbar Bridge Icon
- **High-DPI Multi-Resolution Bridge Icon:** Handcrafted architectural bridge icon packed with 16x16, 32x32, and 48x48 RGBA layers for pixel-perfect rendering across standard, 125%, 150%, and 200% Windows High-DPI displays.
- **Anti-Flicker Debouncing:** Status polling uses debounced hysteresis and suppresses Windows taskbar redraws unless state actually changes. The icon remains a steady 🟢 **Emerald Green** whenever the mesh is operational.
- **State Colors:**
  - 🟢 **Emerald Green:** Peer devices connected and ready.
  - 🟡 **Amber Gold:** Active file sync in progress or print job transmitting.
  - 🔵 **Electric Azure:** Print job dispatched or operation completed.
  - ⚪ **Slate Silver:** Offline or in standby mode.
- **Rich Multi-Function Taskbar Menu:**
  - 📁 *Open Shared Folder* (also triggered by **Double-Clicking** the icon)
  - 🖨️ *Print on <PeerName>...* (direct print dialog for any peer with live online status)
  - 📋 *Copy My Tailscale IP* (copies local Tailscale IP to Windows clipboard with 1 click)
  - 📊 *Device Network Status* (shows local IP, printer status, spool ports, and connected devices)
  - 🔄 *Sync Now* (triggers instant mesh inventory reconciliation)
  - 📂 *Open Service Logs* (opens `C:\Bridge\Logs` in File Explorer)
  - ❌ *Exit Bridge*

### 4. Multi-Device Virtual Printing (Parallel, Native Accuracy & Low Latency)
- **High-Fidelity Silent Print Engine (SumatraPDF):** Autonomous, self-provisioning standalone print engine (`C:\BridgeApp\SumatraPDF.exe`) renders PDF, XPS, and OpenXPS documents at true 100% 1:1 scale (`-print-settings` with `noscale`, e.g. `1-N,paper=<PaperName>,<orientation>,noscale`) and direct-printed images with clean page fitting (`-print-settings` with `fit`) so high-DPI images are never cropped.
- **Supported Formats:** Direct printing (Taskbar *Print on \<PeerName\>...* picker and Explorer *Send to*) supports **PDF** (`.pdf`), **XPS/OpenXPS** (`.xps`, `.oxps`), and **image files** (`.png`, `.jpg`, `.jpeg`, `.tif`, `.tiff`, `.bmp`, `.gif`). Native **Ctrl+P** printing via `Print on <PeerName>` supports **any Windows application** (Word, Excel, Chrome, Acrobat Reader, AutoCAD, etc.).
- **Autonomous Auto-Provisioning & Self-Healing:** The first-time setup terminal and background service automatically verify, download from direct CDN mirrors, and configure the standalone print engine (`C:\BridgeApp\SumatraPDF.exe`) without external package manager dependencies or unwanted desktop shortcuts, and place a single `Bridge Shared Folder` shortcut on the active user's Desktop.
- **PJL & Spool Stream Stripping:** In-memory stream parser detects and strips PJL (Printer Job Language) headers/trailers emitted by port monitors, guaranteeing pure, uncorrupted XPS/PDF document payload delivery to the print engine.
- **Comprehensive Virtual Printer Filtering:** Rejects all non-physical printer drivers (Generic / Text Only, OpenXPS Document Writer, PDFCreator, Fax, OneNote, etc.), guaranteeing that only true physical raster/inkjet/laser print heads receive jobs.
- **Instant Win32 EnumPrinters:** Uses native `winspool.drv` API calls (< 1ms execution, 0 subprocesses) for printer discovery, completely eliminating the legacy 3-second PowerShell CIM query delay.
- **Dedicated Virtual Printers:** Each peer device gets its own independent Windows virtual printer:
  - **U1** shows: `Print on U2`, `Print on U3`, etc.
  - **U2** shows: `Print on U1`, `Print on U3`, etc.
  - **U3** shows: `Print on U1`, `Print on U2`, etc.
- Each virtual printer binds to an independent RAW TCP spooler port (`127.0.0.1:9101`, `9102`, `9103`...).
- **Simultaneous Usage:** Multiple users can print simultaneously across the network without waiting for each other or colliding in queue.
- **Echo Loop Prevention:** Directs remote print jobs strictly to real physical printers (filtering out virtual spooler ports) and blocks reflected loopback jobs via `X-Bridge-Origin`.
- **Default Printer Protection:** Windows default printer is permanently safeguarded (`LegacyDefaultPrinterMode = 1`) so Bridge will never hijack your normal local office printer.

---

## ⚡ Step-by-Step Setup Walkthrough

### Setup on Device 1 (e.g. "U1")
1. Copy **`bridge.exe`** to Device 1.
2. Double-click **`bridge.exe`** (click **Yes** on the Windows UAC prompt).
3. `bridge.exe` checks Tailscale and opens browser login if needed.
4. **Name Device 1:**
   ```text
   ======================================================================
                          DEVICE CONFIGURATION                           
   >>> THIS DEVICE'S TAILSCALE IP: 100.92.92.81 <<<
   ======================================================================
   What name to give THIS device? (e.g. U1, U2, Unit 1) [Default: 'U1']: U1
   [✓] Configured this device as: 'U1'
   ```
5. **Network Discovery:**
   ```text
   [*] No other active Bridge devices detected on your network.
   [✓] 'U1' will act as your first network device.
   Press Enter if this is Device 1: [Press Enter]
   ```
6. Setup completes instantly! The modern Bridge icon appears in your Windows taskbar near the clock.

---

### Setup on Device 2 (e.g. "U2")
1. Copy **`bridge.exe`** to Device 2 and double-click it.
2. **Name Device 2:**
   ```text
   What name to give THIS device? (e.g. U1, U2, Unit 1) [Default: 'U1']: U2
   [✓] Configured this device as: 'U2'
   ```
3. **Auto-Discovery of Device 1:**
   ```text
   [*] Current active device found: 'U1' (IP: 100.92.92.81)
   Connect to 'U1' (100.92.92.81)? [Y = Connect, or enter another IP]: Y
   [✓] Linked with 'U1'! Virtual Printer prepared: 'Print on U1' (Port 9101)
   ```
4. **Done!**
   - Device 2 shows `Print on U1` and `Bridge Shared Folder`.
   - Device 1 immediately auto-detects Device 2 and creates `Print on U2`.
   - Both taskbar icons turn 🟢 **Emerald Green**.

---

### Setup on Device 3 (e.g. "U3")
1. Copy **`bridge.exe`** to Device 3 and double-click it.
2. **Name Device 3:** Enter `U3`.
3. **Auto-Discovery of All Existing Devices:**
   ```text
   [*] Current active devices found on your network:
       [1] U1              (IP: 100.92.92.81)
       [2] U2              (IP: 100.92.92.82)

   Connect to these devices? [Y = Connect all, or enter another IP]: Y
   [✓] Linked with 'U1'! Virtual Printer prepared: 'Print on U1' (Port 9101)
   [✓] Linked with 'U2'! Virtual Printer prepared: 'Print on U2' (Port 9102)
   ```
4. **Full 3-Way Mesh Established:**
   - **Device 3** has: `Print on U1`, `Print on U2`.
   - **Device 1** has: `Print on U2`, `Print on U3`.
   - **Device 2** has: `Print on U1`, `Print on U3`.
   - All 3 devices share the global folder `C:\Bridge\Shared`.
   - Any file added or deleted on any device updates across all 3 devices instantly.

---

## 📂 Everyday Usage Guide

### 1. File Sharing & Deletion
- Open **`Bridge Shared Folder`** on your Desktop.
- Any document, image, or PDF placed inside is synced to all connected devices in 1-2 seconds.
- When you delete a file from *any* device, it is permanently deleted from all other devices. It will never re-upload or resurrect.

### 2. Print from Any Application (Ctrl+P)
- Open any document or drawing in **Word**, **Excel**, **Chrome**, **Acrobat Reader**, or **AutoCAD**.
- Press **Ctrl+P**.
- In the printer dropdown, select target computer:
  - `Print on U1`
  - `Print on U2`
  - `Print on U3`
- Click **Print**. The document transmits and prints silently on the remote computer's physical printer.

### 3. Right-Click SendTo Printing
- Right-click any supported direct-print file (PDF, XPS/OpenXPS, PNG, JPG/JPEG, TIF/TIFF, BMP, or GIF) in Windows Explorer (Downloads, Desktop, etc.) ➔ **Send to** ➔ **Print on <PeerName>**.

### 4. Taskbar Multi-Function Icon
- **Left-Click** or **Right-Click** the modern Bridge icon near your clock:
  - Click **📋 Copy My Tailscale IP** to immediately copy your IP to the clipboard.
  - Click **📁 Open Shared Folder** to view files.
  - Click **🖨️ Print on <PeerName>...** to print directly to a specific PC.
  - Click **📊 Device Network Status** to see all connected nodes and physical printer status.
- **Double-Click** the icon to jump directly to `C:\Bridge\Shared`.

### 5. Interactive Control Dashboard & Safe Updates
- When `bridge.exe` is run after setup, it displays the **Control Dashboard** (guaranteed exactly 1 terminal window):
  - View real-time service status, IP, connected peers, and local physical printer.
  - Quick action menu:
    - `[1]` Connect / Pair a New Device
    - `[2]` Start or Restart Background Service
    - `[3]` **Upgrade / Refresh Bridge Installation** (Safely upgrades Bridge to a new version only when confirmed by the user; no surprise auto-updates!)
    - `[4]` Uninstall Bridge Completely
    - `[0]` Exit Dashboard (Background service continues running in the taskbar)
- **Computer Reboot / Startup:**
  - Bridge starts silently in the background with the taskbar icon.
  - ZERO terminal windows open on boot.
  - It runs according to your configured setup without any prompts or popups.

---

## ⚙️ Technical Specifications

| Component | Specification |
| :--- | :--- |
| **Binary** | Single standalone Windows executable (`bridge.exe`, ~8.1 MB) |
| **Runtime Dependencies** | None (pure Go, native Win32 APIs, GDI, Winsock, Winspool) |
| **Idle Resource Usage** | < 25 MB RAM, 0% CPU |
| **Spooler Engine** | RAW TCP Port Monitor (`127.0.0.1:9101-9120`) |
| **Silent PDF Engine** | Embedded SumatraPDF integration with 45s watchdog timer (`-print-settings` uses `noscale` for PDF/XPS & `fit` for images) |
| **Supported Formats** | Direct print: PDF, XPS/OXPS, PNG, JPG/JPEG, TIF/TIFF, BMP, GIF · Ctrl+P virtual printer: Any Windows app |
| **Document Geometry** | High-precision PDF & XPS geometry parser (MediaBox, CropBox, UserUnit, Rotate, ObjStm, FixedPage) |
| **Remote Print Fidelity** | 100% 1:1 scale for PDF/XPS, fit-to-page for images, paper matching (A4, Letter, Legal, etc.) & orientation |
| **Network Security** | Tailscale WireGuard mesh + `X-Bridge-Token` header verification |
| **Default Printer Safety** | Protected via `LegacyDefaultPrinterMode = 1` registry lock |

---

## 🚀 Release History

### v2.3.x — Lifecycle Reliability, Geometry Precision & Mesh Stability
- **Single Desktop Shortcut & Clean Printer Lifecycle:** Resolves the active Windows Desktop (`GetPrimaryDesktopDir`), removes duplicate `Bridge Shared Folder.lnk` / `SumatraPDF.lnk` shortcuts across Public and secondary Desktops, registers an elevated interactive Session 1 logon task (`BridgeAppLogon`), and adds `-pair` and `-sync-printers` CLI flags with properly escaped UAC `SelfElevate` arguments.
- **Validated Peer Discovery & Port Consistency:** Uses 4s read-only Tailscale `/api/health` probes (`X-Bridge-Probe: 1`), confirms peer printer names before creating `Print on <PeerName>`, validates manual IP/device-name targets before adding peers, syncs authoritative spool ports from the running service before shortcut/printer creation, and blocks self/loopback virtual spool routing.
- **Atomic Config Persistence & Safe Peer Updates:** Made `SaveConfig` atomic with temp-file + retry rename, avoided rewriting `config.json` on unchanged heartbeat/authenticated requests, and honored explicit `"configured": true` in `LoadConfig`.
- **Clean Startup, Elevation & Tray Lifecycle:** Fixed UAC self-elevation lock ordering so elevated setup never races with the parent mutex, wired tray *Sync Now* (`OnResync`) to real peer reconciliation, ensured graceful service shutdown on *Exit Bridge*, added 10 MB `bridge.log` startup rotation, and moved heavy legacy startup sweeps out of normal boot/dashboard launches.
- **Virtual Printer Hygiene:** Verified `-SNMP 0` and `WorkOffline=$false` during virtual printer checks and returned clear errors when elevation is needed.

### v2.3.0 — Zero-Defect 1:1 Scale, Spool Decoupling & Terminal Status Sync
- **Restored Peer Connectivity in Dashboard:** Live `/api/health` probes accurately reflect reachable peers as `🟢 Online` in both terminal and system tray.
- **Immediate Port Spool Acknowledgment:** Decoupled Windows RAW TCP spool handshake from remote WAN dispatch in `SpoolerListener`, preventing Windows Print Spooler timeouts.
- **Full-Document Page Count & Geometry Resolution:** Inspects full PDF/XPS documents up to 32 MB and uses `-print-settings` `noscale` for PDF/XPS and `fit` for direct-printed images while preserving existing user `SumatraPDF-settings.txt` files.
- **Safe .NET XPS Ticket Fallback & Clean Uninstall:** Null-safe `PrintTicket` resolution and clean removal of Bridge virtual printers, ports, and startup entries on uninstall.
