// Session ID 不重複（ticket #80，spec #73 第七節）。無狀態呼叫讓同一位使用者對同一份 Profile 並行建立
// Session，ID 相同的話，兩次呼叫的審計記錄會被混成一次。
package core_test

import (
	"strings"
	"sync"
	"testing"

	"github.com/rexshen5913/oryxos/internal/core"
)

// TestNewSessionIDsAreUnique 釘住同一個聯合標識並行建立的 Session，ID 兩兩不同。
//
// 只靠時間戳不夠：macOS 的時鐘只精確到微秒（奈秒那三位永遠是 000），同一微秒內建立的兩個 Session
// 會拿到同一個 ID。這支測試在幾毫秒內建立上萬個，微秒時鐘的平台上一定會撞；時鐘真的到奈秒的平台上
// 可能撞不到，那裡它分辨不出修改前後。
//
// ID 仍以聯合標識開頭：它會出現在日誌與審計裡，人要能一眼看出是誰、哪份 Profile。
func TestNewSessionIDsAreUnique(t *testing.T) {
	const goroutines, perGoroutine = 8, 2000
	ids := make(chan string, goroutines*perGoroutine)
	var wg sync.WaitGroup
	for range goroutines {
		wg.Go(func() {
			for range perGoroutine {
				ids <- core.NewSession("web", "alice", "default").ID
			}
		})
	}
	wg.Wait()
	close(ids)

	seen := make(map[string]bool, goroutines*perGoroutine)
	for id := range ids {
		if seen[id] {
			t.Fatalf("Session ID 重複: %s", id)
		}
		seen[id] = true
		if !strings.HasPrefix(id, "web:alice:default:") {
			t.Fatalf("Session ID = %q, 期望以聯合標識 web:alice:default: 開頭", id)
		}
	}
}
