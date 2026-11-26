# Bridge User Walkthrough & Experience Guide

Welcome to **Bridge** — a lightweight Windows utility (`bridge.exe`) that links your Tailscale PCs into one shared workspace with a shared folder (`C:\Bridge\Shared`) and native `Ctrl+P` remote printing (`Print on <Device>`).

This walkthrough walks you through the full end-to-end user experience: from first-run setup and silent background boot to everyday printing, offline print queuing, multi-device folder sync, upgrades, and clean removal.

---

## 1. Prerequisites & First-Run Checklist

Before you launch `bridge.exe` for the first time, check these three items:

| Requirement | What to Expect | Why It Matters |
| :--- | :--- | :--- |
| **Tailscale Connectivity** | If Tailscale is already signed in, Bridge detects your `100.x.y.z` IP immediately. If Tailscale is missing or signed out, Bridge installs it silently (`winget` or official installer), starts the Tailscale service, and opens your browser for sign-in. | All device discovery, file sync, and print traffic stay inside your private Tailscale network. |
| **Administrator Rights (UAC Prompt)** | Double-clicking `bridge.exe` for first-time setup, pairing (`[1]`), upgrading (`[3]`), syncing printers (`-sync-printers`), or uninstalling (`[4]`) triggers a Windows UAC prompt. | Windows requires administrator privileges to create Standard TCP/IP printer ports (`127.0.0.1:9101+`), register native `Print on <Device>` printers, add the Windows Firewall rule (`BridgeSystemService_8765`), and register the elevated logon task (`BridgeAppLogon`). |
| **Physical Printer Readiness (Target PC)** | Any computer that will *print incoming jobs* needs a physical printer installed, powered on, and online (`WorkOffline` off). Bridge automatically skips virtual printers (`Microsoft Print to PDF`, `XPS Document Writer`, `Fax`, `OneNote`, `Print on ...`) so jobs never loop. PCs without a physical printer can still join the mesh for file sync and sending prints *to* printer-equipped PCs. | Ensures remote prints go straight to real paper without hijacking your Windows default printer (`LegacyDefaultPrinterMode = 1`), while printerless PCs report `[ONLINE | No Printer]` cleanly. |

> **About the Silent Print Engine (`SumatraPDF.exe`):**  
> Bridge does **not** bundle `SumatraPDF.exe` inside `bridge.exe`. During first-time setup or upgrade (or automatically when needed), Bridge checks `C:\BridgeApp\SumatraPDF.exe` and standard install paths; if missing, it downloads the portable SumatraPDF package (`v3.5.2`) into `C:\BridgeApp\SumatraPDF.exe` (with `winget` fallback), removes any extra `SumatraPDF.lnk` shortcuts, and sets `PrintScale = none` in `SumatraPDF-settings.txt` so 1:1 scale printing works out of the box.

---

## 2. Silent Background Boot & Tray Icon States

### Zero Terminal Flash on Boot
`bridge.exe` is built as a Windows GUI-subsystem application (`-ldflags "-H windowsgui"`). When Windows signs you in, the scheduled logon task **`BridgeAppLogon`** launches `"C:\BridgeApp\bridge.exe" -autostart` silently in your interactive desktop session (with fallback to `HKCU\Software\Microsoft\Windows\CurrentVersion\Run`).

- **No black console window flashes** at startup or during background sync/print checks.
- **Background PowerShell/Win32 helpers** run with hidden windows (`CREATE_NO_WINDOW`).
- **One instance at a time:** Background service (`Global\BridgeApp_Service_Mutex`) and interactive dashboard (`Local\BridgeApp_Interactive_Mutex`) each enforce single-instance protection. If a Control Dashboard is already open, launching `bridge.exe` again brings the existing window to the front.

### Understanding the System Tray Icon
Look near your Windows clock (click `^` if hidden in the overflow tray) for the Bridge printer/cloud status icon:

| Tray Icon | State | Meaning |
| :--- | :--- | :--- |
| **Slate Printer with Red Slash** | `Disconnected` | No peers configured yet, or all paired peers are currently offline. |
| **Gray Printer** | `Partial` | At least one paired peer is online, while one or more other peers are offline. |
| **Emerald Green Printer** | `Connected` | All paired devices are online and ready for instant sync and printing. |
| **Amber Printer** | `Printing` | Sending a print job to a peer or printing an incoming job locally. |
| **Blue Printer** (4s hold) | `PrintCompleted` | Print job was dispatched or printed successfully, then automatically returns to your baseline printer status. |
| **Amber Cloud (Up Arrow)** | `Syncing` | Uploading or synchronizing files in `C:\Bridge\Shared`. |
| **Blue Cloud (Checkmark)** (4s hold) | `SyncCompleted` | Sync finished; rapid multi-file saves are debounced (~2s window, 6s max wait) into a single clean summary balloon (e.g., `Synced 12 files`). |

### Tray Notifications & Context Menu
- **Balloon severity:** Informational updates (sync complete, print dispatched, new device found) show info notifications; offline print holds and printer setup reminders show warnings; failures show error alerts.
- **Double-click** the tray icon to open `C:\Bridge\Shared` immediately.
- **Left-click or right-click** the tray icon for the quick menu:
  - **Status Header:** `Bridge: <YourDevice> (<online>/<total> Online)` or `(Standby)`
  - **`Queued Prints (<N>)`** *(shown whenever offline print jobs are waiting)*
  - **`Connect New Device: <Name>...`** *(shown at the top whenever an unlinked Bridge PC is discovered on Tailscale)*
  - **`Open Shared Folder`** · **`Open Control Dashboard`** · **`Pair New Device...`**
  - **`Print on <Device> (Online)...`** / **`Print on <Device> (Online - No Printer)...`** / **`Print on <Device> (Offline)...`** *(clicking `(Online - No Printer)` immediately shows `"Printer Not Connected"` without opening the file picker)*
  - *Tip:* `Use Ctrl+P -> "Print on <Device>" for Word/Web/Any App`
  - **`Copy My Tailscale IP`** · **`Device Network Status`** · **`Sync Now`** · **`Open Service Logs`** · **`Exit Bridge`**

---

## 3. Multi-Device Setup (Device 1 → Device 2 → Device 3+)

### Device 1: Establishing Your Network
1. Copy `bridge.exe` to your first PC and double-click it (accept the UAC prompt).
2. Confirm Tailscale sign-in if prompted. Bridge downloads/verifies `SumatraPDF.exe` and scans Tailscale.
3. **Name this computer:** Bridge suggests `U1` (or your existing non-generic name). Press `Enter` to accept `U1` or type a friendly name like `Reception` or `Office-PC`.
4. When no other Bridge nodes are online yet, Bridge shows:
   ```text
   [*] No other active Bridge devices detected on your network.
   [✓] 'U1' will act as your first network device.
   Enter peer IP address or device name if connecting to a specific peer,
   or press Enter if this is Device 1 (or setting up other devices later):
   ```
   Press `Enter` to finish Device 1 setup. Bridge installs into `C:\BridgeApp`, creates the **`Bridge Shared Folder`** shortcut on your active Desktop, and starts the tray icon.

### Device 2: One-Prompt Pairing & Automatic Virtual Printers
1. Copy `bridge.exe` to your second PC and double-click it.
2. Name Device 2 (Bridge automatically avoids `U1` and suggests `U2`).
3. Bridge discovers `U1` via read-only `/api/health` probe (`X-Bridge-Probe: 1`) and asks:
   ```text
   [*] Current active device found: 'U1' (IP: 100.x.y.1)
   Connect to 'U1' (100.x.y.1)? [Y = Connect, N = Skip, or enter another IP/name]: Y
   ```
4. **Peer name confirmation & collision avoidance:** Bridge asks:
   ```text
   Confirm name for 100.x.y.1 [Enter to keep 'U1', or type custom name]:
   ```
   Press `Enter` to keep `U1` or enter a local custom alias. If another paired peer already uses that name, Bridge automatically appends the IP (`Name (100.x.y.z)`) and blocks self-naming collisions.
5. **What happens automatically on both PCs:**
   - Device 2 links with `U1`, introduces itself via `POST /api/peers`, and inherits any existing mesh peers.
   - Windows registers **`Print on U1`** on port **`Bridge - U1 (9101)`** (`127.0.0.1:9101`, SNMP disabled, `WorkOffline = False`) using `Microsoft XPS Document Writer v4` (with automatic feature/INF repair or `Microsoft Print To PDF` fallback).
   - Explorer **Send To** adds **`Print on U1`**.
   - Device 1 receives the introduction, registers **`Print on U2`** on **`Bridge - U2 (9101)`**, and both tray icons turn **Emerald Green**.

### Device 3+ (Full Mesh Expansion)
1. Run `bridge.exe` on Device 3 (`U3`).
2. Bridge lists every active Bridge node found on Tailscale:
   ```text
   [1] U1              (IP: 100.x.y.1)
   [2] U2              (IP: 100.x.y.2)
   Connect to these devices? [Y = Connect all, 1-N = Pick one, N = Skip, or enter IP/name]: Y
   ```
3. Pressing `Y` (or `Enter`) links Device 3 to all peers, assigns sequential local spool ports (`9101`, `9102`, ... avoiding `9100` and `8765`), and creates `Print on U1` and `Print on U2` on Device 3 while `U1` and `U2` automatically add `Print on U3`.

---

## 4. Upgrading & New Device / Port Discovery UX

Bridge **never** silently overwrites your installed binary just because you opened a new `.exe`. Upgrading is explicit, safe, and includes automatic discovery and printer repair:

1. **Detecting an updated binary:** When you launch a newer `bridge.exe` from Downloads or another folder, the Control Dashboard highlights the mismatch:
   ```text
   [!] Running from external binary: C:\Users\...\Downloads\bridge.exe
       Installed binary location:    C:\BridgeApp\bridge.exe
   [!] Binary SHA256 DIFFERS (running: ... vs installed: ...)
       Select [3] below to upgrade the installed service to this binary.
   ```
2. **Select `[3] Upgrade Bridge Installation to this executable`** (or run `bridge.exe -upgrade <path>`):
   - Stops active `bridge.exe` and `SumatraPDF.exe` processes cleanly.
   - Deploys the updated binary to `C:\BridgeApp\bridge.exe` while preserving `config.json`, peers, ignored peers, and `C:\Bridge\Shared`.
   - Refreshes tray icons, ensures `SumatraPDF.exe` (`PrintScale = none`), updates `BridgeAppLogon`, syncs virtual printers, prunes obsolete printers/ports, refreshes Windows Firewall rules, restarts the service, and verifies `/api/health`.
3. **Post-upgrade Printer & New Device Discovery (`PromptNewDiscoveries`):**
   - Verifies your local physical printer and automatically repairs any missing `Print on <Device>` virtual printers for already-paired peers.
   - Scans Tailscale for **newly discovered, unlinked Bridge devices** (respecting any devices you previously marked as ignored):
     ```text
     ======================================================================
                    NEW BRIDGE DEVICES DISCOVERED ON NETWORK
     ======================================================================
       [1] Warehouse-PC    (IP: 100.x.y.9, Host: warehouse) (Printer: Ready)
     ======================================================================
     Connect to discovered device(s)? [Y = Connect all / 1-N = Pick / S = Skip this time / I = Ignore permanently]:
     ```
     - **`Y` (or `Enter`)**: Connects all listed devices, confirms names, and verifies their `Print on <Device>` printers.
     - **`[1]`..`[N]`**: Connects only the numbered device you picked. Discovery and pairing prompts display `(Printer: Ready)` or `(No Printer)` beside each peer.
     - **`S`**: Skips for now without hiding it from future scans.
     - **`I`**: Saves the device in `ignored_peers` so Bridge stops prompting for it (you can bring it back anytime via Dashboard **`[1]` → `U` (Un-ignore)**).
4. **Runtime Tray Discovery:** While running in the background, if a new unlinked Bridge PC appears on Tailscale, Bridge shows a non-intrusive **"New Bridge Device Found"** notification and adds **`Connect New Device: <Name>...`** right at the top of the tray menu so you can pair with one click.

---

## 5. Printing Guide

### A. Native Printing (`Ctrl+P`) — Any Windows App
Use this for **Word, Excel, Chrome, Edge, Acrobat Reader, AutoCAD, Notepad, or any Windows program**:

1. Press **`Ctrl+P`** in your app.
2. Choose **`Print on <Device>`** from the Windows printer list (for example, `Print on U2`).
3. Click **Print**.

**What happens behind the scenes for 1:1 print fidelity:**
- Windows spools the job to `127.0.0.1:<Port>` (`Bridge - <Device> (<Port>)`).
- Bridge immediately closes the local TCP spool connection after reading the payload — so Windows marks the local spool job complete right away and **never** shows a false *"Printer is offline"* error.
- Bridge strips any PJL port-monitor wrappers, detects `.pdf` / `.xps` / `.oxps` format, and inspects exact page geometry (`/CropBox` preferred over `/MediaBox`, `/UserUnit`, `/Rotate`, and XPS `*_PT.xml` `PageOrientation`).
- Standard paper sizes (`A2`, `A3`, `A4`, `A5`, `A6`, `Letter`, `Legal`, `Tabloid`, `Statement`) and orientation (`portrait` / `landscape`) are preserved at **100% `noscale`** (oversized formats larger than `A2` automatically use `shrink`, and mixed-size/orientation documents let the print engine match each page).

### B. Quick Print — System Tray or Explorer Right-Click (`Send to`)
Best when you already have a ready-to-print file on disk:
- **Supported direct formats:** `.pdf`, `.xps`, `.oxps`, and images (`.png`, `.jpg`, `.jpeg`, `.tif`, `.tiff`, `.bmp`, `.gif`).
- **Via System Tray:** Click the Bridge tray icon → **`Print on <Device> (Online)...`** → pick your file in the filtered dialog (`PDF, XPS & Images`).
- **Via Right-Click:** Right-click any supported file in File Explorer → **`Send to`** → **`Print on <Device>`**.
- Images respect EXIF/TIFF orientation metadata and print with `fit` scaling so high-resolution photos aren't cropped.
- *Tip:* If you try to direct-print an Office document (like `.docx` or `.xlsx`) via Send To, Bridge blocks it with a clear message reminding you to open it and use **`Ctrl+P` → `Print on <Device>`**.

### C. Offline Hold & Auto-Flush Queue
What if the target PC is turned off or disconnected from Tailscale when you print?
- **From `Ctrl+P`:** If the peer is unreachable, Bridge automatically saves the job payload and metadata (`.meta.json` with paper size, orientation, page count) into **`C:\Bridge\OutboundQueue`** instead of discarding it, and notifies you: *"Peer `<Name>` Offline — Print Queued"*.
- **From System Tray:** Clicking **`Print on <Device> (Offline)...`** prompts: *"`<Name>` is currently offline. Queue the document to print automatically when it comes online?"* Confirming `Yes` stages the file in `C:\Bridge\OutboundQueue`.
- **Tray visibility:** While jobs are waiting, the tray menu displays **`Queued Prints (<N>)`**.
- **Automatic flush:** The moment the target peer comes back online (or on service startup when the peer is reachable), Bridge flushes queued jobs in FIFO order and notifies you (*"Sent `<N>` Queued Print(s) to `<Name>`"*).

### D. Printerless Devices & Missing Printer Behavior
Bridge distinguishes between an **offline PC** (unreachable on Tailscale) and an **online PC with no physical printer** (or with a physical printer that is unplugged/`WorkOffline`):
- **Clear Status Indicators:**
  - Devices with a connected physical printer print natively and appear as **`[ONLINE | Printer: <Name>]`** in the Dashboard and Device Network Status.
  - Devices without a physical printer (or with a disconnected/`WorkOffline` printer) appear as **`[ONLINE | No Printer]`** in the Dashboard and **`Print on <Name> (Online - No Printer)...`** in the System Tray.
- **Immediate Rejection Without Queuing (`412 Precondition Failed`):**
  - Sending a print (`Ctrl+P`, Tray, or `Send to`) to an online device that has no physical printer immediately notifies **`"Printer Not Connected"`** on the sender (HTTP `412 Precondition Failed`) and **`"Print Rejected — No Printer"`** on the receiver.
  - Because the peer is online and simply lacks a ready printer, the job is **not** queued in `C:\Bridge\OutboundQueue` as an offline job.
- **Seamless File Sync & Outbound Printing:** A printerless device continues to sync `C:\Bridge\Shared` and can still print *to* any other printer-equipped peer on the mesh without interruption.

---

## 6. Shared Folder Mesh (`C:\Bridge\Shared`)

Open **`Bridge Shared Folder`** from your Desktop, the tray menu (or double-clicking the tray icon), or Dashboard `[6]`.

- **True Multi-Device Relay (>2 PCs):** When Device A saves or deletes a file to Device B, Device B relays that update to Device C (and every other peer except the sender), keeping meshes of 3, 4, or 5+ PCs in sync even across partial connectivity.
- **Safe-Save Debounce:** Bridge opens files non-exclusively (`FILE_SHARE_READ | FILE_SHARE_WRITE | FILE_SHARE_DELETE`) and waits until a file is unlocked and its size remains stable for `500 ms` before hashing and uploading. `.part` / `.tmp` editor staging files are ignored, and local deletions are debounced (`350 ms`) so Office/editor atomic saves don't trigger accidental deletions.
- **Conflict Copy Preservation:** If two PCs edit the same file within 60 seconds (or while disconnected) and a newer remote version arrives, Bridge preserves your local copy before updating:
  ```text
  budget (conflict U2 20261126-140500).xlsx
  ```
- **Deletion Propagation & Tombstones:** Deleting a file on any PC records a timestamped tombstone in `C:\BridgeApp\state.json` (kept for 30 days) and deletes the file across all peers (`RobustRemove` clears read-only flags and retries against Windows Explorer/antivirus locks). Recreating a file with the same name later with a newer timestamp syncs cleanly as a new file.

---

## 7. Control Dashboard Actions

Launch `bridge.exe` interactively (via double-click, Start/CLI, or Tray → **`Open Control Dashboard`**) to open the Control Dashboard:

| Option | Action | What It Does |
| :---: | :--- | :--- |
| **`[1]`** | **Connect / Pair a New Device** | Auto-elevates if needed (`-pair`), scans Tailscale for unlinked Bridge nodes, lets you pair by selection or manual IP/name, lets you un-ignore previously ignored devices (`U`), and registers virtual printers. |
| **`[2]`** | **Start / Restart Background Service** | Starts or cleanly restarts the elevated Session 1 background service and tray icon via `BridgeAppLogon`. |
| **`[3]`** | **Upgrade / Refresh Bridge Installation** | Upgrades `C:\BridgeApp\bridge.exe` from the current or specified binary (auto-elevates `-upgrade`), repairs missing virtual printers, and prompts to connect newly discovered peers. |
| **`[4]`** | **Uninstall Bridge Completely** | Auto-elevates (`-uninstall`), notifies all paired peers, removes printers/ports/tasks/registry/app files, and preserves `C:\Bridge\Shared`. |
| **`[5]`** | **Remove a Paired Device** | Deregisters a single peer cleanly on both machines without uninstalling Bridge. |
| **`[6]`** | **Open Shared Folder** | Opens `C:\Bridge\Shared` in File Explorer. |
| **`[0]`** | **Exit Dashboard** | Closes the dashboard window while keeping the Bridge background service and tray icon running. |

---

## 8. Removing a Single Peer vs. Complete Uninstallation

### Removing a Single Peer (`Dashboard [5]`)
Use this when one PC is being retired, renamed, or replaced:
1. Open the Control Dashboard and choose **`[5] Remove a Paired Device`**.
2. Pick the peer number and confirm (`y`).
3. Bridge performs a two-sided cleanup:
   - Sends **`DELETE /api/peers`** to the remote peer so *your* computer is removed from its peer list and printers too.
   - Sends **`DELETE /api/peers`** to your local service (`127.0.0.1:8765`), immediately stopping that peer's spooler TCP port, removing it from `SyncEngine`, updating `config.json`, and refreshing the tray menu.
   - Removes the `Print on <Peer>.lnk` Send To shortcut and removes `Print on <Peer>` + its `Bridge - <Peer> (<Port>)` port from Windows Printers (if not elevated, run `bridge.exe -sync-printers` as Administrator to remove the Windows printer entry).

### Complete Uninstallation (`Dashboard [4]` or `bridge.exe -uninstall`)
Use this to wipe Bridge cleanly from a machine:
1. Choose **`[4]`** in the Dashboard (or run `bridge.exe -uninstall`, optionally adding `-yes` / `-y` for unattended scripts).
2. Bridge executes a complete 7-step teardown:
   - **`[0/6]` Peer Deregistration:** Sends `DELETE /api/peers` to every configured peer so other PCs immediately stop showing this device as paired/online.
   - **`[1/6]` Process & Task Shutdown:** Stops and deletes `BridgeAppLogon` and legacy `BridgeSystemService`, and terminates all `bridge.exe` and `SumatraPDF.exe` processes.
   - **`[2/6]` Startup Purge:** Clears `HKCU`, `HKLM`, `WOW6432Node`, all `HKEY_USERS` hives, `Software\BridgeApp`, and Startup folder `.lnk` files.
   - **`[3/6]` Virtual Printer & Port Cleanup:** Clears stuck jobs, protects your physical default printer, removes all `Print on *` printers, restarts the Print Spooler, and removes all `Bridge - *`, `Bridge_Port_*`, and `Bridge_TCP_Port` ports.
   - **`[4/6]` Firewall Cleanup:** Removes `BridgeSystemService_8765`.
   - **`[5/6]` Shortcut Cleanup:** Deletes Desktop and Send To shortcuts across user profiles.
   - **`[6/6]` App & Queue Cleanup:** Removes `C:\Bridge\Logs`, `C:\Bridge\PrintQueue`, `C:\Bridge\PrintFailed`, `C:\Bridge\Print`, and `C:\BridgeApp` (using a post-exit self-delete helper when run from inside `C:\BridgeApp`).
   - **Your files in `C:\Bridge\Shared` are never deleted.**

---

## 9. Troubleshooting Matrix

| Symptom | Likely Cause | Fix |
| :--- | :--- | :--- |
| **`Print on <Device>` is missing in `Ctrl+P` (or shows `Missing` in Dashboard)** | Setup or pairing ran without Administrator privileges, or XPS Document Writer driver was disabled in Windows Features. | Run **`bridge.exe -sync-printers`** (accept UAC), or run Dashboard **`[3] Upgrade / Refresh`**. Bridge will re-enable `Printing-XPSServices-Features` or fall back to `Microsoft Print To PDF`. |
| **A removed/uninstalled device still appears on another PC** | The uninstalled PC was offline during uninstall so `DELETE /api/peers` could not reach the remaining PC. | On the remaining PC, open Control Dashboard → **`[5] Remove a Paired Device`** and remove the retired peer. |
| **Print job says *"Peer Offline — Print Queued"* or `Queued Prints (N)` appears** | The target PC is powered off, asleep, or Tailscale is disconnected on either machine. | Verify Tailscale is connected on both PCs. Once the peer responds to `/api/health`, Bridge automatically flushes `C:\Bridge\OutboundQueue` in order. |
| **New Bridge PC on Tailscale wasn't prompted during upgrade** | It was previously marked `I` (Ignore permanently) or Tailscale wasn't logged in yet. | Open Dashboard → **`[1] Connect / Pair a New Device`** and press **`U`** to un-ignore, or enter the peer's Tailscale IP/name directly. |
| **Cannot direct-print `.docx` / `.xlsx` / `.txt` via Right-Click Send To** | Send To direct-print only accepts PDF, XPS/OXPS, and image formats. | Open the document in Word/Excel/Notepad and use **`Ctrl+P` → `Print on <Device>`**. |
| **Dashboard shows `Local Printer: None (No physical printer connected...)` or `[ONLINE | No Printer]` / `"Printer Not Connected"` (`412`)** | No physical printer is installed on that PC, only virtual PDF/XPS/Fax drivers exist, or the physical printer is unplugged / set to `Use Printer Offline` (`WorkOffline`). | Connect, power on, and clear `Use Printer Offline` on the physical printer in Windows Settings → Printers & scanners; Bridge detects readiness automatically within a few seconds via `EnumPrintersW`. File sync and printing *to* other printer-equipped PCs continue to work normally. |
| **Printouts look shrunk or cut off** | Local `SumatraPDF-settings.txt` had an old `PrintScale` override or oversized > A2 PDF scaling. | Run Dashboard **`[3] Upgrade / Refresh`** to enforce `PrintScale = none` in `SumatraPDF-settings.txt`. Note that pages larger than `A2` intentionally use `shrink` to fit standard printers. |
| **Bridge didn't start at Windows logon or asks for UAC** | Group policy blocked `BridgeAppLogon` scheduled task creation, falling back to `HKCU\Run`. | Run Dashboard **`[3] Upgrade / Refresh`** as Administrator to re-register the elevated interactive `BridgeAppLogon` task. |
