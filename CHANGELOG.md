# Changelog

## Unreleased

- Homebrew tap: `brew install --cask ischademadda/tap/adbmount`.
- UI: help and notices fit 80 columns; home directory shown as `~`.
- `scripts/make-demo.py` regenerates `docs/demo.gif` from the fake adb.

## 0.2.0 - 2026-10-23

- Interactive console UI (`adbmount` / `adbmount tui`): live device and mount status, mount and
  unmount, Wi-Fi, settings saved to a config file that `up` also honours (`--rw` to override).

## 0.1.0 - 2026-10-23

- Single `adbmount` binary with subcommands replaces the `adbfs` binary plus bash launcher.
- New: `devices`, `doctor`, `ls`, `pull`, `push` (rsync-style, parallel, size-verified, resumable),
  `wifi` (enable / pair / connect / scan / disconnect / usb), `completion`, `up --open`.
- `wifi` restarts a stale adb server on "No route to host".
- One phone visible over USB and Wi-Fi is no longer reported as "several devices".

## Initial version

- `adbfs` FUSE-T filesystem over adb with read-ahead, write-back on close, NFC/NFD handling,
  macOS junk filtering and retry on transient adb errors.
