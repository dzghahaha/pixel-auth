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
    state = {"price": .25, "links": [], "enabled": True, "fail": False, "role": "admin"}
    errors = []

    def api(route):
        path = route.request.url.split(origin, 1)[-1].split("?", 1)[0]
        data = {"success": True}
        if path == "/api/admin/check":
            data.update(role=state["role"], permissions=["jio_suppliers"])
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
                raw = route.request.post_data_json["links"]
                values = [x.strip() for x in (raw if isinstance(raw, list) else raw.splitlines()) if x.strip()]
                if any(not x.startswith("https://") for x in values):
                    route.fulfill(status=400, content_type="application/json", body=json.dumps({"success": False, "message": "第 2 行不是有效链接"}))
                    return
                old = len(state["links"])
                state["links"] = list(dict.fromkeys(state["links"] + values))
                data.update(added=len(state["links"]) - old, duplicate=len(values) - (len(state["links"]) - old), total=len(state["links"]))
            else:
                data.update(available=len(state["links"]), issued=0, total=len(state["links"]), page=1, total_pages=1, filtered_total=len(state["links"]),
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

            # 1. 验证 Jio 供应商页面中不再包含库存链接管理入口
            page.goto(origin + "/admin/jio_suppliers.html")
            expect(page.get_by_text("平台自有，无需密钥", exact=False)).to_be_visible()
            expect(page.get_by_role("button", name="管理链接库存")).to_have_count(0)
            page.get_by_role("button", name="供应商参数", exact=True).click()
            page.locator("#cfg-local-price").fill("0.1234")
            page.locator("#btn-modal-save-config").click()
            expect(page.locator("#modal-config")).not_to_have_class("modal-backdrop active")
            assert state["price"] == .1234

            # 2. 验证导航栏中存在独立的 Jio 库存链接菜单
            link_menu = page.locator('a.nav-item[href="/admin/jio_links.html"]')
            expect(link_menu).to_be_visible()

            # 3. 访问专属页面并验证管理功能
            page.goto(origin + "/admin/jio_links.html")
            expect(page.locator("#page-title")).to_contain_text("Jio 库存链接管理")
            expect(page.locator("#links-table-body")).to_contain_text("暂无符合条件")

            # 非法链接校验与反馈
            page.locator("#import-links-input").fill("https://example.com/a\nbad")
            page.locator("#btn-submit-links").click()
            expect(page.locator("#import-feedback-box")).to_contain_text("第 2 行不是有效链接")
            expect(page.locator("#import-links-input")).to_have_value("https://example.com/a\nbad")

            # 批量录入、去重与 XSS 转义验证
            link = "https://example.com/a?value=<img%20src=x%20onerror=alert(1)>"
            page.locator("#import-links-input").fill(link + "\n" + link + "\nhttps://example.com/b")
            page.locator("#btn-submit-links").click()
            expect(page.locator("#import-feedback-box")).to_contain_text("成功录入 2 条新链接，跳过重复 1 条")
            expect(page.locator("#stat-available")).to_have_text("2")
            assert page.locator("#links-table-body img").count() == 0
            expect(page.locator("#import-links-input")).to_have_value("")
            page.screenshot(path=str(Path(tempfile.gettempdir()) / "jio-links-desktop.png"), full_page=True)

            # 错误恢复与刷新
            state["fail"] = True
            page.locator("#btn-refresh-all").click()
            expect(page.locator("#links-table-body")).to_contain_text("测试读取失败")
            state["fail"] = False
            page.locator("#btn-refresh-all").click()
            expect(page.locator("#stat-available")).to_have_text("2")

            # 移动端响应式与无横向溢出
            page.set_viewport_size({"width": 390, "height": 844})
            page.locator("#page-title").scroll_into_view_if_needed()
            assert page.evaluate("document.documentElement.scrollWidth <= innerWidth"), "horizontal page overflow"
            page.screenshot(path=str(Path(tempfile.gettempdir()) / "jio-links-mobile.png"), full_page=True)

            # 4. 权限隔离验证：普通操作员 (role='user') 无法访问此菜单和页面
            state["role"] = "user"
            page.goto(origin + "/admin/dashboard.html")
            expect(page.locator('a.nav-item[href="/admin/jio_links.html"]')).to_be_hidden()
            page.goto(origin + "/admin/jio_links.html")
            # 重定向至 dashboard
            page.wait_for_url("**/dashboard.html")

            assert not errors, errors
            browser.close()
    finally:
        server.shutdown()
    print("Local Jio UI passed: supplier menu removed, dedicated page created, admin-only access, batch import, duplicate feedback, error recovery, escaped links, desktop/mobile.")


if __name__ == "__main__":
    main()
