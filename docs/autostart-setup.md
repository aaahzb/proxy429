# Autostart Setup

Proxy429 is a **pure tray / menu-bar app**: no terminal window after startup; the status light lives in the system tray (Windows/Linux) or menu bar (macOS). For the icon to appear, it must start inside the **user login session**.

So all three platforms use **per-user autostart** (start at login) — **don't** install it as a system service (Windows Service / launchd daemon / systemd service): system services run in a desktop-less separate session and the tray icon never appears.

> Example install paths: Windows `C:\RUN_Utility\Proxy429\proxy429.exe`; macOS `/Applications/Proxy429.app`; Linux `/opt/proxy429/proxy429`. Substitute your actual location for `<install path>` below.

---

## Windows

### Method A: Startup folder (recommended, simplest)

1. `Win + R` to open "Run", type `shell:startup` and hit enter — this opens the current user's Startup folder (`%AppData%\Microsoft\Windows\Start Menu\Programs\Startup`).
2. Find your `proxy429.exe`, **right-drag it into the Startup folder**, release, and choose "Create shortcuts here".
3. Done. The proxy starts automatically at next login, and the status light appears in the tray.

### Method B: Registry (script / command-line friendly)

Write the registry `HKCU\...\Run` as the **current user** (no administrator rights needed). In PowerShell or cmd:

```cmd
reg add "HKCU\Software\Microsoft\Windows\CurrentVersion\Run" /v Proxy429 /t REG_SZ /d "\"C:\RUN_Utility\Proxy429\proxy429.exe\"" /f
```

> Keep the quotes in the path (`\"...\"`) so paths with spaces parse as a whole.

**Disabling autostart**:

```cmd
reg delete "HKCU\Software\Microsoft\Windows\CurrentVersion\Run" /v Proxy429 /f
```

Or just delete the shortcut from the Startup folder.

---

## macOS

### Method A: Login Items (recommended, simplest)

1. Drag `Proxy429.app` to `/Applications/` (the recommended location).
2. Open **System Settings -> General -> Login Items & Extensions** (macOS 13+; older versions: System Preferences -> Users & Groups -> Login Items).
3. Click "+", choose `/Applications/Proxy429.app`, add it.
4. Done. The status light appears in the menu bar automatically at next login.

### Method B: LaunchAgent (command-line / automation friendly)

Create `~/Library/LaunchAgents/com.proxy429.app.plist`:

```xml
<?xml version="1.0" encoding="UTF-8"?>
<!DOCTYPE plist PUBLIC "-//Apple//DTD PLIST 1.0//EN" "http://www.apple.com/DTDs/PropertyList-1.0.dtd">
<plist version="1.0">
<dict>
  <key>Label</key>
  <string>com.proxy429.app</string>
  <key>ProgramArguments</key>
  <array>
    <string>/usr/bin/open</string>
    <string>-a</string>
    <string>/Applications/Proxy429.app</string>
    <string>--args</string>
    <string>-config</string>
    <string>/Users/<your-username>/Library/Application Support/proxy429/config.json</string>
  </array>
  <key>RunAtLoad</key>
  <true/>
  <key>KeepAlive</key>
  <false/>
</dict>
</plist>
```

> Launching the `.app` via `open -a` properly enters the menu-bar GUI session; running `Proxy429.app/Contents/MacOS/proxy429` directly does start it but may lack GUI context.
> The two `--args -config ...` lines are optional — delete them if you don't need to specify a config.

Load and verify:

```bash
launchctl load ~/Library/LaunchAgents/com.proxy429.app.plist
launchctl list | grep proxy429      # output = loaded
```

**Disabling autostart**:

```bash
launchctl unload ~/Library/LaunchAgents/com.proxy429.app.plist
rm ~/Library/LaunchAgents/com.proxy429.app.plist
```

Or select the app in "Login Items" and click "-" to remove it.

---

## Linux

Universal across desktop environments (GNOME / KDE / XFCE etc.): drop a `.desktop` file into `~/.config/autostart/`.

Create `~/.config/autostart/proxy429.desktop`:

```ini
[Desktop Entry]
Type=Application
Name=Proxy429
Exec=/opt/proxy429/proxy429
Icon=proxy429
Terminal=false
X-GNOME-Autostart-enabled=true
```

> Change `Exec` to your actual path. `Terminal=false` ensures no terminal window pops up.

**Disabling autostart**: delete `~/.config/autostart/proxy429.desktop`.

> Headless environments (pure command-line servers managed by systemd) are out of this document's scope — there's no tray to speak of there; `systemctl --user` headless mode is suggested, but that's not this program's design purpose.

---

## Verifying autostart works

After setting up, **reboot the system** (or log out and back in), then look:

- **Windows**: the status-light icon appears in the tray at the bottom-right of the taskbar (grey = idle).
- **macOS**: the status light appears in the menu bar at the top of the screen.
- **Linux**: the status light appears in the system tray area.

If it doesn't appear, first check the path is right and the config file loads (temporarily add `"log_file": "proxy.log"` to the config, restart, and read the log).

---

## Notes

- Autostart is **per-user**; logging in as another user won't auto-start it.
- The proxy's listen port (default `127.0.0.1:8080`) is local-only; autostart doesn't expose the port to the LAN (the forwarding channel is always localhost-only, even if `listen` is mistakenly set to `0.0.0.0`).
- Upgrading: just overwrite the executable (Windows `.exe` / macOS `.app` / Linux binary); the autostart setup is unaffected.
