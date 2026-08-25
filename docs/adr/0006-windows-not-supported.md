# Windows 明確不支援

**支援平台是寫過的，只寫了一半。** 需求文檔（`docs/DemandAnalysis.md`）§8.4 兼容性層面明訂「操作系統支援 Linux 主流發行版（Ubuntu 22.04+、CentOS 8+、Debian 11+、Alibaba Cloud Linux 3、Rocky Linux）」——Linux 是既有需求，本 ADR 不動它。

（本文後續一律以「需求文檔 §8.4」指稱這一處。引章節而不引行號是刻意的：行號會隨編輯漂移，章節編號不會。）

沒有被寫下的是**其餘平台的地位**，Windows 尤其。需求文檔只列了 Linux，沒有表態其他平台是排除、是次要支援、還是未決；憲法、`CLAUDE.md` 與 `README.md` 對此也一字未提。

本 ADR 落地**之前**，22 份 tracked Markdown 沒有任何一份提到 Windows。撰寫時的基準 commit 是 `cb7d20296604039488e3127ee44f00ad9f1447d4`：

```sh
git grep -il windows cb7d20296604039488e3127ee44f00ad9f1447d4 -- '*.md'   # → 零命中
```

若本 ADR 最終落在別的 parent 上，用這條自我推導的版本，它不需要維護寫死的 hash：

```sh
add=$(git log --diff-filter=A --format=%H -- docs/adr/0006-windows-not-supported.md | tail -1)
git grep -il windows "${add}^" -- '*.md'                                   # → 零命中
```

兩點說明。**範圍限定 tracked 檔案是刻意的**——`node_modules/` 與 gitignore 的本機檔不算專案的規定。**基準點必須寫明**：本 ADR 本身是第一份提到 Windows 的 tracked Markdown，在它之後的任何 commit 上重跑都會命中本檔，不寫基準點就成了一句自我否定的話。

而程式碼一直在為那個沒有表態的空白付代價：`internal/tool` 有 `//go:build !unix` 的降級實作、測試裡有 `runtime.GOOS == "windows"` 的分支。

**歷史背景一則**：ticket #35 那一輪的 `implement.md` 自檢清單**曾**列有「`GOOS=windows` 的 `go vet` 全綠」。該檔已 gitignore、且逐次工作整檔替換，所以這一條**讀者無法驗證**；**那份自檢清單本身也已隨改版消失**（現在的檔案裡仍會出現同一串指令，但角色是本 ADR 的查證證據，不是自檢要求）。寫在這裡只為說明那個空白當時確實在產生自檢負擔，不作為本 ADR 任何論證的依據。往後的規則見下方「不新增 Windows CI」。

更關鍵的是，這個空白**已經開始生成決策**：spec #5（issue #38）定案「評測用例的初始檔案用宣告式而非 shell 腳本」，寫下的理由是「專案要求跨平台建置，shell 判卷會讓 Windows 直接跑不了」，該理由並隨 ticket #50 發布。一條從未被批准、也與需求文檔對不上的前提，正在替專案做技術選型。

本 ADR 把 Windows 從「未表態」改為「明確不支援」，並且**只做這一件事**。

**現決定：Windows 明確不支援。**

## 決定的邊界（本 ADR 不決定什麼）

維護者定案的原話是「這個專案不需要支援 Windows」。本 ADR **只記錄這一條**，不順帶決定其他平台的地位：

- **Linux 主流發行版** — **既有需求**（需求文檔 §8.4），本 ADR 不改變、也無權改變它的地位。
- **Windows** — 明確不支援。**這是本 ADR 唯一的決定。**
- **macOS** — 需求文檔未列。可查到的證據只有它是開發環境（本機 `make build`／`make test` 的執行平台），**沒有任何文檔授予它部署目標的地位**。不在本 ADR 決定範圍。
- **FreeBSD、OpenBSD、NetBSD** — 需求文檔未列，建置通過但執行未驗證。不在本 ADR 決定範圍。
- **Solaris、AIX** — 需求文檔未列，且**建置就不通過**，原因在相依而非本專案（見事實二）。不在本 ADR 決定範圍。
- **plan9、`js/wasm`、`wasip1/wasm`** — 不在本 ADR 決定範圍，但**會被下方的清理連帶影響**，見 Consequences。

### 事實一：沒有任何平台有自動化的 runtime 驗證

repo 裡唯一的 workflow 是 `deploy-site.yml`，它在 `ubuntu-latest` 上跑 `actions/setup-node` 與 `npm run docs:build`——**建置的是 VitePress 網站，完全不碰 Go**。沒有任何 workflow 建置或測試 OryxOS 本身。

因此「Linux 有 CI 涵蓋」是錯的說法：那台 Ubuntu 只編網站。`make test` 只在開發者本機執行，跑在哪個平台不留紀錄。

**連需求文檔 §8.4 指定的 Linux 也沒有。** 一個被需求明訂為支援目標的平台，沒有任何自動化建置或測試在守它——這是既有缺口，不是本 ADR 造成的，也不由本 ADR 解決；記在這裡是因為它直接決定了下一段的論證強度。

這一點對本 ADR 是誠實但不利的：**Windows 未被驗證，其他平台也一樣。** 排除 Windows 的理由因此不能只靠「沒驗證過」，必須靠下方 Considered Options 的另外兩項。

### 事實二：可建置的邊界比 `unix` 窄，而且卡在相依

Go 的 `unix` build constraint 涵蓋的遠不只 Linux 與 macOS。build tag 的選檔結果（`GOOS=<os> go list -f '{{.GoFiles}}' ./internal/tool`）：

```
linux darwin freebsd openbsd netbsd solaris aix  → mcp_process_unix.go  shell_cancel_unix.go
windows plan9                                    → mcp_process_other.go shell_cancel_other.go
```

但**選得到檔案不等於編得出來**。實際交叉編譯（`CGO_ENABLED=0 GOOS=<os> go build ./...`）：

```
linux darwin freebsd openbsd netbsd   → BUILD OK
solaris (amd64) / aix (ppc64)         → FAIL
windows (amd64)                       → BUILD OK
```

Solaris 與 AIX 的失敗**不在本專案的程式碼**，而在憲法 1.2 指定的純 Go SQLite 驅動：

```
imports modernc.org/sqlite → modernc.org/libc → modernc.org/libc/errno:
  build constraints exclude all Go files in .../libc@v1.74.4/errno
```

換句話說，可建置範圍由 `modernc.org/libc` 的平台支援決定，那比 `unix` 窄。這條約束是憲法 1.2（禁 cgo、用 modernc）的直接後果，不是本 ADR 造成的，本 ADR 也不改變它。

### 為什麼這些區分不是文字遊戲

程式碼裡真實存在的邊界有兩條——build tag 的 `unix` vs `!unix`，以及相依所允許的建置範圍——**兩條都不等於需求文檔那條「Linux 主流發行版」**，也都不等於「Linux＋macOS vs 其餘」。

把決定寫成後者，會在維護者沒有表態的情況下替 macOS 與三個 BSD 定性，那正是本 ADR 開頭在批評的那種「空白在生成決策」。本 ADR 只把 Windows 這一格填上，其餘維持空白——**空白是誠實的狀態，填錯比留空更難修**。

## 與憲法的關係

不衝突。憲法 1.1 要求的是「`CGO_ENABLED=0` 靜態編譯成單一二進制」——那講的是**部署形態**，不是平台清單。排除一個平台不影響單一靜態二進制成立，禁用 cgo（憲法 1.2）的理由也完全不受影響。

## Considered Options

另一條路是補一支 Go 的 CI workflow、`matrix.os` 加上 `windows-latest`，把 Windows 從「編得過」升格為「真的支援」。否決的理由是代價不對稱。

**現況是「編得過」，不是「支援」。** 三項事實各自獨立成立：

1. **沒有留下任何原生 Windows 的驗證紀錄。** `Makefile` 只有 `test` 與 `build` 兩個 target，沒有任何跨平台的驗收步驟。`CGO_ENABLED=0 GOOS=windows go build` 成功、`GOOS=windows go vet ./...` 全綠——但兩者都是**交叉編譯期**的檢查，證明的是「編得過」，不是「跑得對」。repo 裡查不到任何在 Windows 上實際執行的紀錄；本 ADR 只據此主張「未被驗證」，不主張「歷史上從未有人跑過」。

   **這一項單獨不足以支撐排除。** 依上方事實一，沒有任何平台有自動化的 runtime 驗證——連需求文檔指定的 Linux 也沒有。真正把 Windows 分出來的是下面兩項，但兩項的獨有程度不同，見第 3 項末尾。

2. **shell 的程式解析路徑帶著已載明的 Windows 缺口。** ADR-0005 把 shell 的保證收斂成「`argv[0]` 是否在白名單裡一次字串比對」——那一步在任何平台都成立，白名單內容本來就由 Workspace 自訂（`shell.allowed_commands`，預設空清單即全部拒絕），不是問題所在。問題在**它後面那一步**：`lookupInPathDirs` 自行解析 PATH，判準是「普通檔 ＋ 帶任一執行位」，而 Windows 沒有 Unix 執行位、可執行與否由 `PATHEXT` 副檔名決定。該函式的註解已經明白寫著「Windows 的副檔名解析（PATHEXT）不在本切片範圍」——這個缺口是**既有的、已知的、寫在程式碼裡的**，本 ADR 只是把它從「暫不處理」正式改為「不處理」。

3. **正確性測試整批不編譯。** shell 與 MCP 的進程生命週期測試帶 `//go:build unix`，在 Windows 上直接不進編譯。`shell_lifecycle_internal_unix_test.go` 一檔 620 行、11 個頂層測試函式，涵蓋 issue #35 的四道防線與 `pending → ready → handed/abandoned` 四態狀態機（該檔註解特別論證過「三態不夠」）；另有十一格突變測試全數被抓到，紀錄在 commit `00051df` 的訊息裡（`git show -s 00051df`）——兩個「十一」數字相同但不是同一件事，前者是測試函式、後者是被刻意植入的錯誤。這些驗證在 Windows 上**全部消失**。剩下的 `//go:build !unix` 實作沒有 process group 的對應物，只殺得到直接子進程，而這條降級路徑同樣沒有留下被原生執行驗證的紀錄。

   **兩項缺口的獨有程度不同，不可混為一談：**

   - **第 2 項（`PATHEXT`）是 Windows 特有的。** 其他 `!unix` 目標沒有這個問題。
   - **第 3 項（生命週期測試整批缺席）是所有 `!unix` 目標共有的。** 那些測試帶 `//go:build unix`，因此 plan9、`js/wasm`、`wasip1/wasm` 同樣一格都不跑，同樣走 `*_other.go` 的降級路徑。Windows 只是其中之一。

   相對於 `unix` 目標（Linux、各 BSD），第 3 項確實構成實質差異——那是本項在論證裡的作用。但它**不能**被用來論證「Windows 比其他 `!unix` 平台更該被排除」，也正因如此，Consequences 才要求刪 `!unix` 檔案時必須把更寬的影響範圍寫明。

要讓 shell tool 在 Windows 上真的成立，得為 `lookupInPathDirs` 補上 `PATHEXT` 與 Windows 的可執行判準、以 job object 補上進程樹清理、並把那 620 行的生命週期驗證移植過去。這是為一個沒有使用者需求的平台，付企業級正確性的代價，違反憲法 3.1（YAGNI）。

一個未經驗證、程式解析路徑帶著已載明缺口、正確性測試整批不編譯的平台，稱不上被支援。把它寫成不支援，是讓文件對得上事實。

## Consequences

### 本 ADR 授權移除的：判斷式寫死 Windows 的程式碼

只有一類：各測試裡以 `runtime.GOOS == "windows"` 為唯一條件的分支，例如 `internal/config/skill_test.go` 與 `internal/core/agent_bootstrap_test.go` 的 `skipIfNoSymlink`——它對非 Windows 一律直接回傳空字串，判斷的對象**就是** Windows 本身。

移除它們只影響 Windows 上的測試行為，落在本 ADR 的決定範圍內。

### 本 ADR **不**授權移除的：`!unix` 的降級實作

`internal/tool/shell_cancel_other.go`、`internal/tool/mcp_process_other.go` 及其對應的 `*_other_test.go` 是 `//go:build !unix`，**同時服務 plan9 與 `js/wasm`、`wasip1/wasm`**（`js` 與 `wasip1` 是 GOOS，`wasm` 是 GOARCH）。本 ADR 沒有決定這些目標的地位，因此**無權授權刪除它們**——刪了等於順帶把它們一起排除，正是本 ADR 反覆在防的那種越權。

這些檔案的處置**留待後續評估**，兩條合法路徑：

1. **另行定案「`!unix` 一律不支援」**——那是比本 ADR 更寬的一次決定，需要維護者批准，並在該次清理的 ticket 與 commit 訊息裡寫明範圍；之後才能刪檔。
2. **先把 Windows 的部分拆出來**——把降級實作改用 `//go:build windows` 與 `//go:build !unix && !windows` 分家，只刪前者。這樣 Windows 在編譯期失敗、其餘 `!unix` 目標維持現狀，影響範圍剛好等於本 ADR 的決定範圍。

第 2 條是唯一能在**不擴大決定**的前提下達成「Windows 編譯失敗」的做法。若目標只是本 ADR 這一條，就走它。

無論走哪一條，`unix` 目標都不受影響——它們選入的是 `*_unix.go`。其中 Linux（需求文檔 §8.4 指定）、macOS 與三個 BSD 建置通過；Solaris 與 AIX 本來就建置不通過，與清理無關，原因見事實二。

### 為什麼值得讓 Windows 在編譯期失敗

不支援就在編譯期大聲說出來，而不是靜默降級成一條沒人測過的路徑。憲法 5.1 要求錯誤被顯式處理，一個未經驗證的平台路徑是同一類問題的另一種形態。

但這個好處**不足以**成為越權刪檔的理由——上面第 2 條路徑同樣達得到，且不必替 plan9 做決定。

### 逐處判斷的依據

無論哪一類，判準都是同一句：**這段程式碼在一台 Linux 機器上是否還可能被觸發？**

**判準要看呼叫路徑，不是看有沒有 `runtime.GOOS` 守衛。** 這一條是本 ADR 審查過程中踩出來的，值得單獨寫下——`internal/storage` 的 `fileDSN` 就是那個反例，而且它同一個函式裡兩種都有：

**A · 通用、Linux 上會觸發 → 保留**

用 `url.URL` 組 DSN 而不是字串拼接。路徑含 `?` 或 `#` 時，拼接出來的 DSN 會把後半當成 query，`busy_timeout` pragma **靜默失效**（連線照開、表照建，不報任何錯）。Linux 上的路徑一樣可能含這些字元，這段與平台無關。

守著這一項的是 `TestFileDSN` 表中的三格：`unix 絕對路徑`、`含空白`、**`含 ? 與 #`**。最後那格正是 A 的回歸測試——**它與測試骨架、以及 pragma 收尾的斷言都必須保留**。

**B · Windows 特定、Linux 上不會觸發**

`filepath.ToSlash` 與補前導斜線的 drive／UNC 處理，加上 `TestFileDSN` 表中的**兩格**：`windows 磁碟路徑（ToSlash 後）` 與 `windows UNC 路徑（ToSlash 後）`。

正式呼叫路徑是 `dataSourceName` → `filepath.Abs`，Linux 上結果必有前導 `/`、`ToSlash` 是 no-op，那個分支永遠進不去；那兩格測試也是**直接注入 ToSlash 之後的 Windows 路徑形狀**，測試註解自己就寫明了這一點。

**A 與 B 在同一個函式、同一張測試表裡，切割線在表的第 3 格與第 4 格之間**，不是「這支測試屬於誰」。

---

B **與 CRLF 那類不同**，不要混為一談：CRLF 是「Windows 產生的檔案、Linux 消費」，資料真的會抵達 Linux；而一個 Windows 磁碟路徑字串沒有辦法進到 Linux 行程的 `filepath.Abs`。

**本 ADR 不裁定 B 的去留。** 它確實是 Windows 特定、因此落在本 ADR 的決定範圍內，但移除它屬於「精簡」而非「修正」；應與上方 `!unix` 檔案在同一次清理評估裡一併處理，不在這裡逐一判死。若日後真要精簡，動的是那兩行 production code 與那兩格測試 case——**不是整支 `TestFileDSN`**。

這個反例最初被寫成「通用正確性程式碼、不可移除」，是**誤用了本節自己訂的判準**——判準沒問題，它正確地把那段歸為平台特定；錯的是判斷時只看了有沒有 `GOOS` 守衛，沒有去看誰在呼叫、傳進來的是什麼。

移除本身不急，屬獨立清理，不阻塞任何 spec 或 ticket。

### 絕不可移除的：處理來自 Windows 的檔案

`internal/config/skill.go` 與 `internal/config/legacy_bootstrap.go` 的 CRLF 正規化**必須保留**。

**但這兩處的既有註解所給的理由，在本 ADR 之後不再充分，需要換一組。** 它們寫的是「同一份檔案在 Windows checkout 出來就是 CRLF」——那是 `core.autocrlf=true` 在**該台 Windows 機器的工作區**發生的事，而 CRLF 在 commit 時會被正規化回 LF，Linux 端 checkout 拿到的是 LF。若唯一情境是「在 Windows 上執行 oryxos 去讀那份 CRLF 檔案」，本 ADR 正好把它排除了。

真正撐得住保留的，是**檔案沒有經過 Git 文字往返**的那些路徑，它們與程式跑在哪個平台無關：

- Workspace 以壓縮檔、`scp`、共享磁碟或 `Dockerfile` 的 `COPY` 從一台 Windows 機器搬到 Linux 伺服器——中間沒有 Git，CRLF 原樣抵達
- 撰寫者的 Windows 環境 `core.autocrlf` 設成 `false` 或被 `.gitattributes` 覆寫，CRLF 因此**進到 repo 裡**，Linux 端 checkout 拿到的就是 CRLF
- 檔案在 Windows 上由編輯器以 **UTF-8 ＋ CRLF** 存檔（VS Code 在 Windows 的預設換行），再複製進 Workspace

三者都不需要 oryxos 跑在 Windows 上，症狀卻都出現在 Linux：frontmatter 的 `---` 分隔線變成 `---\r`，整份 Skill 被判成「沒有 frontmatter」（issue #16 已為 Bootstrap 的舊模板比對付過同一筆學費）。

**三個例子刻意都是 UTF-8，這是保留論證的適用邊界。** `normalizeNewlines` 是位元組層的 `\r\n` → `\n` 替換，只對 UTF-8／ASCII 成立。UTF-16 存檔（例如 Windows PowerShell 5.1 的 `Out-File` 預設是 UTF-16LE；PowerShell 6+ 才改為 `utf8NoBOM`）換行是 `\r\x00\n\x00`，這道正規化比對不到——**那不是本段論證涵蓋的情況，舉例時不能拿它當證據**。

**檔案來自 Windows，與程式跑在 Windows，是兩件不同的事。** 本 ADR 只排除後者。未來讀者看到這些正規化，不要當成 Windows 支援的殘留物清掉——那會弄壞一批真實使用者，而且症狀出現在一台 Linux 機器上，極難聯想回這裡。

`skill.go` 與 `legacy_bootstrap.go` 的註解原本留著一條指向已排除平台的依據（「在 Windows checkout 出來就是 CRLF」），會讓下一個讀者誤判成可刪——**已改寫成上述理由**：明寫「這與 oryxos 跑在哪個平台無關」，並列出那三條繞過 Git 文字往返的路徑。

**`internal/storage` 的 `fileDSN` 不屬於本節這一類。** 它處理的是 Windows 的路徑**字串形狀**，而那種字串進不到一台 Linux 機器的 `filepath.Abs`——與本節「Windows 產生的檔案、Linux 消費」是兩回事。詳見上方「逐處判斷的依據」，該處也說明了它為什麼不在本 ADR 裁定範圍內。

### spec #5 的理由已更正，決定本身不變

「評測用例的初始檔案用宣告式而非 shell 腳本」**仍然成立**，換掉的是依據。更正後的依據是：宣告式是確定性的、不依賴目標系統上存在哪一種 shell、沒有引號與跳脫的解析坑，且讓佈置與判卷都能寫成可表格驅動測試的純函式。這些理由與平台無關，比原本那條更強。

issue #38 與 ticket #50 原按舊理由寫成——**已於本 ADR 定案當日（2026-08-25）更正完畢**：兩張票的 body 各只替換那一句理由，並各留一則留言記錄出處與「決定不變」。其餘十張 ticket（#45–#49、#51–#55）掃過，無同類敘述。

記在這裡是為了讓後續讀者知道這條後果已經落地，不必再去找。

### 不新增 Windows CI

本 ADR 之後，`GOOS=windows` 的 vet 不再是自檢項目——日後的 `implement.md` 自檢清單不應再列它（該檔已 gitignore、逐次工作替換，這裡給的是往後的規則，不是指向某一行現存文字）。

日後若要重新納入 Windows，必須明確 supersede 本 ADR，而不是靠某張 ticket 順手把它加回自檢清單或 CI matrix。這條約束與上方的清理討論方向相反但同源：**平台範圍的增減都得是明示的決定，不是某次改動的副作用。**

### README 已寫明「不支援 Windows」

`README.md` 原本對平台一字未提，使用者無從得知。**已於技術棧表格補上一列**，內容是本 ADR 決定了的那一條——**不支援 Windows**——加上需求文檔 §8.4 的 Linux 主流發行版與 macOS 的開發環境定位，並註明其餘平台未表態。

正面的支援清單**已經有出處**：需求文檔 §8.4。README 引用它，沒有另立一份。

留給日後編輯 README 的人兩條約束：

- 措辭必須對得上事實一與事實二——Linux 是需求指定的支援平台，但**沒有自動化 runtime 驗證**（唯一的 CI 只建置網站）；macOS 是開發環境，需求文檔未賦予部署地位；FreeBSD／OpenBSD／NetBSD 建置通過但執行未驗證；Solaris 與 AIX 建置就不通過。
- **不要把「網站 CI 跑在 Ubuntu」寫成「Linux 已被涵蓋」**——那是審查抓到過的過度解讀。

不要在 README 寫出一份看起來權威的「支援平台清單」：那份清單本 ADR 沒有授權，寫出來就等於用文檔悄悄完成一次未經定案的決定。

### 憲法未動

理由見上方「與憲法的關係」，此處只記後果：本 ADR **不**觸發憲法修訂。

若日後要把平台範圍升格為不可協商原則——無論是把 Windows 的排除寫進憲法，或是決定一份正式的支援平台清單——那都是一次憲法修訂，需人類批准並帶明確版本號與批准日期（憲法治理條款）。ADR 記錄已批准的決定，不代替批准。
