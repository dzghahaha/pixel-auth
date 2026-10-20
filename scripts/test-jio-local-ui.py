"""Local supplier UI regression; APIs are fixtures, no paid services are used."""
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
    state = {"price": .25, "links": [], "enabled": True, "fail": False}
    errors = []

    def api(route):
        path = route.request.url.split(origin, 1)[-1].split("?", 1)[0]
        data = {"success": True}
        if path == "/api/admin/check":
            data.update(role="admin", permissions=["jio_suppliers"])
        elif path == "/api/admin/jio/suppliers":
            data.update(active_provider="local", dispatch_strategy="specific", current_effective_provider="local",
                        suppliers=[{"name": "local", "display_name": "Jio（自有）", "enabled": state["enabled"],
                                    "config": {"price_usd": state["price"]}, "has_key": False}],
                        candidates=[{"name": "local", "available": True, "price_usd": state["price"]}],
                        pricing_config={"exchange_rate": 7.2}, proxy={"enabled": "off"})
        elif path == "/api/admin/jio/suppliers/products":
            data["products"] = [{"id": "jio", "name": "Jio 链接", "price_usd": state["price"], "stock": len(state["links"]), "is_active": True}]
        elif path == "/api/admin/jio/suppliers/balance":
            data["balances"] = []
        elif path == "/api/admin/jio/suppliers/save_config":
            state["price"] = route.request.post_data_json["config"]["price_usd"]
        elif path == "/api/admin/jio/suppliers/local/links":
            if state["fail"]:
                route.fulfill(status=500, content_type="application/json", body=json.dumps({"success": False, "message": "测试读取失败"}))
                return
            if route.request.method == "POST":
                values = [x.strip() for x in route.request.post_data_json["links"].splitlines() if x.strip()]
                if any(not x.startswith("https://") for x in values):
                    route.fulfill(status=400, content_type="application/json", body=json.dumps({"success": False, "message": "第 2 行不是有效链接"}))
                    return
                old = len(state["links"])
                state["links"] = list(dict.fromkeys(state["links"] + values))
                data.update(added=len(state["links"]) - old, duplicates=len(values) - (len(state["links"]) - old))
            else:
                data.update(available=len(state["links"]), issued=0, total=len(state["links"]), page=1, pages=1,
                            items=[{"id": i+1, "link": link, "status": "available", "created_at": "2026-10-20 12:00:00"} for i, link in enumerate(state["links"])])
        route.fulfill(status=200, content_type="application/json", body=json.dumps(data))

    try:
        with sync_playwright() as p:
            browser = p.chromium.launch(executable_path=os.environ.get("JIO_UI_BROWSER", r"C:\Program Files\Google\Chrome\Application\chrome.exe"), headless=True)
            context = browser.new_context(viewport={"width": 1440, "height": 1000})
            context.route("**/api/**", api)
            page = context.new_page()
            page.on("pageerror", lambda error: errors.append(str(error)))
            page.on("dialog", lambda dialog: dialog.accept())
            page.goto(origin + "/admin/jio_suppliers.html")
            expect(page.get_by_text("平台自有，无需密钥", exact=False)).to_be_visible()
            page.get_by_role("button", name="供应商参数", exact=True).click()
            page.locator("#cfg-local-price").fill("0.1234")
            page.locator("#btn-modal-save-config").click()
            expect(page.locator("#modal-config")).not_to_have_class("modal-backdrop active")
            assert state["price"] == .1234
            page.get_by_role("button", name="管理链接库存").click()
            expect(page.locator("#local-jio-rows")).to_contain_text("暂无符合条件")
            page.locator("#local-jio-input").fill("https://example.com/a\nbad")
            page.locator("#local-jio-import").click()
            expect(page.locator("#local-jio-feedback")).to_contain_text("第 2 行")
            expect(page.locator("#local-jio-input")).to_have_value("https://example.com/a\nbad")
            link = "https://example.com/a?value=<img%20src=x%20onerror=alert(1)>"
            page.locator("#local-jio-input").fill(link + "\n" + link + "\nhttps://example.com/b")
            page.locator("#local-jio-import").click()
            expect(page.locator("#local-jio-feedback")).to_contain_text("已新增 2 条，跳过 1 条")
            expect(page.locator("#local-jio-summary")).to_have_text("未使用 2 条 · 已发放 0 条")
            assert page.locator("#local-jio-rows img").count() == 0
            expect(page.locator("#local-jio-input")).to_have_value("")
            page.screenshot(path=str(Path(tempfile.gettempdir()) / "jio-local-desktop.png"), full_page=True)
            state["fail"] = True
            page.locator("#local-jio-refresh").click()
            expect(page.locator("#local-jio-summary")).to_contain_text("读取失败")
            state["fail"] = False
            page.locator("#local-jio-refresh").click()
            expect(page.locator("#local-jio-summary")).to_contain_text("未使用 2 条")
            page.set_viewport_size({"width": 390, "height": 844})
            page.locator("#local-jio-title").scroll_into_view_if_needed()
            assert page.evaluate("document.documentElement.scrollWidth <= innerWidth"), "horizontal page overflow"
            page.screenshot(path=str(Path(tempfile.gettempdir()) / "jio-local-mobile.png"), full_page=True)
            assert not errors, errors
            browser.close()
    finally:
        server.shutdown()
    print("Local Jio UI passed: price, batch import, duplicate feedback, error recovery, escaped links, desktop/mobile.")


if __name__ == "__main__":
    main()
