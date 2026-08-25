#!/usr/bin/env python3
"""突變測試：故意把生產程式碼改壞一處，確認對應的測試會轉紅。

測試全綠不代表測試有用。這支工具檢查的是「這支測試真的在守它宣稱在守的東西嗎」——
把程式碼改成「錯的那個樣子」，測試應該轉紅；如果仍是綠的，那條理由現在只是一段散文。

用法：
    python3 mutate.py mutations.json

一律跑完全部案例，再依結果決定離開碼：**只有全部都「如預期轉紅」才回 0**，任何一格
仍綠／對照組也轉紅／無效證據／baseline 不可信／未跑都回非零。沒有旗標可以壓掉這個
判定——那會讓自動化拿到一個假的通過。空的突變定義也回非零：沒有證據就不是通過。

mutations.json 是一個陣列，每個元素是一格突變：

    [
      {
        "label": "EmitEvent 拿掉 Text 去敏",
        "package": "./internal/core/",
        "test": "TestProcessEventTextRedacted",
        "edits": [
          {
            "file": "internal/core/event.go",
            "old": "\\te.Text = RedactErrorText(e.Text)\\n",
            "new": ""
          }
        ],
        "control": "TestSomeOtherThing"
      }
    ]

欄位：
    label    這一格在測什麼（會印出來，也會進工作記錄）
    package  go test 的目標，例如 "./internal/core/"
    test     期望**轉紅**的測試名（-run 的參數，支援 regexp）
    edits    要套用的改動；每個 old 必須在該檔中恰好出現一次，否則整格跳過
    control  選填。期望**仍綠**的對照組測試名——用來證明「只有新測試守得到這條」

安全性：
    - 每格跑完無條件還原，例外與 Ctrl-C 也還原（finally）
    - old 命中不唯一時整格跳過，不猜位置
    - 全部跑完後執行 go build ./... 確認沒有殘留
"""

import argparse
import json
import subprocess
import sys
from pathlib import Path
from typing import NamedTuple


class TestOutcome(NamedTuple):
    """一次 `go test` 的結果。

    **「有沒有可信的證據」與「測試是綠是紅」必須分開。** 只看 process 的離開碼會產生
    三種假證據，三種都實測確認過：

      - 突變造成**編譯失敗**：離開碼非零，但測試一支都沒執行 → 被誤判成「轉紅」
      - 測試名**拼錯或沒匹配到**：離開碼 0（package 照樣 pass）→ 被誤判成「仍綠」
      - 測試**通過但 package 另外失敗**（`TestMain` 收尾、cleanup、leak detector）：
        事件流是 `run → pass(Test) → fail(package)`、離開碼 1 → 只看離開碼會誤判成
        「轉紅」，只看 Test 事件會誤判成「仍綠」

    第三種最陰險：一個同時弄壞 target 斷言與 package 收尾的突變，會被報成
    「target 轉紅、control 仍綠」——一份完全編造出來的證據，而且離開碼是 0。
    """

    valid: bool  # 這次執行有沒有產生可信的證據
    passed: bool  # valid 為真時：測試是綠是紅。valid 為假時無意義
    detail: str  # valid 為假時說明原因


def parse_test_events(stdout: str, stderr: str, returncode: int) -> TestOutcome:
    """把 `go test -json` 的輸出解析成 TestOutcome。

    抽成純函式是為了可測：編譯失敗、零匹配、package 收尾失敗都不好在單元測試裡重現，
    但它們的事件流可以直接餵進來。三種的事件流都取自對真實 `go test` 的實測。
    """
    ran = False
    test_failed = False
    package_failed = False
    build_output: list[str] = []

    for line in stdout.splitlines():
        try:
            event = json.loads(line)
        except json.JSONDecodeError:
            continue  # go test 偶爾夾雜非 JSON 行，略過
        action = event.get("Action")
        if not event.get("Test"):
            if action in ("fail", "build-fail"):
                package_failed = True
            if action in ("build-output", "output") and event.get("Output"):
                build_output.append(event["Output"])
            continue
        if action == "run":
            ran = True
        elif action == "fail":
            test_failed = True

    if not ran:
        # 沒有任何測試執行過。分辨是編譯壞了還是名字沒匹配到——兩者的處置不同：
        # 前者是突變寫得太粗（改壞了語法），後者是 spec 的測試名寫錯。
        noise = "".join(build_output) + stderr
        if "build failed" in noise or "cannot use" in noise or "undefined:" in noise:
            # 取第一行**實際的**錯誤：go 的建置輸出開頭是 `# <package>` 標頭，印它出來
            # 只會告訴你「這個 package 壞了」，而你已經知道了。要的是壞在哪一行。
            first = next(
                (l.strip() for l in noise.splitlines() if l.strip() and not l.startswith("#")),
                "",
            )
            return TestOutcome(False, False, f"編譯失敗（{first[:120]}）")
        return TestOutcome(False, False, "沒有測試匹配這個名字")

    # 測試自己失敗 → 紅，這是有效證據。此時 package 當然也 fail、離開碼也非零，
    # 那是必然的後果、不是「另外失敗」，所以這一項要排在下面的檢查之前。
    if test_failed:
        return TestOutcome(True, False, "")

    # 測試全過，卻還是有東西壞了。綠色結果**必須**同時滿足 package 乾淨收尾與離開碼 0，
    # 否則這次執行說明不了任何事：測試是綠的，但整包是紅的。
    if package_failed or returncode != 0:
        return TestOutcome(
            False, False, "測試通過但 package 另外失敗（TestMain 收尾、cleanup 或 leak detector？）"
        )
    return TestOutcome(True, True, "")


def run_go_test(package: str, test: str) -> TestOutcome:
    """跑一支測試。-count=1 避開快取，-json 讓我們看得到「測試有沒有真的執行」。

    離開碼也傳給 parser：`-json` 的事件流與 process 的結果是兩個獨立來源，
    綠色結果要求兩邊都乾淨。
    """
    r = subprocess.run(
        ["go", "test", package, "-count=1", "-run", test, "-json"],
        capture_output=True,
        text=True,
    )
    return parse_test_events(r.stdout, r.stderr, r.returncode)


# 每格突變的結果，以及它算不算通過。
OUTCOMES = {
    "ok": True,  # target 如期轉紅，control（若有）如期仍綠
    "weak": False,  # target 仍綠——這支測試沒有在守這條性質
    "control_red": False,  # target 轉紅，但 control 也轉紅——「只有前者守得到」不成立
    "no_evidence": False,  # 突變後這次執行說明不了任何事——編譯壞了、沒匹配到、或 package 另外失敗
    "baseline": False,  # 突變前就不是綠的、或測試名沒匹配到，整格拒絕執行
    "miss": False,  # old 命中不唯一，整格沒跑
}


def apply_case(case: dict, runner=run_go_test) -> str:
    """套用一格突變、跑測試、還原。回傳 OUTCOMES 的其中一個 key。

    runner 可注入，供本檔的測試替換掉真正的 go test。
    """
    label = case["label"]
    edits = case["edits"]
    originals: dict[Path, str] = {}
    working: dict[Path, str] = {}

    # 先在記憶體裡把每一處都套好、驗好，全部通過才寫入磁碟——避免改了一半才發現
    # 第二處對不上。
    #
    # **edits 累積套用在 working 上，不是各自從 originals 出發。** 同一個檔案有多處
    # 改動時，後者從原始內容出發會把前者蓋掉，最後只有最後一處生效——而測試照樣會
    # 轉紅（原因卻是別的），於是這一格看起來通過了，實際上什麼都沒驗到。
    for edit in edits:
        path = Path(edit["file"])
        if path not in originals:
            originals[path] = path.read_text(encoding="utf-8")
            working[path] = originals[path]
        hits = working[path].count(edit["old"])
        if hits != 1:
            print(f"  跳過 {label}：old 在 {path} 命中 {hits} 次（必須恰好 1 次）")
            return "miss"
        working[path] = working[path].replace(edit["old"], edit["new"], 1)

    # **突變前先確認 baseline：測試存在、而且是綠的。**
    #
    # 一支本來就紅的測試，突變後仍紅會被判成「轉紅 ✓」——假陽性，而且發生在最不該
    # 發生的時候：開發中途、測試還沒全綠時。而一個**拼錯的測試名**（尤其是 control）
    # 在這裡就會被抓到，不必等到突變之後才發現整格白跑。
    for name in (case["test"], case.get("control")):
        if not name:
            continue
        outcome = runner(case["package"], name)
        if not outcome.valid:
            print(f"  拒絕 {label}：{name} 的 baseline 不可信（{outcome.detail}）")
            return "baseline"
        if not outcome.passed:
            print(f"  拒絕 {label}：{name} 在突變前就不是綠的，這一格的結果不可信")
            return "baseline"

    try:
        for path, content in working.items():
            path.write_text(content, encoding="utf-8")

        target = runner(case["package"], case["test"])
        control = runner(case["package"], case["control"]) if case.get("control") else None
    finally:
        # 無條件還原：例外、Ctrl-C、測試逾時都走這裡。
        for path, src in originals.items():
            path.write_text(src, encoding="utf-8")

    # 這次執行說明不了任何事 = 沒有證據。三種成因（編譯壞了、沒匹配到、測試過了但
    # package 另外失敗）都不得算成「轉紅」——只看離開碼的話三種全會被誤報成轉紅。
    if not target.valid:
        print(f"  {label}\n    → {case['test']} 無效證據 ✗（{target.detail}）")
        return "no_evidence"
    if control is not None and not control.valid:
        print(f"  {label}\n    → 對照 {case['control']} 無效證據 ✗（{control.detail}）")
        return "no_evidence"

    turned_red = not target.passed
    status = "轉紅 ✓" if turned_red else "仍綠 ✗（這支測試沒有守到這條）"
    note = ""
    if control is not None:
        note = (
            f"；對照 {case['control']} "
            + ("仍綠（只有前者守得到這條）" if control.passed else "也轉紅 ✗")
        )
    print(f"  {label}\n    → {case['test']} {status}{note}")

    if not turned_red:
        return "weak"
    # control 轉紅代表「只有 target 守得到這條」不成立——那正是加 control 要驗的宣稱，
    # 所以它必須計入失敗。只看 turned_red 的話，這種假陽性會安靜地通過。
    if control is not None and not control.passed:
        return "control_red"
    return "ok"


def exit_code(tally: dict[str, int]) -> int:
    """依結果統計決定離開碼：任何一格不是 ok 就回非零。

    抽成純函式是為了讓它可以被表格驅動測試——這是整支工具的**通過契約**，
    判錯的話自動化會拿到一個假的通過，而那正是突變測試要防的那種失效。
    """
    failed = sum(n for outcome, n in tally.items() if not OUTCOMES[outcome])
    return 1 if failed else 0


def verify_no_residue() -> bool:
    """還原之後 go build 應該全綠；不綠代表有突變殘留在工作樹裡。"""
    build = subprocess.run(["go", "build", "./..."], capture_output=True, text=True)
    if build.returncode != 0:
        print("\n還原後 go build 失敗——有突變殘留，立即檢查 git diff：")
        print(build.stderr)
        return False
    print("還原後 go build 全綠（無殘留）")
    return True


def main(argv=None, runner=run_go_test, residue_check=verify_no_residue) -> int:
    """跑完全部案例，再依結果決定離開碼。

    **沒有「忽略失敗」的旗標。** 曾經有一個 --keep-going，它唯一的作用就是把失敗的
    離開碼壓成 0——那與這支工具的用途直接矛盾：突變測試的價值就在於它會失敗。

    runner 與 residue_check 可注入，供本檔的測試替換掉真正的 go test／go build。
    """
    parser = argparse.ArgumentParser(description="突變測試：改壞程式碼，確認測試轉紅")
    parser.add_argument("spec", help="突變定義的 JSON 檔")
    args = parser.parse_args(argv)

    cases = json.loads(Path(args.spec).read_text(encoding="utf-8"))
    # 空的定義不是「零個失敗」，是**沒有證據**。生成 spec 的那一步失敗、或整份忘了填，
    # 都會長成這個樣子；回 0 等於背書一份不存在的驗證。
    if not isinstance(cases, list) or not cases:
        print("突變定義是空的或不是陣列——沒有證據就不算通過")
        return 1

    print(f"突變測試：{len(cases)} 格\n")

    tally = dict.fromkeys(OUTCOMES, 0)
    for case in cases:
        tally[apply_case(case, runner)] += 1

    print(
        f"\n結果：{tally['ok']} 格如預期"
        f"、{tally['weak']} 格仍綠"
        f"、{tally['control_red']} 格對照組也轉紅"
        f"、{tally['no_evidence']} 格無效證據"
        f"、{tally['baseline']} 格 baseline 不可信"
        f"、{tally['miss']} 格未跑"
    )

    if not residue_check():
        return 2
    return exit_code(tally)


if __name__ == "__main__":
    sys.exit(main())
