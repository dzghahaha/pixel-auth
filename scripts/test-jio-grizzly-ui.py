"""Local browser regression for the Grizzly UI; all API calls are intercepted.

Requires Python Playwright and Chrome. Run: python scripts/test-jio-grizzly-ui.py
Set JIO_UI_BROWSER to override the installed Chrome executable path.
"""
import functools
import json
import os
from pathlib import Path
import tempfile
import threading
from http.server import SimpleHTTPRequestHandler, ThreadingHTTPServer
from playwright.sync_api import sync_playwright, expect

ROOT = Path(__file__).resolve().parents[1]


class QuietHandler(SimpleHTTPRequestHandler):
    def log_message(self, *_):
        pass


def main():
    server = ThreadingHTTPServer(("127.0.0.1", 0), functools.partial(QuietHandler, directory=str(ROOT / "frontend")))
    threading.Thread(target=server.serve_forever, daemon=True).start()
    origin = f"http://127.0.0.1:{server.server_port}"
    state = {"starts": 0, "done": False, "enabled": False, "failed": False}
    token = "a" * 64
    link = "https://serviceactivation.google.com/subscription/new/" + "x" * 60
    errors = []

    def api(route):
        path = route.request.url.split(origin, 1)[-1].split("?", 1)[0]
        data = {"success": True}
        if path == "/api/jio/redeem":
            state["starts"] += 1
            assert route.request.post_data_json["async"] is True
            data["data"] = {"status": "processing", "task_id": token, "progress": 0}
        elif path == "/api/jio/redeem/status":
            assert route.request.post_data_json == {"task_id": token}
            data["data"] = {"status": "processing", "task_id": token, "progress": 94}
            if state["done"]:
                data["data"].update(status="failed" if state["failed"] else "success", progress=94 if state["failed"] else 100,
                                    result={"success": not state["failed"], "message": "模拟失败", "data": {"offer_url": link}})
        elif path == "/api/admin/check":
            data.update(role="admin", permissions=["jio_suppliers"])
        elif path == "/api/admin/jio/suppliers/enabled":
            state["enabled"] = route.request.post_data_json["enabled"]
        elif path == "/api/admin/jio/suppliers":
            data.update(active_provider="grizzly", dispatch_strategy="auto_lowest_cost",
                        current_effective_provider="grizzly" if state["enabled"] else "",
                        suppliers=[{"name": "grizzly", "display_name": "Grizzly", "enabled": state["enabled"],
                                    "config": {"api_key": "fixture-key"}, "has_key": True, "masked_key": "fix***", "support_balance": True}],
                        candidates=[{"name": "grizzly", "available": state["enabled"], "price_usd": .208}],
                        pricing_config={"exchange_rate": 7.2}, proxy={"enabled": "off"})
        elif path == "/api/admin/jio/suppliers/products":
            data["products"] = [{"id": "jio", "name": "Jio Gemini 自动获取链接", "price_usd": .208, "unlimited_stock": True}]
        elif path == "/api/admin/jio/suppliers/balance":
            data["balances"] = [{"provider_key": "grizzly", "balance": {"supported": True, "balance": 12, "currency": "USD"}}]
        route.fulfill(status=200, content_type="application/json", body=json.dumps(data))

    try:
        with sync_playwright() as p:
            executable = os.environ.get("JIO_UI_BROWSER", r"C:\Program Files\Google\Chrome\Application\chrome.exe")
            browser = p.chromium.launch(executable_path=executable, headless=True)
            context = browser.new_context(viewport={"width": 390, "height": 844}, reduced_motion="reduce")
            context.route("**/api/**", api)
            page = context.new_page()
            page.on("pageerror", lambda error: errors.append(str(error)))
            page.goto(origin + "/jio.html")
            page.locator("#jio-card-secret").fill("FIXTURE-CARD")
            page.locator("#btn-jio-redeem").click()
            expect(page.locator("#jio-progress-value")).to_have_text("94%")
            page.locator("#btn-jio-redeem").dispatch_event("click")
            assert state["starts"] == 1
            assert page.locator("#jio-progress").inner_text().strip() == "94%"
            assert page.evaluate("document.documentElement.scrollWidth <= innerWidth")
            page.screenshot(path=str(Path(tempfile.gettempdir()) / "jio-grizzly-progress-mobile.png"), full_page=True)
            page.reload()
            expect(page.locator("#jio-progress-value")).to_have_text("94%")
            assert state["starts"] == 1, "refresh started another purchase"
            state["done"] = True
            expect(page.locator("#jio-progress-value")).to_have_text("100%")
            expect(page.locator("#jio-result-url")).to_have_value(link)
            assert page.evaluate("sessionStorage.getItem('jio-redemption-task')") is None

            state.update(done=True, failed=True)
            page.locator("#jio-card-secret").fill("FAILURE-CARD")
            page.locator("#btn-jio-redeem").click()
            expect(page.locator("#jio-progress")).to_have_class("jio-progress failed")
            expect(page.locator("#jio-progress-value")).to_have_text("94%")
            expect(page.locator("#btn-jio-redeem")).to_be_enabled()

            page.set_viewport_size({"width": 1440, "height": 1000})
            page.goto(origin + "/admin/jio_suppliers.html")
            card = page.locator("#card-supplier-grizzly")
            expect(card.locator("#product-stock-grizzly")).to_have_text("库存: 无限")
            expect(card.get_by_role("switch")).to_have_attribute("aria-checked", "false")
            expect(card.get_by_role("button", name="在线试买测试")).to_be_disabled()
            card.get_by_role("switch").click()
            expect(card.get_by_role("switch")).to_have_attribute("aria-checked", "true")
            expect(card.get_by_role("button", name="在线试买测试")).to_be_enabled()
            card.get_by_role("button", name="供应商参数").click()
            expect(page.locator("#cfg-grizzly-key")).to_have_value("fixture-key")
            expect(page.locator("#cfg-grizzly-price")).to_have_value("0.208")
            expect(page.locator("#cfg-grizzly-timeout")).to_have_value("300")
            page.screenshot(path=str(Path(tempfile.gettempdir()) / "jio-grizzly-admin.png"), full_page=True, animations="disabled")
            assert not errors, errors
            browser.close()
    finally:
        server.shutdown()
        server.server_close()
    print("PASS: progress, refresh resume, duplicate clicks, success/failure, supplier toggle, unlimited inventory, Grizzly config; no page errors")


if __name__ == "__main__":
    main()
