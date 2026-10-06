"""Render a repeatable AV1 input with Edge/Chromium CDP, without touching desktop UI.

Requires Python Pillow and websocket-client, ffmpeg, and Edge. Only repository
source text is rendered. Output is limited-range BT.709 I420 plus a hash manifest.
This is a browser-rendered workload, not a recording of a remote session.

Windows only: the browser default below is the Edge install path and the child
processes are spawned with CREATE_NO_WINDOW.
"""
import argparse
import base64
import hashlib
import html
import io
import json
from pathlib import Path
import subprocess
import time
import urllib.request

from PIL import Image
import websocket


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--output", type=Path, required=True)
    parser.add_argument("--frames", type=int, default=360)
    parser.add_argument("--fps", type=int, default=60)
    parser.add_argument("--browser", default=r"C:\Program Files (x86)\Microsoft\Edge\Application\msedge.exe")
    args = parser.parse_args()
    if not 30 <= args.frames <= 720 or not 1 <= args.fps <= 120:
        parser.error("frames must be 30..720, fps must be 1..120")
    root = Path(__file__).resolve().parents[1]
    out = args.output.resolve()
    out.mkdir(parents=True, exist_ok=True)
    profile = out / "browser-profile"
    # A fresh profile prevents connecting to an unrelated existing browser.
    if profile.exists():
        parser.error("output browser-profile already exists; use a new output path")
    source = (root / "libs/scrap/src/common/aom.rs").read_text(encoding="utf-8")
    page = out / "scene.html"
    page.write_text('''<!doctype html><meta charset="utf-8"><style>
    *{box-sizing:border-box}body{margin:0;background:#c6d3e0;font:18px 'Segoe UI';overflow:hidden}
    header{height:48px;background:#182c42;color:white;padding:10px 24px}
    #editor{position:absolute;left:30px;top:72px;width:1300px;height:950px;background:#fff;overflow:hidden;border:1px solid #9aaabc}
    .bar{position:relative;z-index:1;height:42px;background:#e9edf1;padding:8px 20px;border-bottom:1px solid #b5c1cf}
    #code{margin:0;padding:16px 24px;font:18px/27px Consolas,monospace;color:#193957;white-space:pre}
    #window{position:absolute;left:1320px;top:130px;width:540px;height:610px;background:white;border:1px solid #708394;box-shadow:0 6px 18px #0003}
    #window p{margin:22px}table{margin:22px;border-collapse:collapse;width:490px}td{border-bottom:1px solid #bbc9d8;padding:12px}
    .swatches{display:flex;margin:22px}.swatches span{width:74px;height:70px}
    #chart{margin:22px;width:490px;height:120px;background:repeating-linear-gradient(0deg,#fff 0 23px,#cad5e0 24px);border:1px solid #8595a5}
    </style><header>P2PDesk / AV1 desktop encoding workload — 文字与界面清晰度</header>
    <div id="editor"><div class="bar">aom.rs — source editor</div><pre id="code">'''
                    + html.escape(source * 3) + '''</pre></div>
    <div id="window"><div class="bar">Encoding measurements / 编码测试</div>
    <p>Small text, colored edges and moving windows.</p><table>
    <tr><td>Codec</td><td>AV1 · realtime</td></tr><tr><td>Resolution</td><td>1920 × 1080</td></tr>
    <tr><td>Target</td><td>60 FPS</td></tr><tr><td>画面质量</td><td>保持清晰 / Clear</td></tr></table>
    <div class="swatches"><span style="background:#e74352"></span><span style="background:#239ab2"></span><span style="background:#54a266"></span><span style="background:#ecd04d"></span><span style="background:#7560aa"></span></div>
    <svg id="chart" viewBox="0 0 490 120"><path d="M0 95 L40 40 L80 85 L120 30 L160 50 L200 15 L240 75 L280 55 L320 20 L360 40 L400 10 L440 30 L490 12" fill="none" stroke="#127b9a" stroke-width="2"/></svg></div>
    <script>window.setFrame=(t)=>{
      const scroll=Math.min(t,3)*320;
      document.getElementById('code').style.transform=`translateY(${-scroll}px)`;
      const move=Math.max(0,Math.min(t-3,2));
      const w=document.getElementById('window');
      w.style.transform=`translate(${-Math.sin(move*Math.PI/4)*650}px,${Math.sin(move*Math.PI/2)*150}px)`;
      return document.body.offsetHeight;
    };</script>''', encoding="utf-8")
    browser = subprocess.Popen([
        args.browser, "--headless=new", "--disable-gpu", "--no-first-run",
        "--no-default-browser-check", "--disable-background-networking",
        "--remote-debugging-port=0", f"--user-data-dir={profile}", "about:blank",
    ], stdout=subprocess.DEVNULL, stderr=subprocess.DEVNULL,
        creationflags=subprocess.CREATE_NO_WINDOW)
    ws = None
    converter = None
    try:
        active = profile / "DevToolsActivePort"
        deadline = time.monotonic() + 30
        while not active.exists():
            if browser.poll() is not None or time.monotonic() > deadline:
                raise RuntimeError("headless browser did not start")
            time.sleep(0.1)
        port = int(active.read_text().splitlines()[0])
        with urllib.request.urlopen(f"http://127.0.0.1:{port}/json/list", timeout=10) as response:
            target = next(t for t in json.load(response) if t["type"] == "page")
        ws = websocket.create_connection(target["webSocketDebuggerUrl"], timeout=30, suppress_origin=True)
        serial = 0

        def call(method, params=None):
            nonlocal serial
            serial += 1
            ws.send(json.dumps(dict(id=serial, method=method, params=params or {})))
            while True:
                reply = json.loads(ws.recv())
                if reply.get("id") == serial:
                    if "error" in reply:
                        raise RuntimeError(reply["error"])
                    return reply.get("result", {})

        browser_version = call("Browser.getVersion")
        call("Emulation.setDeviceMetricsOverride", dict(width=1920, height=1080, deviceScaleFactor=1, mobile=False))
        call("Page.navigate", dict(url=page.as_uri()))
        for _ in range(100):
            ready = call("Runtime.evaluate", dict(expression="typeof window.setFrame === 'function' && document.readyState === 'complete'", returnByValue=True))
            if ready.get("result", {}).get("value"):
                break
            time.sleep(0.1)
        else:
            raise RuntimeError("scene did not load")
        call("Runtime.evaluate", dict(expression="document.fonts.ready", awaitPromise=True))
        raw = out / "browser-desktop.i420"
        with (out / "ffmpeg.log").open("wb") as error_log:
            converter = subprocess.Popen([
                "ffmpeg", "-hide_banner", "-loglevel", "error", "-y", "-f", "rawvideo",
                "-pixel_format", "rgb24", "-video_size", "1920x1080", "-framerate", str(args.fps),
                "-i", "pipe:0", "-vf", "scale=in_range=pc:out_range=tv:out_color_matrix=bt709",
                "-pix_fmt", "yuv420p", "-f", "rawvideo", str(raw),
            ], stdin=subprocess.PIPE, stdout=subprocess.DEVNULL, stderr=error_log,
                creationflags=subprocess.CREATE_NO_WINDOW)
            for i in range(args.frames):
                call("Runtime.evaluate", dict(expression=f"window.setFrame({i / args.fps})"))
                png = base64.b64decode(call("Page.captureScreenshot", dict(format="png", captureBeyondViewport=False))["data"])
                if i in (0, args.frames // 2, args.frames - 1):
                    (out / f"frame-{i:04d}.png").write_bytes(png)
                with Image.open(io.BytesIO(png)) as image:
                    if image.size != (1920, 1080):
                        raise RuntimeError(f"unexpected screenshot size {image.size}")
                    converter.stdin.write(image.convert("RGB").tobytes())
            converter.stdin.close()
            if converter.wait(timeout=30) != 0:
                raise RuntimeError("ffmpeg conversion failed; see ffmpeg.log")
        if raw.stat().st_size != args.frames * 1920 * 1080 * 3 // 2:
            raise RuntimeError("unexpected I420 output size")
        digest = hashlib.sha256()
        with raw.open("rb") as stream:
            for block in iter(lambda: stream.read(1024 * 1024), b""):
                digest.update(block)
        manifest = dict(width=1920, height=1080, fps=args.fps, frames=args.frames,
                        format="I420 limited BT.709", sha256=digest.hexdigest(),
                        scene_sha256=hashlib.sha256(page.read_bytes()).hexdigest(),
                        browser=browser_version, phases="0..3s scroll; 3..5s window drag; 5..6s idle",
                        note="Headless browser render; no desktop capture, network or remote session timing.")
        (out / "manifest.json").write_text(json.dumps(manifest, indent=2, ensure_ascii=False), encoding="utf-8")
        print(json.dumps(manifest, ensure_ascii=False))
    finally:
        if converter is not None and converter.poll() is None:
            converter.kill()
            converter.wait(timeout=10)
        if ws is not None:
            try:
                call("Browser.close")
            except (websocket.WebSocketException, OSError):
                pass  # Browser.close may close the socket before replying.
            ws.close()
        try:
            browser.wait(timeout=10)
        except subprocess.TimeoutExpired:
            browser.terminate()
            browser.wait(timeout=10)


if __name__ == "__main__":
    main()
