#!/usr/bin/env python3
from __future__ import annotations

import base64
from pathlib import Path
import shutil

try:
    from playwright.sync_api import sync_playwright
except ImportError as exc:
    raise SystemExit("Playwright is required: python -m pip install playwright") from exc

ROOT = Path(__file__).resolve().parent.parent
HTML = (ROOT / "internal/adapter/web/index.html").read_text(encoding="utf-8")


def state_payload(*, engine: bool = False, fade: float = 1.0, pressed: bool = False) -> dict[str, object]:
    pressure = bytearray(64)
    if pressed:
        pressure[0] = 1
    rgb = bytearray(16 * 32 * 3)
    rgb[0] = 100
    return {
        "status": "ok",
        "engine_connected": engine,
        "desired_sequence": 1 if engine else 0,
        "desired_frame_age_seconds": 0.01 if engine else -1,
        "fade_ratio": fade,
        "fps": 0,
        "configured_fps": 50,
        "udp_write": "available",
        "floor_seen": True,
        "floor_rotation": 0,
        "active_pressed_tiles": 1 if pressed else 0,
        "pressure_base64": base64.b64encode(pressure).decode(),
        "rgb_base64": base64.b64encode(rgb).decode(),
        "window_minutes": 5,
        "tile_stats": [{"presses": 1 if i == 0 else 0, "duration": 2.5 if i == 0 else 0} for i in range(512)],
        "debug_controls_enabled": True,
    }


def main() -> int:
    initial = state_payload()
    with sync_playwright() as playwright:
        launch_options: dict[str, object] = {"headless": True}
        chromium = shutil.which("chromium") or shutil.which("chromium-browser")
        if chromium:
            launch_options["executable_path"] = chromium
            launch_options["args"] = [
                "--no-sandbox",
                "--disable-dev-shm-usage",
                "--disable-gpu",
                "--single-process",
                "--no-zygote",
            ]
        browser = playwright.chromium.launch(**launch_options)
        page = browser.new_page(viewport={"width": 1280, "height": 800})
        page.evaluate(
            """() => {
            window.__requests = [];
            window.__mockState = null;
            window.fetch = async (url, options = {}) => {
              window.__requests.push({url: String(url), method: options.method || 'GET', body: options.body || ''});
              return {
                ok: true,
                status: options.method === 'POST' ? 204 : 200,
                json: async () => window.__mockState
              };
            };
            class MockEventSource {
              constructor(url) {
                this.url = url;
                this.listeners = {};
                window.__eventSource = this;
                queueMicrotask(() => {
                  if (this.onopen) this.onopen();
                  const listener = this.listeners.state;
                  if (listener && window.__mockState) listener({data: JSON.stringify(window.__mockState)});
                });
              }
              addEventListener(name, listener) { this.listeners[name] = listener; }
              close() { this.closed = true; }
              emit(state) { const listener = this.listeners.state; if (listener) listener({data: JSON.stringify(state)}); }
            }
            window.EventSource = MockEventSource;
            }
            """
        )
        page.evaluate("state => { window.__mockState = state; }", initial)
        page.set_content(HTML, wait_until="domcontentloaded")
        page.wait_for_function("document.querySelectorAll('.tile').length === 512")
        page.wait_for_function("document.querySelector('#config').textContent.includes('ENGINE WAITING')")

        assert page.locator(".tile").count() == 512
        assert page.locator("#reset").is_visible()
        assert not page.locator("#status").evaluate("element => element.classList.contains('live')")
        assert "0/50 FPS" in page.locator("#config").text_content()

        live = state_payload(engine=True, fade=0.5, pressed=True)
        page.evaluate("state => { window.__mockState = state; window.__eventSource.emit(state); }", live)
        page.wait_for_function("document.querySelector('.tile').classList.contains('pressed')")
        assert page.locator("#status").evaluate("element => element.classList.contains('live')")
        first_background = page.locator(".tile").first.evaluate("element => getComputedStyle(element).backgroundColor")
        assert first_background == "rgb(50, 0, 0)", first_background

        page.get_by_role("button", name="Steps").click()
        assert "stats" in (page.locator("#board").get_attribute("class") or "")
        assert page.locator(".tile .label").first.text_content() == "1"
        page.get_by_role("button", name="Dwell").click()
        assert page.locator(".tile .label").first.text_content() == "2.5s"
        page.get_by_role("button", name="Channels").click()
        assert page.locator(".tile .label").first.text_content().startswith("CH")
        page.get_by_role("button", name="Live").click()

        first_tile = page.locator(".tile").first
        box = first_tile.bounding_box()
        assert box is not None
        page.mouse.move(box["x"] + box["width"] / 2, box["y"] + box["height"] / 2)
        page.mouse.down()
        page.wait_for_timeout(650)
        page.mouse.up()
        requests = page.evaluate("window.__requests")
        press_requests = [request for request in requests if request["url"] == "/press"]
        true_requests = [request for request in press_requests if '"pressed":true' in request["body"]]
        assert len(true_requests) >= 2, press_requests  # initial press + lease renewal
        assert any('"pressed":false' in request["body"] for request in press_requests)

        page.get_by_role("button", name="15m").click()
        page.wait_for_function("window.__eventSource.url.includes('window=15')")
        browser.close()

    print("Embedded diagnostics browser smoke test passed.")
    return 0


if __name__ == "__main__":
    raise SystemExit(main())
