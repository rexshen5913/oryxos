package tool

// 回填給 LLM 的結果上限集中在這一個常數區塊。
//
// **放同一處是刻意的**：這些數字彼此有關係（它們共同決定一次 turn 最多能塞多少
// 內容進 prompt），散在各 Tool 的檔案裡，下一個要加上限的人看不到別人給了多少，
// 於是每張 ticket 各給一個數字。spec #4 為此把三個上限一次定案（issue #29
// 「回填給 LLM 的結果形狀」）。
//
// 目前住在這裡的只有 read_file 那一格（它沿用 HTTP Tool 已有的上限）。另外兩個
// 數字 spec 已經定好，**不由 ticket 自己挑**，落地時原樣加進這個區塊：
// list_dir 的條目上限 **1000**、shell 的 stdout 與 stderr **各 256 KiB**。
const (
	// maxResponseBytes 限制單次回填給 LLM 的內容大小（資源佔用限制，需求 §5.6）。
	// HTTP Tool 的回應內文與 read_file 的檔案內容共用同一個上限——兩者都是「一段
	// 從外部拿回來、要塞進 prompt 的位元組」，沒有理由給兩個數字。
	maxResponseBytes = 1 << 20 // 1 MiB
)
