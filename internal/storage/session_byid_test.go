// Session 以 ID 定位的三項操作：建立、依 ID 讀取、依 ID 歸檔（ticket #78，spec #73 第六節）。
// SQLite 用真的（t.TempDir()，憲法 4.3）。
package storage

import (
	"context"
	"errors"
	"path/filepath"
	"testing"
	"time"

	"github.com/rexshen5913/oryxos/internal/core"
)

// newTestSessions 在 t.TempDir() 開一個真實的 SQLite，回傳上面的 Session 儲存；測試結束時關閉。
func newTestSessions(t *testing.T) *SessionManager {
	t.Helper()
	db, err := Open(context.Background(), filepath.Join(t.TempDir(), "oryxos.db"))
	if err != nil {
		t.Fatalf("開啟 SQLite: %v", err)
	}
	t.Cleanup(func() {
		if err := db.Close(); err != nil {
			t.Errorf("關閉 SQLite: %v", err)
		}
	})
	return NewSessionManager(db)
}

// mustCreate 建立一個 Session，失敗時測試失敗。
func mustCreate(t *testing.T, sessions *SessionManager, channel, userID, profileName string) *SessionRecord {
	t.Helper()
	record, err := sessions.Create(context.Background(), channel, userID, profileName)
	if err != nil {
		t.Fatalf("Create(%s, %s, %s): %v", channel, userID, profileName, err)
	}
	return record
}

// TestCreateSession 釘住建立：**建立即落庫**，寫入一筆對話歷史為空的 active 資料列；同一聯合
// 標識已有 active Session 時，回報可辨識的錯誤並帶回既有那一場的 ID（spec #73 第五、六節）。
func TestCreateSession(t *testing.T) {
	ctx := context.Background()

	t.Run("正常建立：active、空歷史、立刻讀得到", func(t *testing.T) {
		sessions := newTestSessions(t)
		created := mustCreate(t, sessions, "web", "alice", "default")
		if created.Status != "active" || created.ArchivedAt != nil {
			t.Errorf("建立後 status = %q、archived_at = %v, 期望 active 與 nil", created.Status, created.ArchivedAt)
		}
		if got := created.Session; got.Channel != "web" || got.UserID != "alice" || got.ProfileName != "default" {
			t.Errorf("聯合標識 = %s／%s／%s, 期望 web／alice／default", got.Channel, got.UserID, got.ProfileName)
		}
		if len(created.Session.Messages) != 0 {
			t.Errorf("建立後的對話歷史有 %d 條, 期望 0", len(created.Session.Messages))
		}
		if !created.CreatedAt.Equal(created.LastActiveAt) {
			t.Errorf("建立後 created_at = %v、last_active_at = %v, 期望相同", created.CreatedAt, created.LastActiveAt)
		}
		// 「立即落庫」：另一次讀取就查得到，不必等第一個 turn。
		read, err := sessions.SessionByID(ctx, created.Session.ID)
		if err != nil {
			t.Fatalf("建立之後依 ID 讀取: %v", err)
		}
		if read.Session.ID != created.Session.ID || read.Status != "active" {
			t.Errorf("讀回 %s（%s）, 期望 %s（active）", read.Session.ID, read.Status, created.Session.ID)
		}
	})

	tests := []struct {
		name string
		// second 是第二次建立的聯合標識；第一次一律是 web／alice／default。
		secondChannel, secondUser, secondProfile string
		// archiveFirst 為真時，第二次建立之前先把第一場歸檔。
		archiveFirst bool
		// wantExists 為真時，期望第二次建立回報「已有 active Session」並帶回第一場的 ID。
		wantExists bool
	}{
		{name: "同一聯合標識已有 active：回報既有的 ID",
			secondChannel: "web", secondUser: "alice", secondProfile: "default", wantExists: true},
		{name: "同一使用者、不同 Profile：兩者都成功",
			secondChannel: "web", secondUser: "alice", secondProfile: "analyst"},
		{name: "同一使用者與 Profile、不同接入來源：兩者都成功",
			secondChannel: "cli", secondUser: "alice", secondProfile: "default"},
		{name: "第一場歸檔之後：可以再建立",
			secondChannel: "web", secondUser: "alice", secondProfile: "default", archiveFirst: true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			sessions := newTestSessions(t)
			first := mustCreate(t, sessions, "web", "alice", "default")
			if tt.archiveFirst {
				if _, err := sessions.ArchiveByID(ctx, first.Session.ID); err != nil {
					t.Fatalf("歸檔第一場: %v", err)
				}
			}
			second, err := sessions.Create(ctx, tt.secondChannel, tt.secondUser, tt.secondProfile)

			var exists *ActiveSessionExistsError
			if tt.wantExists {
				if !errors.As(err, &exists) {
					t.Fatalf("第二次建立的錯誤 = %v, 期望 *ActiveSessionExistsError", err)
				}
				if exists.SessionID != first.Session.ID {
					t.Errorf("回報的既有 ID = %q, 期望第一場的 %q", exists.SessionID, first.Session.ID)
				}
				return
			}
			if err != nil {
				t.Fatalf("第二次建立: %v", err)
			}
			if second.Session.ID == first.Session.ID {
				t.Errorf("兩場的 ID 相同（%s）", first.Session.ID)
			}
		})
	}
}

// TestSessionByID 釘住依 ID 讀取：回傳 Session（含對話歷史）與狀態、三個時間戳；不存在時回可
// 辨識的「找不到」（spec #73 第六節）。
func TestSessionByID(t *testing.T) {
	ctx := context.Background()
	sessions := newTestSessions(t)
	created := mustCreate(t, sessions, "web", "alice", "default")

	// 用既有的 Save 寫進兩條訊息：依 ID 讀取要還原出同一段歷史。
	withHistory := *created.Session
	withHistory.Append(core.Message{Role: core.RoleUser, Content: "第一句"})
	withHistory.Append(core.Message{Role: core.RoleAssistant, Content: "第一句的回覆"})
	if err := sessions.Save(ctx, &withHistory); err != nil {
		t.Fatalf("Save: %v", err)
	}

	t.Run("存在：還原歷史與狀態", func(t *testing.T) {
		read, err := sessions.SessionByID(ctx, created.Session.ID)
		if err != nil {
			t.Fatalf("SessionByID: %v", err)
		}
		if len(read.Session.Messages) != 2 || read.Session.Messages[1].Content != "第一句的回覆" {
			t.Errorf("還原的歷史 = %+v, 期望兩條、第二條是「第一句的回覆」", read.Session.Messages)
		}
		if read.Status != "active" || read.ArchivedAt != nil {
			t.Errorf("status = %q、archived_at = %v, 期望 active 與 nil", read.Status, read.ArchivedAt)
		}
		if read.LastActiveAt.Before(read.CreatedAt) {
			t.Errorf("last_active_at %v 早於 created_at %v", read.LastActiveAt, read.CreatedAt)
		}
	})

	t.Run("不存在：ErrSessionNotFound", func(t *testing.T) {
		if _, err := sessions.SessionByID(ctx, "web:ghost:default:1"); !errors.Is(err, ErrSessionNotFound) {
			t.Errorf("SessionByID(不存在的 ID) 的錯誤 = %v, 期望 ErrSessionNotFound", err)
		}
	})
}

// TestArchiveByID 釘住依 ID 歸檔（spec #73 第六節）：active 轉 archived、蓋上 archived_at、對話歷史
// 不動；已經歸檔的再歸檔一次視為成功，archived_at 不變；不存在時回 ErrSessionNotFound。
//
// **它不接到依聯合標識歸檔上**（見 ArchiveActive 的註解）：最後一格先歸檔 A、為同一聯合標識開
// 新的 B，再對 A 歸檔一次。若依 ID 歸檔其實是照聯合標識找 active 那一列，B 就會被誤歸檔。
func TestArchiveByID(t *testing.T) {
	ctx := context.Background()

	t.Run("首次歸檔：狀態轉 archived，歷史不動", func(t *testing.T) {
		sessions := newTestSessions(t)
		created := mustCreate(t, sessions, "web", "alice", "default")
		withHistory := *created.Session
		withHistory.Append(core.Message{Role: core.RoleUser, Content: "留著"})
		if err := sessions.Save(ctx, &withHistory); err != nil {
			t.Fatalf("Save: %v", err)
		}
		archived, err := sessions.ArchiveByID(ctx, created.Session.ID)
		if err != nil {
			t.Fatalf("ArchiveByID: %v", err)
		}
		if archived.Status != "archived" || archived.ArchivedAt == nil {
			t.Fatalf("歸檔後 status = %q、archived_at = %v, 期望 archived 與非 nil", archived.Status, archived.ArchivedAt)
		}
		if len(archived.Session.Messages) != 1 || archived.Session.Messages[0].Content != "留著" {
			t.Errorf("歸檔後的歷史 = %+v, 期望原樣保留那一條", archived.Session.Messages)
		}
	})

	t.Run("重複歸檔：成功，archived_at 不變", func(t *testing.T) {
		sessions := newTestSessions(t)
		created := mustCreate(t, sessions, "web", "alice", "default")
		first, err := sessions.ArchiveByID(ctx, created.Session.ID)
		if err != nil {
			t.Fatalf("第一次歸檔: %v", err)
		}
		time.Sleep(2 * time.Millisecond) // 讓「第二次蓋掉 archived_at」看得出差別
		second, err := sessions.ArchiveByID(ctx, created.Session.ID)
		if err != nil {
			t.Fatalf("第二次歸檔應該成功（網路重試不該把成功變成錯誤）: %v", err)
		}
		if second.ArchivedAt == nil || !second.ArchivedAt.Equal(*first.ArchivedAt) {
			t.Errorf("第二次歸檔後 archived_at = %v, 期望維持第一次的 %v", second.ArchivedAt, first.ArchivedAt)
		}
	})

	t.Run("不存在：ErrSessionNotFound", func(t *testing.T) {
		sessions := newTestSessions(t)
		if _, err := sessions.ArchiveByID(ctx, "web:ghost:default:1"); !errors.Is(err, ErrSessionNotFound) {
			t.Errorf("ArchiveByID(不存在的 ID) 的錯誤 = %v, 期望 ErrSessionNotFound", err)
		}
	})

	t.Run("對舊 ID 再歸檔一次：不波及同一聯合標識的新 Session", func(t *testing.T) {
		sessions := newTestSessions(t)
		old := mustCreate(t, sessions, "web", "alice", "default")
		if _, err := sessions.ArchiveByID(ctx, old.Session.ID); err != nil {
			t.Fatalf("歸檔舊的: %v", err)
		}
		current := mustCreate(t, sessions, "web", "alice", "default")
		if _, err := sessions.ArchiveByID(ctx, old.Session.ID); err != nil {
			t.Fatalf("再歸檔舊的一次: %v", err)
		}
		read, err := sessions.SessionByID(ctx, current.Session.ID)
		if err != nil {
			t.Fatalf("讀新的: %v", err)
		}
		if read.Status != "active" {
			t.Errorf("新的 Session 被波及，status = %q, 期望 active", read.Status)
		}
	})
}
