# Bridge: Autonomous Multi-Device Mesh Sync, Virtual Printing & Status Dot

A lightweight, zero-dependency Windows application (`bridge.exe`) providing automatic **multi-device mesh folder synchronization**, **native virtual printer integration (Ctrl+P)**, **isolated parallel silent remote printing**, **autonomous Tailscale onboarding**, and a **multifunctional taskbar status dot** across any number of Windows PCs (Unit 1, Unit 2, Unit 3, Unit 4, Unit 5...) over Tailscale.

Designed for low-end Windows 10/11 machines with **idle RAM < 25 MB** and **idle CPU near 0%**.

---

## 🌟 Architecture & Core Capabilities

```
    [ Unit 1 ] (100.x.y.1)                [ Unit 2 ] (100.x.y.2)               [ Unit 3 ] (100.x.y.3)
+----------------------------+        +----------------------------+       +----------------------------+
| 🟢 Status Dot (2/2 online) |        | 🟢 Status Dot (2/2 online) |       | 🟢 Status Dot (2/2 online) |
|                            |        |                            |       |                            |
| 📁 Global Shared Folder    |<======>| 📁 Global Shared Folder    |<=====>| 📁 Global Shared Folder    |
|    (C:\Bridge\Shared)      |  Mesh  |    (C:\Bridge\Shared)      | Mesh  |    (C:\Bridge\Shared)      |
|                            |  Sync  |                            | Sync  |                            |
| 🖨️ Print on Unit 2 (9101)  |=======>| 🖨️ Physical Printer        |       |                            |
| 🖨️ Print on Unit 3 (9102)  |============================================>| 🖨️ Physical Printer        |
| 🖨️ Physical Printer        |<=======| 🖨️ Print on Unit 1 (9101)  |       |                            |
|                            |<============================================| 🖨️ Print on Unit 1 (9101)  |
+----------------------------+        +----------------------------+       +----------------------------+
```

### 1. Autonomous Tailscale Setup (Zero-Friction Onboarding)
- When `bridge.exe` runs, it checks whether Tailscale is installed, running, and logged in.
- If Tailscale is missing or disconnected, `bridge.exe` installs it silently in the background, initiates authentication, and opens your default browser directly to the Tailscale login page.
- The terminal displays a live countdown while waiting for you to log in via the browser. Once Tailscale assigns a private IP (`100.x.y.z`), Bridge proceeds automatically with zero manual terminal commands required.

### 2. Device Naming & First-Time Setup UX
- Never silently reuses old, stale device names.
- Always asks for the friendly name for this computer (e.g. `Unit 1`, `Unit 2`, `Office-PC`, `Billing-PC`) and displays its local Tailscale IP address.
- When entering a peer's IP (e.g. `100.x.y.1`), Bridge automatically queries the peer node, detects its friendly name (`Unit 1`), and automatically configures virtual printer `Print on Unit 1`.
- When Device 2 connects to Device 1, Device 1 also auto-detects Device 2 and configures `Print on Unit 2` immediately.

### 3. Multi-Device Global File Sharing (Units 1, 2, 3, 4, 5...)
- **True Mesh Synchronization:** All devices share a single folder at `C:\Bridge\Shared` (accessible via Desktop shortcut).
- Any file created, modified, or deleted on *any* unit instantly replicates across *all* connected units in parallel.
- Non-blocking peer broadcast ensures that an offline or slow device does not block or slow down synchronization to other online devices.
- Built-in SHA-256 fingerprinting prevents loopback echo and sync storms.

### 4. Multi-Device Virtual Printing (Parallel & Non-Blocking)
- Each peer device gets its own independent Windows virtual printer:
  - **Unit 1** shows: `Print on Unit 2`, `Print on Unit 3`, etc.
  - **Unit 2** shows: `Print on Unit 1`, `Print on Unit 3`, etc.
  - **Unit 3** shows: `Print on Unit 1`, `Print on Unit 2`, etc.
- Each virtual printer binds to an independent RAW TCP spooler port (`127.0.0.1:9101`, `9102`, `9103`...).
- **Simultaneous Usage:** Multiple users can print simultaneously across the network without waiting for each other or colliding in queue.
- **Echo Loop Prevention:** Directs remote print jobs strictly to real physical printers (filtering out virtual spooler ports) and blocks reflected loopback jobs via `X-Bridge-Origin`.
- **Default Printer Protection:** Windows default printer is permanently safeguarded (`LegacyDefaultPrinterMode = 1`) so Bridge will never hijack your normal local office printer.

### 5. Real-Time Multifunctional Taskbar Status Dot
- Displays a small live status dot in the Windows notification area (near the clock):
  - 🟢 **Green Dot:** All configured peers are online and connected (e.g., `Bridge: 3/3 peers online`).
  - 🟡 **Yellow Dot:** Active file sync in progress, print job transmitting, or partial mesh (e.g., `Bridge: 2/3 peers online`).
  - 🔵 **Blue Dot:** Remote print job completed successfully.
  - ⚪ **Gray Dot:** All peers are currently offline or network disconnected.
- **Lightweight Polling:** Periodic health checks run concurrently in background goroutines every 10 seconds with sub-second timeouts, keeping CPU and network overhead negligible.
- **Taskbar Context Menu:**
  - 📁 *Open Shared Folder*
  - 🖨️ *Print on <PeerName>... (for each configured peer)*
  - ℹ️ *Network Status & Devices* (shows breakdown of online/offline units)
  - 🔄 *Sync Now*
  - ❌ *Exit*

---

## ⚡ Step-by-Step Setup Walkthrough

### Setup on Device 1 (e.g., "Unit 1")
1. Download or copy **`bridge.exe`** to your computer.
2. Double-click **`bridge.exe`** (click **Yes** on the Windows UAC Administrator prompt).
3. **Tailscale Check:**
   - If Tailscale is not logged in, `bridge.exe` automatically opens your web browser. Click **Log in** in the browser.
   - `bridge.exe` detects your successful login and displays your Tailscale IP (e.g., `100.64.0.1`).
4. **Device Naming:**
   ```text
   ==================================================
                 Bridge Initial Setup
   ==================================================
   [✓] Tailscale connected!
       Your Tailscale IP is: 100.64.0.1

   Enter a friendly name for this computer: Unit 1
   ```
5. **Peer Prompt:**
   ```text
   Enter peer Tailscale IP (leave blank if setting up Unit 1): [Press Enter]
   ```
6. Setup completes automatically! The terminal closes and the status dot appears in your taskbar near the clock.

---

### Setup on Device 2 (e.g., "Unit 2")
1. Copy **`bridge.exe`** to Device 2 and double-click it.
2. Log in via browser if prompted by Tailscale.
3. **Device Naming:**
   ```text
   Enter a friendly name for this computer: Unit 2
   ```
4. **Peer Prompt:**
   ```text
   Enter peer Tailscale IP: 100.64.0.1
   [✓] Connected to peer: Unit 1 (100.64.0.1)
   [✓] Virtual Printer 'Print on Unit 1' registered.
   ```
5. Press **Enter** to start Bridge.
6. **Instant Bi-Directional Link:**
   - Device 2 shows `Print on Unit 1` in its printer list and `Global Shared Folder` on its desktop.
   - Device 1 instantly detects Device 2 and creates `Print on Unit 2` in its printer list.
   - Both taskbar dots turn 🟢 **Green** (`Bridge: 1/1 peers online`).

---

### Setup on Device 3, 4, 5... (Adding Additional Devices)
1. Copy **`bridge.exe`** to Device 3 and double-click it.
2. Enter friendly name: `Unit 3`.
3. Enter the Tailscale IP of any existing device (e.g. `100.64.0.1`).
4. Bridge automatically discovers all other devices in the mesh through transitive peer exchange!
5. **Result:**
   - **Device 3** has printers: `Print on Unit 1`, `Print on Unit 2`.
   - **Device 1** has printers: `Print on Unit 2`, `Print on Unit 3`.
   - **Device 2** has printers: `Print on Unit 1`, `Print on Unit 3`.
   - All three share the same real-time folder at `C:\Bridge\Shared`.
   - All taskbar dots display 🟢 **Bridge: 2/2 peers online**.

---

## 🔄 How to Upgrade an Existing Installation

1. Copy the newly compiled `bridge.exe` to `C:\BridgeApp\bridge.exe` (or run `install.ps1`).
2. When `bridge.exe` starts, it automatically:
   - Detects existing configuration and migrates legacy settings seamlessly.
   - Allocates dedicated virtual spooler ports (`9101`, `9102`, etc.) for every peer.
   - Reconfigures Windows Firewall for ports `8765` and `9100-9120`.
   - Cleans up legacy drop-to-print folders and obsolete shortcuts.
   - Installs virtual printers for all known peers.
3. No configuration data or synchronized files are lost.

---

## 📂 Everyday Usage Guide

### 1. Print from Any Application (Ctrl+P)
- Open any document, PDF, or drawing in **Adobe Acrobat / Reader**, **Microsoft Word**, **Excel**, **AutoCAD**, or **Google Chrome**.
- Press **Ctrl+P** (Print).
- In the **Printer** dropdown, select target unit:
  - `Print on Unit 1`
  - `Print on Unit 2`
  - `Print on Unit 3`
- Click **Print**. The document is captured by the Windows Spooler and sent directly to the destination device, where it prints silently to the physical printer.

### 2. Right-Click Send-To Printing
- In Windows Explorer, right-click any PDF or document file.
- Navigate to **Send to** ➔ click **Print on <UnitName>**.
- The file transmits immediately in the background and prints.

### 3. Mesh Shared Folder
- Open the desktop shortcut **`Global Shared Folder`** (or browse to `C:\Bridge\Shared`).
- Any file saved here is immediately replicated to all online units in the mesh.
- If a unit is temporarily offline, it receives all updated files automatically as soon as it reconnects.

### 4. Taskbar Status Dot
- Look at the bottom-right corner of your screen next to the clock.
- **Hover** over the dot to see peer status (e.g., `Bridge: 2/2 peers online | Unit 1 (Online), Unit 2 (Online)`).
- **Right-Click** the dot to quickly open the shared folder or print to any specific unit.

---

## ⚙️ Technical Specifications

| Component | Specification |
| :--- | :--- |
| **Binary** | Single standalone Windows executable (`bridge.exe`, ~7.7 MB) |
| **Runtime Dependencies** | None (pure Go, native Win32 APIs, GDI, Winsock, Winspool) |
| **Idle Resource Usage** | < 25 MB RAM, 0% CPU |
| **Spooler Engine** | RAW TCP Port Monitor (`127.0.0.1:9101-9120`) |
| **Silent PDF Engine** | Embedded SumatraPDF integration with 45s watchdog timer |
| **Network Security** | Tailscale WireGuard mesh + `X-Bridge-Token` header verification |
| **Default Printer Safety** | Protected via `LegacyDefaultPrinterMode = 1` registry lock |
