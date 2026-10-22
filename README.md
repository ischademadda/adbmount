# adbmount

[![ci](https://github.com/ischademadda/adbmount/actions/workflows/ci.yml/badge.svg)](https://github.com/ischademadda/adbmount/actions/workflows/ci.yml)
[![license: MIT](https://img.shields.io/badge/license-MIT-blue.svg)](LICENSE)

[Русская версия](README.ru.md)

Mount an Android phone's storage (internal memory and SD card) on macOS as a normal read-write
volume over **adb**. It needs no kernel extension (it uses [FUSE-T](https://www.fuse-t.org/)) and
nothing on the phone except USB debugging.

```
phone ──USB/Wi-Fi── adb ──► adbmount serve (Go, cgofuse) ──► FUSE-T (NFS) ──► Finder, Python, ffmpeg, foobar2000…
```

One binary does everything: `adbmount up` mounts the phone, `adbmount pull/push` copy
folders rsync-style, `adbmount wifi` cuts the cable, `adbmount doctor` tells you what is wrong.

## Requirements

* macOS (Apple Silicon or Intel)
* [FUSE-T](https://www.fuse-t.org/) and Android platform tools
* Go (only to build from source; the version in `go.mod`)
* An Android phone with **USB debugging** enabled and the computer authorized

## Install

```bash
# Homebrew (installs FUSE-T and Android platform-tools too; FUSE-T asks for your admin password)
brew install --cask ischademadda/tap/adbmount
```

Without Homebrew:

```bash
brew install --cask fuse-t android-platform-tools

# a) prebuilt universal binary (Apple Silicon + Intel) from the latest release
curl -L https://github.com/ischademadda/adbmount/releases/latest/download/adbmount-macos-universal.tar.gz | tar xz
xattr -d com.apple.quarantine adbmount 2>/dev/null; sudo mv adbmount /usr/local/bin/

# b) with Go (needs FUSE-T installed, cgo builds against it)
go install github.com/ischademadda/adbmount@latest

# c) from a checkout
git clone https://github.com/ischademadda/adbmount.git && cd adbmount
make build && sudo make install   # or: make install PREFIX=$HOME/.local
```

Then check the setup: `adbmount doctor`.

## Interactive UI

Run `adbmount` with no arguments in a terminal (or `adbmount tui`):

```
adbmount v0.1.0

Devices
▸ ● ✓ PBYP8TRSZHUS9PEM   usb   connected   Xiaomi 23108RN04Y · Android 16

Storage
  internal  50.5 GiB free of 222.2 GiB
  SD card   236.9 GiB free of 461.2 GiB  /storage/FEAB-4F2C

Mounts
▸ android  ~/mnt/android   mounted read-write
  sdcard   ~/mnt/sdcard    not mounted

Settings
  [r] read-only mounts      off
  [f] open Finder after up  off
```

The status refreshes every 2 seconds (device connected / unauthorized / offline, USB or Wi-Fi,
free space, what is mounted). Keys: `↑↓` move, `tab` switch panel, `enter` mount/unmount the
selected volume or choose a device, `a` mount all, `u` unmount all, `o` open in Finder,
`c` connect over Wi-Fi (asks for `host[:port]`), `w` switch a USB phone to Wi-Fi, `x` disconnect a
Wi-Fi device, `r` / `f` toggle the settings, `l` show the daemon log, `q` quit. Quitting leaves the
mounts running. Settings are saved in `~/Library/Application Support/adbmount/config.json` and also
become the defaults of `adbmount up` (`--rw` overrides read-only).

## Usage

```
adbmount up [--ro|--rw] [--only android|sdcard] [--open]   mount internal storage → ~/mnt/android, SD card → ~/mnt/sdcard
adbmount down [name...]                      unmount (everything by default)
adbmount status                              what is mounted
adbmount log [-f] android|sdcard             daemon log of a mount
adbmount devices                             phones, how they are connected, free space
adbmount ls [-l] [PATH]                      list a folder on the phone
adbmount pull [-n] [-f] [-j N] REMOTE... LOCAL
adbmount push [-n] [-f] [-j N] LOCAL... REMOTE
adbmount wifi enable|connect|pair|scan|disconnect|usb
adbmount doctor                              diagnose the setup
adbmount completion zsh|bash|fish            shell completion (zsh: adbmount completion zsh > "${fpath[1]}/_adbmount")
```

Phone paths accept shortcuts: `sd:Music` is the SD card's `Music` folder (the card is detected
automatically), `int:DCIM` is `/sdcard/DCIM`; absolute paths like `/storage/emulated/0/Download` work too.

Mount points live in `~/mnt` (change with `ADBMOUNT_DIR=/some/dir`). Daemon state and logs are in
`~/Library/Caches/adbfs/run`. With several devices attached, choose one with `-s SERIAL` or
`ANDROID_SERIAL=…` (see `adbmount devices`).

> **Note:** read-write mode modifies the phone's storage directly. Start with `--ro` if you only
> want to look around, and keep a backup of anything you cannot lose. The software is provided
> as-is, see [LICENSE](LICENSE).

### Copying without mounting

```bash
adbmount pull sd:Music ~/Music        # → ~/Music/Music (rsync rules: no trailing slash keeps the folder name)
adbmount pull sd:Music/ ~/Music       # → contents of Music straight into ~/Music
adbmount push -n ~/Photos/ int:Backup # dry run
adbmount push ~/Photos/ int:Backup
```

* Files whose size and modification time (±2 s, FAT/exFAT precision) already match are skipped,
  so repeating a command only copies what changed. `-f` copies everything.
* Several files are transferred in parallel (`-j`, default 4); each one is written to a temporary
  name and renamed when complete, and its size is checked afterwards. Failed files are retried
  once and listed at the end with a non-zero exit status.
* `push` does not send macOS `._*` / `.DS_Store` files (`--junk` to include them).
* Nothing is ever deleted on either side.

### Wi-Fi instead of a cable

adb can talk to the phone over the network, and everything above (mounting, `pull`, `push`) works
unchanged with a Wi-Fi serial. The phone and the Mac must be on the same network.

```bash
# Option 1, any Android: plug the phone in once
adbmount wifi enable                 # switches adb to TCP and connects; unplug the cable afterwards
adbmount up -s 192.168.1.20:5555

# Option 2, Android 11+, no cable at all: Developer options → Wireless debugging
adbmount wifi pair 192.168.1.20:41233 123456    # address and code from "Pair device with pairing code"
adbmount wifi connect 192.168.1.20:37001        # address from the main Wireless debugging screen
adbmount wifi scan                              # or find phones advertising themselves

adbmount wifi disconnect             # drop all network connections
adbmount wifi usb                    # put a phone back to USB-only mode
```

Expect lower and less steady throughput than USB (Wi-Fi quality dominates); the mount keeps
working through short hiccups thanks to the retry logic. With the cable still plugged in the
phone shows up twice, once per transport: pick one with `-s`.

### The daemon

`adbmount up` starts one background `adbmount serve` per volume. You can run it yourself:

```bash
adbmount serve -root /storage/ABCD-1234 -mount ~/mnt/sdcard -volname SDCard [-ro] [-allow-junk] [-serial S]
```

| flag | meaning |
|---|---|
| `-root` | directory on the phone exposed as the mount root (default `/sdcard`) |
| `-mount` | local mount point, created if missing |
| `-serial` | adb device serial when several devices are attached |
| `-adb` | path to the adb binary (default: `adb` from `PATH`) |
| `-ro` | read-only mount; every mutation returns `EROFS` |
| `-allow-junk` | store macOS `._*` / `.DS_Store` files instead of discarding them |
| `-volname` | name shown in Finder |

Set `ADBFS_TRACE=1` to log every operation with its duration.

## How it works

* **Listing**: one `adb exec-out find … -exec stat` per directory, cached for 10 s. Changes made
  *outside* the mount (e.g. by the phone itself) become visible within that window.
* **Reading**: a file is streamed in the background into a local temp copy, never more than 8 MB
  ahead of what was read. A read far past the streamed part (tag readers jump to the end of a
  file) is served by a separate `dd skip=` range request instead of waiting. Both were needed:
  `tail -c +N` scans the file from the start and took ~1 s on a 300 MB FLAC.
* **Writing**: a file opened for writing is first downloaded completely, modified locally, and
  uploaded **when it is flushed/closed** via `temp file + mv`, so the phone never holds a
  half-written file. A failed upload keeps the file dirty and the next flush retries.
  Nothing appears on the phone before the writer closes the file (except that `create` makes an
  empty file immediately so later lookups see it).
* **Renames/deletes/mkdir** are single `mv`/`rm`/`mkdir` calls on the phone, so moving folders
  inside a volume is instant regardless of size.
* **Unicode**: macOS hands names over as NFD while Android stores NFC (or vice versa). Lookups
  fall back to an NFC-normalised walk, so both spellings work.
* **macOS junk**: `._*`, `.DS_Store`, `.Trashes`, `.fseventsd`, … are accepted while open and then
  discarded, so copying from a Mac no longer litters the card with AppleDouble files. The volume
  also serves `.metadata_never_index` so Spotlight does not crawl the phone.
* **Resilience**: transient adb errors (device offline, daemon restarted, "closed") are retried
  with back-off for ~10 s. Per-process temp dirs live in `~/Library/Caches/adbfs/<pid>-*`; dirs
  left behind by a crashed run are removed on the next start.

Measured on a USB-attached phone: sequential read ≈ 17–20 MB/s, write ≈ 8–9 MB/s, catalog
scan of ~50 FLAC files ≈ 16 s (≈ 0.3 s/file, dominated by one adb round trip per file).

## Limitations

* Writes are whole-file: changing one tag in a 300 MB FLAC re-uploads 300 MB.
* No symlinks, hard links, xattrs or permissions (Android's `/sdcard` has none).
* Free space shown in Finder is refreshed every 30 s.
* Only one writer per file is meaningful; there is no locking.
* macOS only (FUSE-T). Linux users have better options, e.g. `adb` + `sshfs`/`madbfs`.
* For bulk work that rewrites many files (re-encoding a whole library), running the tool on the
  phone itself (Termux) is much faster than going through the cable.

## Development

```bash
make check        # go vet + go test (incl. CLI end-to-end on a fake adb) + build
make e2e-mount    # real FUSE-T mount against the fake adb (local only, needs FUSE-T)
```

`device` (in `adb.go`) is the only thing the filesystem knows about the phone. `fs_test.go` implements
it in memory, so the whole filesystem — paging, create/write/flush, truncate, rename, unlink of
open files, junk filtering, NFC/NFD, read-ahead back-pressure, upload retry — is tested without
a phone or FUSE. Tests were mutation-checked (breaking flush, unlink or back-pressure makes
specific tests fail).

| file | role |
|---|---|
| `serve.go` | the daemon: flags, temp-dir hygiene, mount |
| `fs.go` | the FUSE filesystem (cache, downloads, write-back) |
| `adb.go` | `device` interface + adb implementation, retries |
| `fs_test.go`, `main_test.go` | in-memory device + tests |
| `main.go`, `cli.go` | command dispatch and flag helpers |
| `mount.go` | `up` / `down` / `status` / `log`: starts and stops daemons |
| `device.go`, `doctor.go` | device selection, `devices`, `doctor` |
| `wifi.go` | `wifi` subcommands |
| `transfer.go` | `ls`, `pull`, `push`: planning, parallel copy, progress |
| `testdata/fakeadb` | a fake `adb` serving a local folder, used by the end-to-end tests |

## Troubleshooting

* **Mount is empty / `Resource busy` on unmount**: `adbmount down` (force-unmounts and kills only
  the pids it started). Never `pkill -f "adbmount serve"`: it also matches the other mount.
* **`no phone visible`**: check the cable, enable USB debugging and accept the authorization prompt
  on the phone; `adb devices` must show `device`, not `unauthorized`.
* **Finder is slow on huge folders**: use `--ro` while browsing; every directory is one adb call.
* **A folder created on the phone does not show up**: wait ≤ 10 s (listing cache).
* **`cannot stat /storage/... on device`**: the card is not mounted/unlocked on the phone, or the
  UUID changed; `adb shell ls /storage` shows the real name.
* **`adbmount wifi enable` cannot connect**: the phone and the Mac must share a network (guest
  Wi-Fi with client isolation blocks it); try the pairing flow, or `adb kill-server` and retry.
* **`failed to connect … No route to host`, yet `nc -z HOST 5555` works**: a stale adb server;
  `adbmount wifi` restarts it automatically, by hand it is `adb kill-server`.
* **Wi-Fi is very slow** (tens of KB/s while USB gives ~20 MB/s): that is the network, plain `adb pull`
  is equally slow. Check signal strength, router client isolation and VPN on the phone.
* **Build fails with `fuse.h` not found**: install FUSE-T (`brew install --cask fuse-t`).

## License

[MIT](LICENSE)
