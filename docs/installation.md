# Installing kanshi

Kanshi is one executable. There is nothing to install alongside it: no runtime, no database, no service
account to create. This guide covers each way of running it in detail. For the short version, see the
[README](../README.md#install).

- [Windows](#windows)
- [Linux](#linux)
- [Docker Compose](#docker-compose)
- [npx](#npx)
- [Opening it to other devices](#opening-it-to-other-devices)
- [Updating](#updating)
- [Uninstalling](#uninstalling)
- [Troubleshooting](#troubleshooting)

Every release on the [Releases page](https://github.com/rene-roid/kanshi/releases) has:

| File | For |
|---|---|
| `kanshi-windows-amd64.exe` | Windows 10/11 on Intel or AMD |
| `kanshi-windows-arm64.exe` | Windows on ARM (Snapdragon laptops) |
| `kanshi-linux-amd64` | Linux on Intel or AMD |
| `kanshi-linux-arm64` | 64-bit ARM Linux: Raspberry Pi 4/5, Ampere, Graviton |
| `kanshi.service` | A systemd unit for the Linux binary |
| `SHA256SUMS` | Checksums of all of the above |

And the image `ghcr.io/rene-roid/kanshi`, for `linux/amd64` and `linux/arm64`.

To check a download: `sha256sum -c SHA256SUMS --ignore-missing` on Linux, or
`Get-FileHash kanshi-windows-amd64.exe` in PowerShell and compare it with the line in `SHA256SUMS`.

---

## Windows

### Run it

1. Download `kanshi-windows-amd64.exe` (or `-arm64`) and put it wherever you like, e.g. `C:\Tools\kanshi\`.
2. Double-click it. A console window opens showing where the dashboard is listening, and your browser
   opens <http://localhost:8100>. Closing the console window stops kanshi.

**"Windows protected your PC".** SmartScreen shows this for any executable that is not code-signed. Click
**More info**, then **Run anyway**. You only have to do this once per file.

**Firewall prompt.** Windows asks for permission the first time kanshi listens on anything other than
localhost, i.e. once you allow LAN or Tailscale access. Allow it on *Private networks*.

### What it shows on Windows

- **Processor:** overall and per-core usage, network and disk throughput. Windows has no load average, and
  no CPU temperature that can be read reliably without administrator rights, so those two are not shown.
- **Memory & volumes:** RAM and one meter per drive.
- **Storage map:** `C:\`, your user folder (`C:\Users\<you>`) and every other fixed drive. Junctions and
  symbolic links are not followed, so nothing is counted twice, and `C:\Windows`' hard-linked component
  store is only counted once. Folders your account cannot open are listed as "unreadable" under the map.
  **Run as administrator** (right-click → *Run as administrator*) to include them.
- **Containers:** if Docker Desktop is running, its containers. Otherwise the card says Docker was not
  found, and kanshi only checks again once a minute.

### Settings

Put a `kanshi.env` file next to the `.exe`, or in `%APPDATA%\kanshi\kanshi.env`, with `KEY=VALUE` lines:

```ini
KANSHI_ACCESS=lan
KANSHI_PORT=8100
KANSHI_STORAGE_ROOTS=auto,E:\Media
```

Changing the network access from the dashboard's **Network** panel writes this file for you.

From a terminal, flags work too:

```powershell
.\kanshi-windows-amd64.exe --access tailscale --port 9000
```

When started from a terminal the browser is not opened automatically; add `--open` if you want it.

### Start it automatically

To have kanshi running in the background from boot, with no window, register a scheduled task. Run this in
an **administrator** PowerShell (adjust the path):

```powershell
$exe = "C:\Tools\kanshi\kanshi-windows-amd64.exe"
$action    = New-ScheduledTaskAction -Execute $exe -Argument "--open=false"
$trigger   = New-ScheduledTaskTrigger -AtStartup
# S4U runs it without a window and without storing your password; Highest lets
# it read every folder for the storage map.
$principal = New-ScheduledTaskPrincipal -UserId $env:USERNAME -LogonType S4U -RunLevel Highest
# Tasks are stopped after three days unless told otherwise.
$settings  = New-ScheduledTaskSettingsSet -ExecutionTimeLimit ([TimeSpan]::Zero) `
               -RestartCount 3 -RestartInterval (New-TimeSpan -Minutes 1)
Register-ScheduledTask -TaskName "kanshi" -Action $action -Trigger $trigger `
  -Principal $principal -Settings $settings -Description "kanshi dashboard"
Start-ScheduledTask -TaskName "kanshi"
```

Keep its `kanshi.env` next to the `.exe` so the task finds it. `Unregister-ScheduledTask -TaskName kanshi`
removes it.

---

## Linux

### Run it

```sh
curl -fLo kanshi https://github.com/rene-roid/kanshi/releases/latest/download/kanshi-linux-amd64
chmod +x kanshi
./kanshi
```

It prints the addresses it is listening on and stops with Ctrl-C.

**Container stats** need access to the Docker socket: run it as a user in the `docker` group
(`sudo usermod -aG docker $USER`, then log in again).

**A complete storage map** needs to read every directory, including other users' homes. Rather than running
it as root, give the binary read-only access to directories:

```sh
sudo setcap cap_dac_read_search+ep ./kanshi
```

This grants reading and listing only — not writing. Without it, folders you cannot open are counted as
"unreadable" under the map.

### Linux as a service

```sh
sudo install -m 755 kanshi-linux-amd64 /usr/local/bin/kanshi
sudo install -m 644 kanshi.service /etc/systemd/system/kanshi.service
sudo systemctl daemon-reload
sudo systemctl enable --now kanshi
```

The unit runs kanshi as a throwaway user in the `docker` group, with read-only access to directories and
the rest of the system locked down. It expects the `docker` group to exist; on a machine without Docker,
remove the `SupplementaryGroups=docker` line.

Settings go in `/var/lib/kanshi/kanshi.env`, a directory systemd creates on the first start:

```sh
echo 'KANSHI_ACCESS=tailscale' | sudo tee /var/lib/kanshi/kanshi.env
sudo systemctl restart kanshi
```

Logs: `journalctl -u kanshi -f`.

---

## Docker Compose

On a machine that already runs Docker this is the tidiest option: one container, restarted automatically,
updated with a pull.

```sh
mkdir kanshi && cd kanshi
curl -fLO https://raw.githubusercontent.com/rene-roid/kanshi/main/docker-compose.yml
docker compose up -d
```

Settings go in a `.env` file in the same folder ([`.env.example`](../.env.example) lists them all):

```ini
KANSHI_ACCESS=tailscale
KANSHI_STORAGE_EXCLUDE=/var/lib/docker
```

then `docker compose up -d` again to apply them.

### What the container can see

| Mount / setting | Why |
|---|---|
| `network_mode: host` | Real host network throughput, and the ability to listen on the host's LAN and Tailscale addresses |
| `/var/run/docker.sock` (read-only) | Container stats |
| `/` → `/hostfs` (read-only, `rslave`) | The storage map. `rslave` makes drives mounted later appear without a restart |
| `kanshi-data` → `/data` | The folder size cache, so an update or restart does not measure the disk again |
| `cap_add: DAC_READ_SEARCH` | Read any directory, so the map is complete. Does not allow writing |
| `read_only`, `cap_drop: ALL`, `no-new-privileges` | Nothing else |
| `cpus: 1.0`, `mem_limit: 96m` | A ceiling; normal use is a fraction of this |

Pin a version with `KANSHI_VERSION=0.2.0` in `.env`; the default is `latest`.

To build the image from a checkout instead of pulling it: `docker compose up -d --build`.

---

## npx

With Node.js 18+ and Docker (Compose v2) installed:

```sh
npx @yuuki824/kanshi                 # start it
npx @yuuki824/kanshi update          # pull the newest image and restart
npx @yuuki824/kanshi logs -f         # follow the logs
npx @yuuki824/kanshi down            # stop and remove it
```

This runs the same Compose setup as above. A `.env` file in the directory you run it from is used.

---

## Opening it to other devices

By default only the machine kanshi runs on can open it. Choose who else can:

| `KANSHI_ACCESS` | Adds |
|---|---|
| `lan` | The machine's private addresses on real network adapters, wired and Wi-Fi. Docker, VM and WSL networks are left out |
| `tailscale` | The machine's Tailscale addresses (`100.x.y.z` and `fd7a:115c:a1e0::…`) |
| `all` | Every address. There is no login, so only use this behind a firewall or an authenticating proxy |
| an IP address | That address, e.g. `192.168.1.50` |

Combine them with commas (`lan,tailscale`). Set it with:

- the **Network** button at the bottom of the dashboard, when it is opened on the machine itself (not
  available when the setting comes from a flag or the environment, since those would override it on the next
  start)
- `--access` on the command line
- `KANSHI_ACCESS` in the environment, `.env` or `kanshi.env`

Addresses are re-checked every 30 seconds, so kanshi picks up Tailscale connecting after it started, a new
Wi-Fi network, or a changed DHCP lease without a restart.

If you used an earlier version: the old `KANSHI_HOST` setting still works. `KANSHI_HOST=0.0.0.0` means `all`,
and a specific address means that address plus localhost.

---

## Updating

| Installed with | Update by |
|---|---|
| Windows `.exe` | Download the new `.exe` and replace the old one |
| Linux binary | Download the new binary, replace it, `sudo systemctl restart kanshi` if it runs as a service |
| Docker Compose | `docker compose pull && docker compose up -d` |
| npx | `npx @yuuki824/kanshi update` |

Settings files are untouched by an update.

## Uninstalling

- **Windows:** delete the `.exe`, `%APPDATA%\kanshi\`, and the scheduled task if you made one.
- **Linux service:** `sudo systemctl disable --now kanshi`, then remove `/etc/systemd/system/kanshi.service`,
  `/usr/local/bin/kanshi` and `/var/lib/kanshi`.
- **Docker Compose / npx:** `docker compose down` (or `npx @yuuki824/kanshi down`), then
  `docker image rm ghcr.io/rene-roid/kanshi`.

Kanshi writes nothing anywhere else.

---

## Troubleshooting

**"cannot listen on localhost on port 8100"** — something else is using the port. Pick another:
`--port 8101` or `KANSHI_PORT=8101`.

**"cannot listen on http://100.x.y.z:8100 yet"** — that address is not up yet (Tailscale still connecting, say).
Kanshi keeps trying every 30 seconds and logs when it succeeds; there is nothing to do.

**Other devices cannot connect.** Check the address list printed at startup, or the Network panel: it lists
every address kanshi is listening on right now. On Windows, check that the firewall prompt was allowed on
*Private networks* (Windows Security → Firewall → Allow an app). On Linux, check `ufw status` or your firewall.

**The storage map says some folders are unreadable.** Kanshi could not list them with its permissions. Run it as
administrator (Windows), with `setcap cap_dac_read_search+ep` or as the systemd service (Linux). The Compose
file already has the capability.

**The containers card says Docker was not found.** Docker is not running, or its socket is elsewhere. Set
`DOCKER_HOST` to the right endpoint, e.g. `unix:///run/user/1000/docker.sock` for rootless Docker.

**Folders say "sizing…" for a while the first time.** Each one is measured once, at low priority and
throttled to a quarter of one core, and then cached; the first visit to `/` measures most of the disk.
`KANSHI_STORAGE_EXCLUDE=/var/lib/docker` skips the largest pile of small files on a Docker host;
`KANSHI_STORAGE_CPU=100` removes the throttle.

**Container (Compose) not starting.** `docker ps --filter name=kanshi` and `docker logs --tail=100 kanshi`.
