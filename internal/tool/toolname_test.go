package tool_test

import (
	"testing"

	"github.com/rexshen5913/oryxos/internal/tool"
)

// TestMatchToolName 釘住 Tool 名比對是**兩端錨定**的整名比對。
//
// 要證的是**前綴假匹配不得中**，兩個方向都要：一條針對 read_file 的規則不得打中
// read_file_v2（右邊多出來），也不得打中 srv__read_file（左邊多出來）；反過來，
// 針對 srv__read_file 的規則也不得打中裸的 read_file。
//
// 這不是邊角案例。MCP 工具的註冊名帶 server 命名空間前綴（`<server>__<tool>`），所以
// 「某個 Tool 名剛好是另一個 Tool 名的前綴」在接了 MCP server 的 Workspace 裡是常態。
// 前綴式比對之下，一條規則的實際作用範圍會取決於「這台機器上剛好註冊了哪些工具」
// ——那是安全規則最不該有的性質。
//
// 判準與 Sandbox 既有的兩處同源：域名比對不讓 example.com 匹配 evil-example.com、
// 路徑比對是子樹包含而不是字串前綴。這一格擋的是把它「優化」成前綴比對的改法。
func TestMatchToolName(t *testing.T) {
	tests := []struct {
		name       string
		rule       string
		registered string
		want       bool
	}{
		{name: "整名相同才匹配", rule: "read_file", registered: "read_file", want: true},
		{name: "MCP 註冊名整名相同才匹配", rule: "srv__read_file", registered: "srv__read_file", want: true},

		{name: "規則不匹配以它為前綴的較長名字", rule: "read_file", registered: "read_file_v2"},
		{name: "規則不匹配帶 MCP server 前綴的同名工具", rule: "read_file", registered: "srv__read_file"},
		{name: "反向：帶 server 前綴的規則不匹配裸名", rule: "srv__read_file", registered: "read_file"},
		{name: "反向：較長的規則不匹配以它為前綴被截短的名字", rule: "read_file_v2", registered: "read_file"},

		{name: "同名工具來自不同 server 時不互相匹配", rule: "srv__read_file", registered: "srv2__read_file"},
		{
			// server 名本身可以含雙底線（`foo` 與 `foo__bar` 能同時宣告），所以這一格
			// 的兩個名字在前綴比對之下會互相牽連。錨定之後它們就是兩個不同的名字。
			name: "server 名互為前綴時不互相匹配", rule: "foo__echo", registered: "foo__bar__echo",
		},

		{name: "大小寫不同不匹配", rule: "read_file", registered: "Read_File"},
		{name: "空規則不匹配任何名字", rule: "", registered: "read_file"},
		{name: "任何規則都不匹配空名字", rule: "read_file", registered: ""},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := tool.MatchToolName(tt.rule, tt.registered); got != tt.want {
				t.Errorf("MatchToolName(%q, %q) = %v, 期望 %v", tt.rule, tt.registered, got, tt.want)
			}
		})
	}
}
