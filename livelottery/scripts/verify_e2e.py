#!/usr/bin/env python3
# 端到端验证：启动真实服务，经 HTTP 跑完整链路并严格断言三大不变量：
#   1) 并发确认不会超扣库存；2) 正式名单唯一；3) 重复回执不会重复发货。
# 另覆盖：规则锁定不可改、重复报名、退货复核、超期弃奖、名单哈希。
import json
import os
import subprocess
import sys
import time
import urllib.request
import urllib.error
from concurrent.futures import ThreadPoolExecutor

BASE = os.environ.get("BASE_URL", "http://127.0.0.1:18099")
BINARY = os.environ.get("SERVER_BIN", "./bin/server")
DB = os.environ.get("E2E_DB", "/tmp/lottery_e2e.db")

failures = []


def check(name, cond, detail=""):
    status = "PASS" if cond else "FAIL"
    print(f"[{status}] {name}" + (f"  -- {detail}" if detail and not cond else ""))
    if not cond:
        failures.append(name)


def call(method, path, body=None, timeout=15):
    url = BASE + path
    data = json.dumps(body).encode() if body is not None else None
    req = urllib.request.Request(url, data=data, method=method,
                                 headers={"Content-Type": "application/json"})
    try:
        with urllib.request.urlopen(req, timeout=timeout) as r:
            txt = r.read().decode()
            return r.status, (json.loads(txt) if txt else {})
    except urllib.error.HTTPError as e:
        txt = e.read().decode()
        try:
            return e.code, json.loads(txt)
        except Exception:
            return e.code, {"error": txt}


def data_of(resp):
    return resp[1].get("data")


def wait_health(proc):
    for _ in range(50):
        if proc.poll() is not None:
            raise RuntimeError("server exited early")
        try:
            code, _ = call("GET", "/api/health", timeout=2)
            if code == 200:
                return
        except Exception:
            pass
        time.sleep(0.1)
    raise RuntimeError("server not healthy")


def main():
    for p in (DB, DB + "-wal", DB + "-shm"):
        try:
            os.remove(p)
        except FileNotFoundError:
            pass

    env = dict(os.environ, ADDR=":18099", DB_PATH=DB)
    proc = subprocess.Popen([BINARY], env=env,
                            stdout=subprocess.DEVNULL, stderr=subprocess.DEVNULL)
    try:
        wait_health(proc)
        run_scenarios()
    finally:
        proc.terminate()
        try:
            proc.wait(timeout=5)
        except Exception:
            proc.kill()

    print()
    if failures:
        print(f"❌ {len(failures)} 项失败: {failures}")
        sys.exit(1)
    print("✅ 全部端到端断言通过")


def run_scenarios():
    future_iso = time.strftime("%Y-%m-%dT%H:%M", time.gmtime(time.time() + 3600))

    # ---- 建活动 / 奖品 / 锁定 ----
    _, r = call("POST", "/api/activities", {
        "name": "e2e-主活动", "draw_at": future_iso,
        "min_account_age_days": 0, "need_bound_phone": False,
        "winner_count": 5, "address_deadline_hours": 24})
    act = data_of((200, r)); act_id = act["id"]

    code, _ = call("POST", f"/api/activities/{act_id}/prizes", {"name": "耳机", "stock": 5})
    check("草稿态可登记奖品库存", code == 200)

    code, _ = call("POST", f"/api/activities/{act_id}/lock")
    check("锁定规则与库存成功", code == 200)

    code, _ = call("POST", f"/api/activities/{act_id}/prizes", {"name": "X", "stock": 1})
    check("锁定后不能再增加库存/改规则", code == 409, f"got {code}")

    # ---- 报名：唯一占名额（含同账号并发）----
    for i in range(8):
        acc = f"viewer-{i+1:02d}"
        code, _ = call("POST", f"/api/activities/{act_id}/register",
                       {"account_id": acc, "order_no": "O-" + acc})
        check(f"报名 {acc}", code == 200, f"got {code}")

    def reg_dup(_):
        return call("POST", f"/api/activities/{act_id}/register",
                    {"account_id": "viewer-dup", "order_no": "OD"})[0]
    with ThreadPoolExecutor(12) as ex:
        codes = list(ex.map(reg_dup, range(12)))
    check("同一账号并发报名仅 1 次成功", codes.count(200) == 1, str(codes))

    # 重复普通报名
    code, _ = call("POST", f"/api/activities/{act_id}/register",
                   {"account_id": "viewer-01", "order_no": "O"})
    check("重复报名被拒(409)", code == 409, f"got {code}")

    # ---- 结束 + 并发开奖：唯一正式名单 ----
    code, _ = call("POST", f"/api/activities/{act_id}/end")
    check("结束直播", code == 200)

    def draw(_):
        return call("POST", f"/api/activities/{act_id}/draw")[0]
    with ThreadPoolExecutor(16) as ex:
        draw_codes = list(ex.map(draw, range(16)))
    check("并发开奖仅 1 次成功", draw_codes.count(200) == 1, str(draw_codes))

    code, _ = call("POST", f"/api/activities/{act_id}/draw")
    check("串行重复开奖被拒(409)", code == 409, f"got {code}")

    _, r = call("GET", f"/api/activities/{act_id}/result")
    result = data_of((200, r))
    winners = result["winners"]
    prize_id = winners[0]["prize_id"]
    check("正式名单人数=5", len(winners) == 5, str(len(winners)))

    code, r = call("POST", f"/api/activities/{act_id}/verify-hash")
    check("名单哈希校验通过", code == 200 and r["data"]["hash_matches"] is True)

    # ---- 并发确认：不超扣（每人重复提交 8 次）----
    def confirm(args):
        acc, _ = args
        return call("POST", f"/api/activities/{act_id}/winners/{acc}/confirm",
                    {"receiver": acc, "phone": "13800000000", "address": "addr-" + acc})[0]
    jobs = [(w["account_id"], k) for w in winners for k in range(8)]
    with ThreadPoolExecutor(16) as ex:
        conf_codes = list(ex.map(confirm, jobs))
    check("并发确认全部有响应(200/409/422)", all(c in (200, 409, 422) for c in conf_codes),
          str(sorted(set(conf_codes))))

    _, r = call("GET", f"/api/prizes/{prize_id}/items")
    items = data_of((200, r))
    claimed = [i for i in items if i["status"] == "claimed"]
    available = [i for i in items if i["status"] == "available"]
    check("并发确认后占用实物恰好 5（不超扣）", len(claimed) == 5, f"claimed={len(claimed)}")
    check("并发确认后在库为 0", len(available) == 0, f"avail={len(available)}")
    bound = [i["winner_record_id"] for i in claimed]
    check("每个奖品编号最多绑定一个中奖者", len(set(bound)) == len(bound) == 5)

    _, r = call("GET", f"/api/activities/{act_id}/result")
    confirmed = [w for w in data_of((200, r))["winners"] if w["status"] == "confirmed"]
    check("confirmed 中奖者恰好 5 且都有奖品编号",
          len(confirmed) == 5 and all(w["prize_item_id"] for w in confirmed),
          f"confirmed={len(confirmed)}")

    _, r = call("GET", f"/api/prizes/{prize_id}/ledger")
    ledger = data_of((200, r))
    debits = [l for l in ledger if l["change"] == -1 and l["reason"] == "claim.confirm"]
    check("扣减流水恰好 5 条 -1", len(debits) == 5, f"debits={len(debits)}")

    # ---- 发货 + 重复回执幂等 ----
    w1 = confirmed[0]
    code, _ = call("POST", f"/api/activities/{act_id}/ship",
                   {"winner_record_id": w1["id"], "tracking_no": "TRK-1"})
    check("按奖品编号发货成功", code == 200, f"got {code}")
    code, _ = call("POST", f"/api/activities/{act_id}/ship",
                   {"winner_record_id": w1["id"], "tracking_no": "TRK-2"})
    check("重复发货被拒", code >= 400, f"got {code}")

    def receipt(_):
        return call("POST", "/api/logistics/receipt",
                    {"receipt_no": "RCPT-DUP", "tracking_no": "TRK-1",
                     "event_type": "delivered"})[0]
    with ThreadPoolExecutor(12) as ex:
        rcpts = list(ex.map(receipt, range(12)))
    check("重复回执全部被接受或识别为重复", all(c == 200 for c in rcpts), str(rcpts))

    _, r = call("GET", f"/api/activities/{act_id}/shipments")
    ships = data_of((200, r))
    check("重复回执不产生第二张发货单", len(ships) == 1, f"ships={len(ships)}")
    check("发货单状态为 delivered", ships[0]["status"] == "delivered", ships[0]["status"])

    # ---- 退货复核：中奖者判失效不补抽，库存释放 ----
    w2 = next(w for w in confirmed if w["id"] != w1["id"])
    code, _ = call("POST", f"/api/activities/{act_id}/return",
                   {"account_id": w2["account_id"], "order_no": "O-" + w2["account_id"]})
    check("退货事件受理进入复核", code == 200)
    code, _ = call("POST", f"/api/activities/{act_id}/review",
                   {"account_id": w2["account_id"], "confirm": False, "note": "确认退货"})
    check("复核判失效成功", code == 200)

    _, r = call("GET", f"/api/activities/{act_id}/result")
    w2_after = next(w for w in data_of((200, r))["winners"] if w["id"] == w2["id"])
    check("退货中奖者变为 disqualified 且释放编号",
          w2_after["status"] == "disqualified" and not w2_after.get("prize_item_id"),
          w2_after["status"])
    code, _ = call("POST", f"/api/activities/{act_id}/draw")
    check("判失效后不能重抽（名单唯一不补抽）", code == 409, f"got {code}")
    _, r = call("GET", f"/api/activities/{act_id}/result")
    check("名单人数仍为 5（不补抽）", len(data_of((200, r))["winners"]) == 5)

    # ---- 超期未填地址：按锁定原规则弃奖 ----
    _, r = call("POST", "/api/activities", {
        "name": "e2e-超期", "draw_at": future_iso,
        "min_account_age_days": 0, "need_bound_phone": False,
        "winner_count": 2, "address_deadline_hours": 0.002})  # ~7.2 秒... 实际 7.2s
    act2 = data_of((200, r)); a2 = act2["id"]
    call("POST", f"/api/activities/{a2}/prizes", {"name": "奖", "stock": 2})
    call("POST", f"/api/activities/{a2}/lock")
    call("POST", f"/api/activities/{a2}/register", {"account_id": "ta"})
    call("POST", f"/api/activities/{a2}/register", {"account_id": "tb"})
    call("POST", f"/api/activities/{a2}/register", {"account_id": "tc"})
    call("POST", f"/api/activities/{a2}/end")
    call("POST", f"/api/activities/{a2}/draw")
    time.sleep(9)  # 超过 0.002h（7.2s）
    _, r2 = call("GET", f"/api/activities/{a2}/result")
    w2list = data_of((200, r2))["winners"]
    target = w2list[0]  # 取实际名单中的一名待确认中奖者
    code, _ = call("POST", f"/api/activities/{a2}/winners/{target['account_id']}/confirm",
                   {"receiver": "r", "phone": "p", "address": "a"})
    check("超期填地址返回 422 弃奖", code == 422, f"got {code}")
    _, r2 = call("GET", f"/api/activities/{a2}/result")
    t2 = next(w for w in data_of((200, r2))["winners"] if w["id"] == target["id"])
    check("超期者状态=forfeited 且未占库存",
          t2["status"] == "forfeited" and not t2.get("prize_item_id"), t2["status"])
    check("超期弃奖不补抽，名单仍 2 人", len(data_of((200, r2))["winners"]) == 2)

    # 批量处理超期
    code, r = call("POST", f"/api/activities/{a2}/expire")
    check("批量处理超期接口可用", code == 200 and r["data"]["expired"] >= 1, str(r))


if __name__ == "__main__":
    main()
