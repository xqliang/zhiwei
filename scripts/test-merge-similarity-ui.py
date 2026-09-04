"""
声纹页「手动合并」两阶段二次确认的端到端验证（Playwright）。

为什么需要这个脚本：本项目前端零测试基建，合并流程的状态机错误（尤其是「预检失败
必须放行」这条）不会被任何自动化手段发现，只能靠浏览器点。本脚本把人工点击固化成
可重复执行的回归。

用法：
  1. 起一个指向测试库的 server（切勿指向共享 dev 库，会真合并掉别人的说话人）：
       ZW_PORT=8081 ZW_MYSQL_DSN="zhiwei:zhiwei@tcp(127.0.0.1:3307)/<你的库>?parseTime=true&charset=utf8mb4" \
       bash scripts/dev.sh start
  2. pip install playwright（浏览器用 ~/Library/Caches/ms-playwright 里已有的即可）
  3. python3 scripts/test-merge-similarity-ui.py [BASE_URL]   默认 http://localhost:8081

环境变量：MERGE_TEST_USER / MERGE_TEST_PASS（默认 owner / simcheck123）

注意：脚本会真的执行合并（那是被测行为），务必指向可牺牲的测试库。
"""
import sys
from playwright.sync_api import sync_playwright

import os
BASE = os.environ.get("MERGE_TEST_BASE", "http://localhost:8081")
TEST_USER = os.environ.get("MERGE_TEST_USER", "owner")
TEST_PASS = os.environ.get("MERGE_TEST_PASS", "simcheck123")
results = []


def check(name, ok, detail=""):
    results.append((name, ok, detail))
    print(f"{'PASS' if ok else 'FAIL'}  {name}" + (f"  — {detail}" if detail else ""), flush=True)


with sync_playwright() as p:
    browser = p.chromium.launch(headless=True)
    page = browser.new_page()
    errors = []
    page.on("console", lambda m: errors.append(m.text) if m.type == "error" else None)
    page.on("pageerror", lambda e: errors.append(str(e)))

    # ---- 登录 ----
    page.goto(BASE + "/", wait_until="networkidle")
    page.fill('#login-user', TEST_USER)
    page.fill('#login-pass', TEST_PASS)
    page.click('button:has-text("登录")')
    page.wait_for_selector("text=声纹", timeout=15000)  # 登录后主导航渲染即算成功
    check("登录成功", True, "主导航已渲染")

    # ---- 切到声纹 tab ----
    page.click('text=声纹')
    page.wait_for_timeout(800)
    check("声纹 tab 可见", page.locator('text=手动合并').count() > 0)

    # ---- 进入合并选择模式 ----
    page.click('text=手动合并')
    page.wait_for_timeout(400)
    boxes = page.locator('input[type=checkbox]')
    n = boxes.count()
    check("出现勾选框", n >= 3, f"共 {n} 个")
    if n < 3:
        sys.exit(1)

    boxes.nth(0).check()
    boxes.nth(1).check()
    boxes.nth(2).check()
    page.wait_for_timeout(300)
    page.click('button:has-text("开始合并")')
    page.wait_for_timeout(400)
    check("进入选目标阶段", page.locator('text=保留目标').count() > 0)

    # ---- 阶段②：第一次点「确认合并」→ 应出相似度表、按钮变文案 ----
    sim_calls = {"n": 0}
    page.on("request", lambda r: sim_calls.__setitem__("n", sim_calls["n"] + 1)
            if "/api/speakers/similarities" in r.url else None)

    page.click('button:has-text("确认合并")')
    page.wait_for_timeout(1500)

    tbl = page.locator('text=两两相似度')
    check("② 相似度表出现", tbl.count() > 0)
    check("② 按钮变「⚠ 仍然合并」", page.locator('button:has-text("仍然合并")').count() > 0)
    check("② 相似度端点被调用一次", sim_calls["n"] == 1, f"实际 {sim_calls['n']} 次")

    rows = page.locator('text=/×/').count()
    badges = page.locator('.badge:has-text("不像")').count()
    check("② 表格有 3 对", rows >= 3, f"{rows} 行含 ×")
    check("② 低分标红+「⚠ 不像」徽标", badges >= 1, f"{badges} 个警示徽标")

    # ---- 不变量：改目标下拉 → 不重拉 ----
    before = sim_calls["n"]
    page.locator('select.mini').last.select_option(index=1)
    page.wait_for_timeout(1200)
    after = sim_calls["n"]
    check("③ 改目标不重拉相似度", after == before, f"{before} → {after} 次")

    # ---- 阶段③：第二次点 → 真正合并 ----
    page.click('button:has-text("仍然合并")')
    page.wait_for_timeout(2000)
    ok_toast = page.locator('text=已合并').count() > 0
    check("③ 第二次点击才真正合并", ok_toast)

    print("\n--- 前端 console/page 错误 ---")
    real = [e for e in errors if "favicon" not in e.lower() and "401" not in e]
    check("无 JS 运行时错误", len(real) == 0, "; ".join(real[:3]))

    # ================= 失败放行回归（关键） =================
    print("\n=== 关键回归：预检接口 500 时必须放行 ===")
    page.goto(BASE + "/", wait_until="networkidle")
    page.click('text=声纹')
    page.wait_for_timeout(800)
    page.click('text=手动合并')
    page.wait_for_timeout(400)
    b2 = page.locator('input[type=checkbox]')
    b2.nth(0).check()
    b2.nth(1).check()
    page.wait_for_timeout(300)
    page.click('button:has-text("开始合并")')
    page.wait_for_timeout(400)

    # 强制 similarities 端点 500
    page.route("**/api/speakers/similarities", lambda route: route.fulfill(
        status=500, content_type="application/json", body='{"error":"boom"}'))

    page.click('button:has-text("确认合并")')
    page.wait_for_timeout(1500)
    check("⑦ 500 时 toast 提示", page.locator('text=相似度预检失败').count() > 0)
    check("⑦ 500 时不出相似度表", page.locator('text=两两相似度').count() == 0)

    # 第二次点击必须能完成合并（这是 Loaded→Checked 语义修复的关键）。
    # toast 仅显示 2s，故等 merge 响应返回后立即读，避免和 toast 过期赛跑。
    with page.expect_response(lambda r: "/api/speakers/merge" in r.url, timeout=10000) as resp_info:
        page.click('button:has-text("仍然合并")')
    merge_resp = resp_info.value
    check("⑦ 500 后再点一次仍能完成合并", merge_resp.status == 200,
          f"merge → HTTP {merge_resp.status}")
    page.wait_for_timeout(600)
    check("⑦ 合并后有成功 toast", page.locator('text=已合并').count() > 0)

    browser.close()

print("\n================ 汇总 ================")
bad = [r for r in results if not r[1]]
for name, ok, detail in results:
    print(f"  {'✓' if ok else '✗'} {name}")
print(f"\n{len(results) - len(bad)}/{len(results)} 通过")
sys.exit(1 if bad else 0)
