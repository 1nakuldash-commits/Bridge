# Bridge: Symmetrical Two-Way File Sync, Native Virtual Printing & Status Dot

A lightweight, zero-dependency Windows application (`bridge.exe`) providing automatic **two-way folder synchronization**, **native virtual printer integration (Ctrl+P)**, **two-way silent remote printing**, and a **multifunctional taskbar status dot** between two Windows PCs over Tailscale.

Designed for low-end Windows 10/11 machines with **idle RAM < 20 MB** and **idle CPU near 0%**.

---

## 🌟 What Makes Bridge Effortless?

```
    [ PC 1: Sulobha Unit 1 ]                                  [ PC 2: Sulobha Unit 2 ]
+------------------------------+                          +------------------------------+
|  🟢 Multifunctional Dot      |                          |  🟢 Multifunctional Dot      |
|     (Windows Taskbar)        |                          |     (Windows Taskbar)        |
|                              |                          |                              |
|  📁 Unit 2 - Shared Folder   | <== (Two-Way Sync) ===>  |  📁 Unit 1 - Shared Folder   |
|  🖨️ Print on Unit 2          | == (Ctrl+P / Spooler) => |  🖨️ Physical HP Printer      |
|     (Virtual Printer)        |                          |     (Prints Silently!)       |
+------------------------------+                          +------------------------------+
```

1. **Automatic Tailscale Peer Discovery (Zero-Typing Setup):**
   - The setup wizard automatically scans your Tailscale network, detects the online peer device, resolves its friendly name, and pre-fills its IP address.
   - Setup requires simply pressing `Enter` to confirm!

2. **Native Virtual Printer in Any Program (Ctrl+P):**
   - Seamlessly appears in the **Printer** dropdown in **Adobe Acrobat / Reader**, **AutoCAD**, **Microsoft Word**, **Google Chrome**, etc., as:
     `Print on <PeerName>` (e.g., `Print on Sulobha Unit 2`).
   - When you press Print, Windows Spooler streams the job directly to the peer PC where it prints silently to the physical printer (e.g., HP Smart Tank)!
   - **Echo Loop Prevention:** Directs jobs exclusively to real physical printers (filtering out virtual loopback ports) and blocks reflected loopback origins via `X-Bridge-Origin`. Windows default printer is permanently protected from hijack (`LegacyDefaultPrinterMode = 1`).

3. **Clean Single Desktop Shortcut (No Unnecessary Print Folders):**
   - Only ONE desktop shortcut is created:
     - `"<PeerName> - Shared Folder"` (e.g., `Sulobha Unit 2 - Shared Folder` -> `C:\Bridge\Shared`).
   - Printing is 100% direct through `Ctrl+P` and Explorer right-click; no messy extra print folders are created.

4. **Native Right-Click Printing (Explorer Send-To):**
   - Right-click **ANY file** in Windows (Desktop, Downloads, etc.) ➔ **Send to** ➔ **Print on <PeerName>**!
   - Transmits immediately to the peer PC and prints silently.

5. **Multifunctional Taskbar Dot (Right Next to Your Clock):**
   - 🟢 **Green Dot:** Connected to peer and ready.
   - 🟡 **Yellow Dot:** Print job transmitting or file sync actively in progress.
   - 🔵 **Blue Dot:** Print job successfully completed (with Windows notification).
   - ⚪ **Gray Dot:** Peer is currently offline or unreachable.
   - **Hover:** Displays tooltip with connection status and printer readiness.
   - **Right-Click Menu:**
     - 📁 *Open Shared Folder*
     - 🖨️ *Print on Remote PC... (opens file picker)*
     - ℹ️ *Connection Status & Details*
     - 🔄 *Sync Now*
     - ❌ *Exit*

---

## ⚡ First-Time Setup (Takes Under 30 Seconds)

### Step 1: On Your First Computer
1. Double-click **`bridge.exe`** (click "Yes" to Administrator prompt).
2. The wizard automatically detects Tailscale and SumatraPDF (installing them silently if missing).
3. If an online Tailscale peer is detected, it displays:
   ```
   [✓] Auto-detected connected Tailscale device: <PeerName> (100.x.y.z)
   ```
4. Simply press **Enter** to accept the auto-detected settings!
5. The terminal closes and the status dot appears in your taskbar near the clock!

### Step 2: On Your Second Computer
1. Run **`bridge.exe`** on the second computer.
2. It auto-detects Computer 1 on Tailscale. Press **Enter** to confirm.
3. **Done!** Both computers link up, the status dots turn 🟢 **Green**, and you'll see:
   - `<PeerName> - Shared Folder` on your desktop.
   - `Print on <PeerName>` in your Ctrl+P print dropdown!

---

## 🔄 How to Upgrade an Already Installed System

1. Copy the newly compiled **`dist\bridge.exe`** (or run `install.ps1`) on your computer.
2. Double-click **`bridge.exe`**.
3. Bridge automatically:
   - Decommissions any old `C:\Bridge\Print` folder and cleans up drop-to-print shortcuts.
   - Preserves `<PeerName> - Shared Folder` on your desktop.
   - Registers native Windows virtual printer `Print on <PeerName>`.
   - Protects Windows default printer mode so your physical printer remains default.
   - Restarts the service with the active taskbar dot.
4. Repeat on your other PC. Both systems will immediately link and display 🟢 **Green**.

---

## 📂 Everyday Usage

- **Ctrl+P Print from Any App:** Open any PDF in Adobe Acrobat, drawing in AutoCAD, or document in Word. Press **Ctrl+P**, pick **`Print on <PeerName>`**, and click **Print**. The document transmits and prints silently on the remote printer.
- **Sync Files:** Open the **`<PeerName> - Shared Folder`** shortcut on your Desktop (or `C:\Bridge\Shared`). Files sync bi-directionally in 2-3 seconds.
- **Native Right-Click Print:** Right-click any file anywhere ➔ **Send to** ➔ **Print on <PeerName>**.
- **Taskbar Menu:** Right-click the tiny dot near your clock to view connection status or open settings.
