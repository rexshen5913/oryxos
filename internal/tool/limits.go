package tool

// 回填給 LLM 的結果上限集中在這一個常數區塊。
//
// **放同一處是刻意的**：這些數字彼此有關係（它們共同決定一次 turn 最多能塞多少
// 內容進 prompt），散在各 Tool 的檔案裡，下一個要加上限的人看不到別人給了多少，
// 於是每張 ticket 各給一個數字。spec #4 為此把三個上限一次定案（issue #29
// 「回填給 LLM 的結果形狀」）。
//
// 目前住在這裡的是 read_file（沿用 HTTP Tool 已有的上限）與 list_dir 兩格。最後一個
// 數字 spec 也已經定好，**不由 ticket 自己挑**，落地時原樣加進這個區塊：
// shell 的 stdout 與 stderr **各 256 KiB**。
const (
	// maxResponseBytes 限制單次回填給 LLM 的內容大小（資源佔用限制，需求 §5.6）。
	// HTTP Tool 的回應內文與 read_file 的檔案內容共用同一個上限——兩者都是「一段
	// 從外部拿回來、要塞進 prompt 的位元組」，沒有理由給兩個數字。
	maxResponseBytes = 1 << 20 // 1 MiB

	// maxListDirEntries 限制 list_dir 單次回填的條目數（spec #4 定案的確數）。
	// 它與上面那個上限量的是不同東西——那個數位元組，這個數條目——所以不能共用：
	// 一萬個短檔名離 1 MiB 還很遠，但塞進 prompt 一樣是把 token 燒光。
	maxListDirEntries = 1000
)

// newFilePerm 是 write_file 新建檔案的權限：擁有者可讀寫、其他人唯讀，**不含執行位**。
//
// 這不是風格選擇，是 spec #4 定案的一條緩解（issue #29「PATH 與可寫路徑重疊矩陣」）：
// 若父進程的 PATH 含有一個落在 file.allowed_paths 之內的目錄，write_file 就能在那裡
// 放一個與 shell 白名單命令同名的檔案，**把檔案寫入權限升級成命令執行權限**。不給
// 執行位讓新放的檔案不會被 LookPath 選中——但**不宣稱足夠**：覆寫該目錄下既有的
// 可執行檔照樣達到目的，主要緩解是啟動時的重疊警告與部署要求（ticket #33）。
//
// 只在**建立**檔案時生效：open(2) 的 mode 參數對既有檔案沒有作用，覆寫因此不改變
// 目標原有的權限（那是定案的另一半，見 write_file 的說明）。
const newFilePerm = 0o644
