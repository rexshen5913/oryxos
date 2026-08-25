#!/usr/bin/env python3
"""mutate.py 的判定邏輯測試。

這支工具是自我審查機制的地基——它把「測試沒守到」判成通過的話，整套突變測試就成了
擺設。所以它自己的判定必須有測試守著。

不打真的 go test：注入一個假的 runner，依「檔案當前內容有沒有被改壞」回答綠或紅，
語義與真實情況一致，但確定、快、不依賴 repo 狀態。

跑法（改動 mutate.py 之後）：

    python3 .claude/skills/implement-ticket/scripts/test_mutate.py

刻意不進 `make test`：那是 Go 專案的測試指令，這是工具腳本自己的測試，兩者不同層。
"""

import sys

# 必須在 import mutate **之前**設定：否則 import 會在這個 skill 目錄裡留下
# __pycache__/mutate.*.pyc。那是生成物，不該混進 skill 的檔案清單裡
#（.gitignore 也擋著，這一行是讓它根本不要產生）。
sys.dont_write_bytecode = True

import json  # noqa: E402
import tempfile  # noqa: E402
import unittest  # noqa: E402
from pathlib import Path  # noqa: E402

from mutate import (  # noqa: E402
    OUTCOMES,
    TestOutcome,
    apply_case,
    exit_code,
    main,
    parse_test_events,
)

MARKER = "BROKEN"


def make_runner(path: Path, sensitive: set[str], *, valid_when_broken: bool = True):
    """假 runner：sensitive 裡的測試在檔案被改壞時回紅，其餘一律回綠。

    以檔案的**當前內容**判斷，所以它同時驗證了「baseline 在寫入前跑」與
    「還原後檔案乾淨」——順序錯了這些測試就會失敗。

    valid_when_broken=False 模擬「突變讓這次執行說明不了任何事」：檔案被改壞之後，
    package 編不過（或收尾失敗），拿不到可信的證據。
    """

    def runner(_package: str, test: str) -> TestOutcome:
        broken = MARKER in path.read_text(encoding="utf-8")
        if broken and not valid_when_broken:
            return TestOutcome(False, False, "編譯失敗（模擬）")
        return TestOutcome(True, not (test in sensitive and broken), "")

    return runner


def green(*_args) -> TestOutcome:
    """一律回綠的 runner。"""
    return TestOutcome(True, True, "")


class ApplyCaseTest(unittest.TestCase):
    def setUp(self):
        self.tmp = tempfile.TemporaryDirectory()
        self.addCleanup(self.tmp.cleanup)
        self.path = Path(self.tmp.name) / "src.go"
        self.path.write_text("alpha\nbravo\ncharlie\n", encoding="utf-8")
        self.original = self.path.read_text(encoding="utf-8")

    def case(self, **overrides) -> dict:
        case = {
            "label": "測試用突變",
            "package": "./pkg/",
            "test": "TestTarget",
            "edits": [{"file": str(self.path), "old": "bravo", "new": MARKER}],
        }
        case.update(overrides)
        return case

    def assertRestored(self):
        self.assertEqual(self.path.read_text(encoding="utf-8"), self.original)

    def test_target_轉紅_算通過(self):
        outcome = apply_case(self.case(), make_runner(self.path, {"TestTarget"}))
        self.assertEqual(outcome, "ok")
        self.assertRestored()

    def test_target_仍綠_算失敗(self):
        # runner 對任何測試都回綠：突變沒有被任何測試抓到。
        outcome = apply_case(self.case(), make_runner(self.path, set()))
        self.assertEqual(outcome, "weak")
        self.assertRestored()

    def test_control_仍綠_算通過(self):
        outcome = apply_case(
            self.case(control="TestControl"),
            make_runner(self.path, {"TestTarget"}),  # control 不敏感 → 仍綠
        )
        self.assertEqual(outcome, "ok")
        self.assertRestored()

    def test_control_也轉紅_算失敗(self):
        """Codex 第一輪抓到的那條：control 轉紅時不得判成通過。

        control 存在的意義就是宣稱「只有 target 守得到這條」。它跟著轉紅代表那個
        宣稱不成立——若只看 target 的結果，這種假陽性會安靜地通過。
        """
        outcome = apply_case(
            self.case(control="TestControl"),
            make_runner(self.path, {"TestTarget", "TestControl"}),
        )
        self.assertEqual(outcome, "control_red")
        self.assertRestored()

    def test_baseline_不綠_整格拒絕且不動檔案(self):
        # runner 一律回紅，模擬「這支測試突變前就是紅的」。
        def always_red(_package, _test):
            return TestOutcome(True, False, "")

        outcome = apply_case(self.case(), always_red)
        self.assertEqual(outcome, "baseline")
        self.assertRestored()

    def test_control_的_baseline_也要檢查(self):
        def red_only_for_control(_package, test):
            return TestOutcome(True, test != "TestControl", "")

        outcome = apply_case(self.case(control="TestControl"), red_only_for_control)
        self.assertEqual(outcome, "baseline")
        self.assertRestored()

    def test_old_命中不唯一_整格跳過(self):
        self.path.write_text("dup\ndup\n", encoding="utf-8")
        self.original = self.path.read_text(encoding="utf-8")
        case = self.case(edits=[{"file": str(self.path), "old": "dup", "new": MARKER}])

        outcome = apply_case(case, make_runner(self.path, {"TestTarget"}))
        self.assertEqual(outcome, "miss")
        self.assertRestored()

    def test_同檔多處_edit_累積套用(self):
        """守住早期版本的 bug：每個 edit 各自從原始內容出發，前者會被後者蓋掉。

        那時只有最後一處生效，測試卻照樣轉紅（原因是別的），這一格看起來通過了、
        實際上什麼都沒驗到——比沒測還糟。
        """
        seen: list[str] = []

        def recording_runner(_package, _test):
            seen.append(self.path.read_text(encoding="utf-8"))
            return TestOutcome(True, True, "")  # 一律回綠，只關心套用出來的內容

        case = self.case(
            edits=[
                {"file": str(self.path), "old": "alpha", "new": "ALPHA"},
                {"file": str(self.path), "old": "charlie", "new": "CHARLIE"},
            ]
        )
        apply_case(case, recording_runner)

        # seen[0] 是 baseline（寫入前，必須是原始內容）
        self.assertEqual(seen[0], self.original)
        # seen[1] 是突變後：**兩處都要生效**
        self.assertIn("ALPHA", seen[1])
        self.assertIn("CHARLIE", seen[1])
        self.assertRestored()

    def test_runner_拋例外時仍然還原(self):
        def boom(_package, test):
            if MARKER in self.path.read_text(encoding="utf-8"):
                raise RuntimeError("測試執行器炸了")
            return TestOutcome(True, True, "")  # baseline 放行，才進得到突變後的呼叫

        with self.assertRaises(RuntimeError):
            apply_case(self.case(), boom)
        self.assertRestored()

    def test_多檔_edit_全部還原(self):
        other = Path(self.tmp.name) / "other.go"
        other.write_text("delta\n", encoding="utf-8")
        case = self.case(
            edits=[
                {"file": str(self.path), "old": "bravo", "new": MARKER},
                {"file": str(other), "old": "delta", "new": "DELTA"},
            ]
        )

        apply_case(case, make_runner(self.path, {"TestTarget"}))

        self.assertRestored()
        self.assertEqual(other.read_text(encoding="utf-8"), "delta\n")


class NoEvidenceTest(unittest.TestCase):
    """突變後測試沒有執行 = 沒有證據，不得算成「轉紅」。

    最常見的成因是突變改壞了語法或型別，整個 package 編不過。那時 `go test` 的離開碼
    非零——只看它就會把「編譯壞了」誤報成「這支測試守得到」。
    """

    def setUp(self):
        self.tmp = tempfile.TemporaryDirectory()
        self.addCleanup(self.tmp.cleanup)
        self.path = Path(self.tmp.name) / "src.go"
        self.path.write_text("alpha\nbravo\n", encoding="utf-8")
        self.original = self.path.read_text(encoding="utf-8")

    def case(self, **overrides) -> dict:
        case = {
            "label": "改壞語法的突變",
            "package": "./pkg/",
            "test": "TestTarget",
            "edits": [{"file": str(self.path), "old": "bravo", "new": MARKER}],
        }
        case.update(overrides)
        return case

    def test_target_無效證據_算_no_evidence(self):
        outcome = apply_case(
            self.case(),
            make_runner(self.path, {"TestTarget"}, valid_when_broken=False),
        )
        self.assertEqual(outcome, "no_evidence")
        self.assertEqual(self.path.read_text(encoding="utf-8"), self.original)

    def test_control_無效證據_也算_no_evidence(self):
        # target 正常轉紅，但 control 因為同一個編譯失敗而沒執行——那一半的證據不成立。
        def runner(_package, test):
            broken = MARKER in self.path.read_text(encoding="utf-8")
            if not broken:
                return TestOutcome(True, True, "")
            if test == "TestTarget":
                return TestOutcome(True, False, "")
            return TestOutcome(False, False, "編譯失敗（模擬）")

        outcome = apply_case(self.case(control="TestControl"), runner)
        self.assertEqual(outcome, "no_evidence")

    def test_control_的_package_失敗_算_no_evidence(self):
        """整條鏈路的回歸：突變讓 target 斷言失敗、同時讓 package 收尾失敗。

        舊 parser 會把 control 判成綠 → 報出「target 轉紅、control 仍綠」並回 0。
        那是這支工具最不該產生的東西：一份憑空捏造的證據。
        """

        def runner(_package, test):
            broken = MARKER in self.path.read_text(encoding="utf-8")
            if not broken:
                return TestOutcome(True, True, "")
            if test == "TestTarget":
                return TestOutcome(True, False, "")  # 斷言真的失敗了
            # control 的測試通過，但 package 收尾失敗 → 說明不了任何事
            return TestOutcome(False, False, "測試通過但 package 另外失敗")

        outcome = apply_case(self.case(control="TestControl"), runner)
        self.assertEqual(outcome, "no_evidence")

    def test_baseline_零匹配_整格拒絕(self):
        """測試名拼錯：`go test` 回 0（package 照樣 pass），舊邏輯會誤判成「仍綠」。

        在 baseline 階段就抓到，不必等到突變之後才發現整格白跑。
        """

        def never_matches(_package, _test):
            return TestOutcome(False, False, "沒有測試匹配這個名字")

        outcome = apply_case(self.case(test="TestTypoInName"), never_matches)
        self.assertEqual(outcome, "baseline")
        self.assertEqual(self.path.read_text(encoding="utf-8"), self.original)


class ParseTestEventsTest(unittest.TestCase):
    """`go test -json` 的解析。事件流取自對真實 go test 的實測。"""

    @staticmethod
    def events(*objs) -> str:
        return "".join(json.dumps(o) + "\n" for o in objs)

    def test_測試通過(self):
        out = self.events(
            {"Action": "run", "Test": "TestFoo"},
            {"Action": "pass", "Test": "TestFoo"},
            {"Action": "pass"},
        )
        self.assertEqual(parse_test_events(out, "", 0), TestOutcome(True, True, ""))

    def test_測試失敗(self):
        out = self.events(
            {"Action": "run", "Test": "TestFoo"},
            {"Action": "fail", "Test": "TestFoo"},
            {"Action": "fail"},
        )
        # 測試自己失敗時 package 當然也 fail、離開碼也非零——那是必然的後果，
        # 不是「另外失敗」，仍然是有效證據。
        self.assertEqual(parse_test_events(out, "", 1), TestOutcome(True, False, ""))

    def test_多支測試_有一支失敗就算紅(self):
        out = self.events(
            {"Action": "run", "Test": "TestA"},
            {"Action": "pass", "Test": "TestA"},
            {"Action": "run", "Test": "TestB"},
            {"Action": "fail", "Test": "TestB"},
            {"Action": "fail"},
        )
        result = parse_test_events(out, "", 1)
        self.assertTrue(result.valid)
        self.assertFalse(result.passed)

    def test_零匹配_ran_為假(self):
        """實測的事件流：package 層級 pass、離開碼 0，但沒有任何 Test 事件。"""
        out = self.events(
            {"Action": "start"},
            {"Action": "output", "Output": "testing: warning: no tests to run\n"},
            {"Action": "pass"},
        )
        result = parse_test_events(out, "", 0)
        self.assertFalse(result.valid)
        self.assertIn("沒有測試匹配", result.detail)

    def test_編譯失敗_ran_為假且說明是編譯問題(self):
        """實測的事件流：build-fail ＋ fail，同樣沒有任何 Test 事件。"""
        out = self.events(
            {"Action": "start"},
            {"Action": "build-output", "Output": "# github.com/example/pkg\n"},
            {"Action": "build-output", "Output": "./x.go:3:2: undefined: foo\n"},
            {"Action": "build-fail"},
            {"Action": "fail"},
        )
        result = parse_test_events(out, "", 1)
        self.assertFalse(result.valid)
        self.assertIn("編譯失敗", result.detail)
        # detail 要指到**壞在哪一行**，而不是 `# <package>` 那行標頭——後者只告訴你
        # 「這個 package 壞了」，而那是你已經知道的事。
        self.assertIn("undefined: foo", result.detail)
        self.assertNotIn("# github.com", result.detail)

    def test_測試通過但_package_另外失敗_是無效證據(self):
        """Codex 第三輪抓到的那條。事件流與離開碼取自對真實 go test 的實測：
        `TestMain` 跑完 `m.Run()` 後 `os.Exit(1)`，得到 run → pass(Test) → fail(package)、
        離開碼 1。

        這種結果說明不了任何事——測試是綠的，但整包是紅的。若判成綠色 control，
        一個同時弄壞 target 斷言與 package 收尾的突變就會被報成「target 轉紅、
        control 仍綠」，一份完全編造出來的證據，而且離開碼是 0。
        """
        out = self.events(
            {"Action": "run", "Test": "TestControl"},
            {"Action": "pass", "Test": "TestControl"},
            {"Action": "fail"},
        )
        result = parse_test_events(out, "", 1)
        self.assertFalse(result.valid, "測試通過但 package 失敗，不得算成綠色")
        self.assertIn("package", result.detail)

    def test_離開碼非零但事件流看似全綠_也是無效證據(self):
        """雙保險：`-json` 事件流與 process 離開碼是兩個獨立來源，綠色要求兩邊都乾淨。

        真實成因例如 `go test` 自己 panic、或訊號中斷——事件流可能停在測試 pass，
        離開碼卻非零。
        """
        out = self.events(
            {"Action": "run", "Test": "TestFoo"},
            {"Action": "pass", "Test": "TestFoo"},
            {"Action": "pass"},
        )
        result = parse_test_events(out, "", 2)
        self.assertFalse(result.valid)

    def test_非_JSON_雜訊不影響解析(self):
        out = "not json at all\n" + self.events(
            {"Action": "run", "Test": "TestFoo"},
            {"Action": "pass", "Test": "TestFoo"},
        )
        self.assertTrue(parse_test_events(out, "", 0).valid)


class OutcomeTableTest(unittest.TestCase):
    def test_結果表完整且只有_ok_算通過(self):
        """**整張表逐項比對**，不是挑幾個來看。

        上一版硬寫了一個 tuple 列舉失敗種類，新增 `no_evidence` 時忘了加進去——
        那格於是宣稱「涵蓋每一種失敗」卻漏了一種。比對整張表的話，任何新增或改動
        都會在這裡轉紅，逼人明確決定它算不算通過。
        """
        self.assertEqual(
            OUTCOMES,
            {
                "ok": True,
                "weak": False,
                "control_red": False,
                "no_evidence": False,
                "baseline": False,
                "miss": False,
            },
        )


class ExitCodeTest(unittest.TestCase):
    """離開碼是這支工具的通過契約：自動化拿它來判斷「突變測試過了沒」。

    判錯的方向只有一種是危險的——把失敗判成通過，那會讓一份假的測試證據被背書。
    """

    def test_全部如預期_回零(self):
        self.assertEqual(exit_code({"ok": 5, "weak": 0, "control_red": 0, "baseline": 0, "miss": 0}), 0)

    def test_一格都沒跑_由_main_擋掉而不是這裡(self):
        # exit_code 只看統計，空統計對它而言沒有失敗可言；「連一格都沒有」是
        # **沒有證據**，那一層的判斷在 main（見 MainExitCodeTest.test_空的_spec_回非零）。
        self.assertEqual(exit_code(dict.fromkeys(OUTCOMES, 0)), 0)

    def test_每一種失敗都讓離開碼非零(self):
        # **從表推導**，不硬寫清單：新增一種失敗時這一格自動涵蓋它。
        failures = [name for name, passes in OUTCOMES.items() if not passes]
        self.assertGreaterEqual(len(failures), 5, "失敗種類少於預期，表可能被改壞了")
        for outcome in failures:
            with self.subTest(outcome=outcome):
                tally = dict.fromkeys(OUTCOMES, 0)
                tally["ok"] = 9  # 大量成功不得淹掉一格失敗
                tally[outcome] = 1
                self.assertEqual(exit_code(tally), 1, f"{outcome} 必須讓離開碼非零")


class MainExitCodeTest(unittest.TestCase):
    """main() 端到端：從 JSON spec 到離開碼。

    ExitCodeTest 守的是判定函式，這裡守的是它**真的有被接上**——曾經有一個
    --keep-going 旗標在最後一步把非零壓成 0，判定函式再正確也沒用。
    """

    def setUp(self):
        self.tmp = tempfile.TemporaryDirectory()
        self.addCleanup(self.tmp.cleanup)
        self.src = Path(self.tmp.name) / "src.go"
        self.src.write_text("alpha\nbravo\n", encoding="utf-8")

    def write_spec(self, **overrides) -> str:
        case = {
            "label": "端到端用的突變",
            "package": "./pkg/",
            "test": "TestTarget",
            "edits": [{"file": str(self.src), "old": "bravo", "new": MARKER}],
        }
        case.update(overrides)
        spec = Path(self.tmp.name) / "spec.json"
        spec.write_text(json.dumps([case]), encoding="utf-8")
        return str(spec)

    def run_main(self, spec: str, sensitive: set[str]) -> int:
        return main(
            [spec],
            runner=make_runner(self.src, sensitive),
            residue_check=lambda: True,
        )

    def test_轉紅_離開碼為零(self):
        self.assertEqual(self.run_main(self.write_spec(), {"TestTarget"}), 0)

    def test_仍綠_離開碼非零(self):
        """沒有任何旗標可以把這個非零壓掉——這正是 --keep-going 被移除的理由。"""
        self.assertEqual(self.run_main(self.write_spec(), set()), 1)

    def test_對照組也轉紅_離開碼非零(self):
        spec = self.write_spec(control="TestControl")
        self.assertEqual(self.run_main(spec, {"TestTarget", "TestControl"}), 1)

    def test_殘留檢查失敗_離開碼為二(self):
        # 突變本身如預期轉紅，但還原後 go build 不綠——那比測試沒守到更嚴重，
        # 用不同的離開碼區分，方便自動化分辨「測試不夠」與「工作樹被弄髒」。
        code = main(
            [self.write_spec()],
            runner=make_runner(self.src, {"TestTarget"}),
            residue_check=lambda: False,
        )
        self.assertEqual(code, 2)

    def test_空的_spec_回非零(self):
        """空的定義不是「零個失敗」，是**沒有證據**。

        生成 spec 的那一步失敗、或整份忘了填，都會長成這個樣子；回 0 等於背書一份
        不存在的驗證。階段 6 要的是 8–16 格實際證據。
        """
        spec = Path(self.tmp.name) / "empty.json"
        spec.write_text("[]", encoding="utf-8")
        self.assertEqual(main([str(spec)], runner=green, residue_check=lambda: True), 1)

    def test_不是陣列的_spec_回非零(self):
        spec = Path(self.tmp.name) / "obj.json"
        spec.write_text('{"label": "忘了包成陣列"}', encoding="utf-8")
        self.assertEqual(main([str(spec)], runner=green, residue_check=lambda: True), 1)

    def test_突變後無效證據_離開碼非零(self):
        spec = self.write_spec()
        code = main(
            [spec],
            runner=make_runner(self.src, {"TestTarget"}, valid_when_broken=False),
            residue_check=lambda: True,
        )
        self.assertEqual(code, 1)

    def test_沒有忽略失敗的旗標(self):
        """守住契約本身：任何新增的旗標都不得讓失敗以 0 收場。"""
        with self.assertRaises(SystemExit):  # argparse 對未知旗標會 exit(2)
            main(["--keep-going", self.write_spec()], runner=green)


if __name__ == "__main__":
    unittest.main(verbosity=2)
