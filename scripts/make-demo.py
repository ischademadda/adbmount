#!/usr/bin/env python3
"""Record docs/demo.gif: drives the interactive UI in a pty against the fake adb,
renders each screen with Chrome and assembles the GIF with ffmpeg.

Needs: pyte (pip install pyte), Google Chrome, ffmpeg, a built ./adbmount, FUSE-T.
  python3 -m venv /tmp/v && /tmp/v/bin/pip install pyte && /tmp/v/bin/python scripts/make-demo.py
"""
import fcntl, html, os, pty, re, select, shutil, struct, subprocess, sys, tempfile, termios, time
import pyte

ROOT = os.path.dirname(os.path.dirname(os.path.abspath(__file__)))
CHROME = "/Applications/Google Chrome.app/Contents/MacOS/Google Chrome"
COLS, ROWS = 84, 24

COLORS = {"default": "#d4d4d4", "black": "#1e1e1e", "red": "#f14c4c", "green": "#23d18b", "brown": "#e5c07b",
          "blue": "#3b8eea", "magenta": "#d670d6", "cyan": "#29b8db", "white": "#e5e5e5"}


def color(name, fallback):
    if name == "default":
        return fallback
    if re.fullmatch(r"[0-9a-fA-F]{6}", name):
        return "#" + name
    return COLORS.get(name, fallback)


def render_html(screen):
    rows = []
    for y in range(screen.lines):
        spans = []
        for x in range(screen.columns):
            ch = screen.buffer[y][x]
            fg, bg = color(ch.fg, COLORS["default"]), color(ch.bg, "transparent")
            if ch.reverse:
                fg, bg = bg if bg != "transparent" else "#1e1e1e", fg
            style = f"color:{fg};"
            if bg != "transparent":
                style += f"background:{bg};"
            if ch.bold:
                style += "font-weight:bold;"
            if ch.underscore:
                style += "text-decoration:underline;"
            spans.append(f'<span style="{style}">{html.escape(ch.data) or " "}</span>')
        rows.append("".join(spans))
    body = "\n".join(rows)
    return f"""<html><body style="margin:0;background:#1e1e1e">
<div style="margin:14px;font:15px/20px Menlo,monospace;white-space:pre;color:#d4d4d4">{body}</div></body></html>"""


class Session:
    def __init__(self, env):
        self.screen = pyte.Screen(COLS, ROWS)
        self.stream = pyte.ByteStream(self.screen)
        self.pid, self.fd = pty.fork()
        if self.pid == 0:
            os.environ.update(env)
            os.chdir(ROOT)
            os.execv("./adbmount", ["./adbmount"])
        fcntl.ioctl(self.fd, termios.TIOCSWINSZ, struct.pack("HHHH", ROWS, COLS, 0, 0))

    def pump(self, seconds):
        end = time.time() + seconds
        while time.time() < end:
            r, _, _ = select.select([self.fd], [], [], 0.05)
            if not r:
                continue
            try:
                chunk = os.read(self.fd, 65536)
            except OSError:
                return
            if b"\x1b]11;?" in chunk:  # background colour query from the UI library
                os.write(self.fd, b"\x1b]11;rgb:1e1e/1e1e/1e1e\x1b\\")
            chunk = re.sub(rb"\x1b\][^\x07\x1b]*(\x07|\x1b\\)", b"", chunk)  # drop OSC
            chunk = chunk.replace(b"\x1b[2m", b"\x1b[38;5;244m")  # pyte ignores "faint"
            self.stream.feed(chunk)

    def wait_for(self, text, timeout=15):
        end = time.time() + timeout
        while time.time() < end:
            self.pump(0.1)
            if text in "\n".join(self.screen.display):
                return
        raise SystemExit(f"timed out waiting for {text!r}:\n" + "\n".join(self.screen.display))

    def send(self, keys):
        os.write(self.fd, keys.encode())


def main():
    for tool in (CHROME, shutil.which("ffmpeg")):
        if not tool or not os.path.exists(tool):
            raise SystemExit(f"missing tool: {tool}")
    work = tempfile.mkdtemp()
    home = os.path.join(work, "home")
    root = os.path.join(work, "phone")
    for d in ("sdcard/Music", "sdcard/DCIM", "storage/FEAB-4F2C/Music", "storage/emulated"):
        os.makedirs(os.path.join(root, d))
    os.makedirs(os.path.join(home, "mnt"))
    env = dict(
        os.environ, HOME=home, TERM="xterm-256color", FAKE_ADB_ROOT=root, FAKE_ADB_SERIALS="R5CW31ABCDE",
        FAKE_ADB_PHONE="Google|Pixel 8|15", ADBMOUNT_DIR=os.path.join(home, "mnt"),
        FAKE_ADB_STATFS="4096 33554432 18874368 18874368",       # 128 GiB, 72 GiB free
        FAKE_ADB_STATFS_SD="4096 125829120 81788928 81788928",   # 480 GiB, 312 GiB free
        PATH=os.path.join(ROOT, "testdata/fakeadb") + ":" + os.environ["PATH"],
    )
    for k in ("ADBMOUNT_CONFIG", "ADBMOUNT_STATE_DIR", "ANDROID_SERIAL"):
        env.pop(k, None)

    s = Session(env)
    frames = []  # (screen html, seconds)

    def snap(seconds):
        frames.append((render_html(s.screen), seconds))

    try:
        s.wait_for("not mounted")
        s.wait_for("SD card")
        s.pump(0.5)
        snap(2.5)
        s.send("a")
        s.wait_for("mounting")
        snap(1.2)
        s.wait_for("mounted read-write")
        s.pump(0.5)
        snap(2.5)
        s.send("r")
        s.wait_for("read-only on next mount")
        snap(2.2)
        s.send("u")
        s.wait_for("unmounted")
        s.pump(0.5)
        snap(2.0)
        s.send("q")
        s.pump(0.5)
    finally:
        subprocess.run([os.path.join(ROOT, "adbmount"), "down"], env=env, capture_output=True)

    pngs = []
    for i, (page, _) in enumerate(frames):
        path = os.path.join(work, f"f{i}.html")
        open(path, "w").write(page)
        png = os.path.join(work, f"f{i}.png")
        subprocess.run([CHROME, "--headless=new", "--hide-scrollbars", "--force-device-scale-factor=1",
                        f"--window-size={COLS * 9 + 30},{ROWS * 20 + 48}", f"--screenshot={png}", f"file://{path}"],
                       check=True, capture_output=True)
        pngs.append(png)

    listing = os.path.join(work, "list.txt")
    with open(listing, "w") as f:
        for png, (_, secs) in zip(pngs, frames):
            f.write(f"file '{png}'\nduration {secs}\n")
        f.write(f"file '{pngs[-1]}'\n")
    out = os.path.join(ROOT, "docs", "demo.gif")
    os.makedirs(os.path.dirname(out), exist_ok=True)
    subprocess.run(["ffmpeg", "-y", "-loglevel", "error", "-f", "concat", "-safe", "0", "-i", listing, "-vf",
                    "split[a][b];[a]palettegen=max_colors=64[p];[b][p]paletteuse=dither=none", "-loop", "0", out], check=True)
    shutil.copy(pngs[2], os.path.join(ROOT, "docs", "screenshot.png"))
    print("wrote", out, os.path.getsize(out) // 1024, "KiB")
    shutil.rmtree(work, ignore_errors=True)


if __name__ == "__main__":
    main()
