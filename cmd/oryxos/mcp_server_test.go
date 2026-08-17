// 本檔提供組裝點測試用的**真實 stdio MCP server**：以子進程啟動、走真實的 JSON-RPC
// 往返（憲法 4.3、ADR-0002）。不 mock 協議、不注入假 transport。
//
// 手法與 internal/core 那份相同——測試二進制兼任 server，TestMain 在跑任何測試之前
// 早退到服務迴圈。兩個測試套件各有一份是刻意的：唯一的替代是多開一個 package 給測試
// 共用，而憲法 1.3 把 internal/ 的分包固定成 8 個。本專案的測試輔助本來就在這兩個
// package 間各有一份（newReplayServer、readFixture 都是），這裡沿用同一個取捨。
//
// 這份刻意做成最小：協議較真的那些檢查（缺 protocolVersion、交握未完成就發請求）留在
// internal/core/mcp_server_test.go，那裡是驗協議正確性的地方。這裡要驗的是**組裝點有
// 沒有真的把 mcp_servers.yaml 接起來**。
package main

import (
	"bufio"
	"bytes"
	"encoding/json"
	"io"
	"os"
	"strings"
	"testing"
)

const (
	mcpServerModeEnv  = "ORYXOS_TEST_MCP_SERVER"
	mcpServerNameEnv  = "ORYXOS_TEST_MCP_NAME"
	mcpServerToolsEnv = "ORYXOS_TEST_MCP_TOOLS"
	// mcpServerExitMarkerEnv 指向一個檔案，server 在 stdin 被關閉（MCP stdio 規範裡
	// 「請你收工」的訊號）而退出前寫進去。
	//
	// 這是從 client 端唯一看得到「子進程真的被收掉了」的可移植方式：檢查 PID 還在不在
	// 得動用訊號，那是 Unix 專屬的，會逼出一組 build tag 檔（見 internal/tool 那兩份）。
	// marker 存在即代表整條收尾走完——關 stdin、server 收到 EOF、自己退出。
	mcpServerExitMarkerEnv = "ORYXOS_TEST_MCP_EXIT_MARKER"
)

// TestMain 讓測試二進制在子進程模式下改當 MCP server。
func TestMain(m *testing.M) {
	if os.Getenv(mcpServerModeEnv) != "" {
		serveTestMcpServer(os.Stdin, os.Stdout)
		return
	}
	os.Exit(m.Run())
}

// serveTestMcpServer 讀 stdin 的逐行 JSON-RPC、回應到 stdout，直到 stdin 關閉。
func serveTestMcpServer(in io.Reader, out io.Writer) {
	name := os.Getenv(mcpServerNameEnv)
	var tools []string
	if raw := os.Getenv(mcpServerToolsEnv); raw != "" {
		tools = strings.Split(raw, ",")
	}

	reader := bufio.NewReader(in)
	for {
		line, err := reader.ReadBytes('\n')
		if len(bytes.TrimSpace(line)) > 0 {
			handleTestMcpMessage(out, name, tools, line)
		}
		if err != nil {
			// stdin 關閉（或出錯）就收工。寫失敗只能忽略：這裡是子進程，沒有
			// *testing.T 可以回報，而 marker 沒寫出來的後果是斷言「子進程被收掉了」
			// 的測試轉紅——那正確地指向有東西壞了。
			if marker := os.Getenv(mcpServerExitMarkerEnv); marker != "" {
				_ = os.WriteFile(marker, []byte("exited\n"), 0o644)
			}
			return
		}
	}
}

func handleTestMcpMessage(out io.Writer, name string, tools []string, line []byte) {
	var req struct {
		ID     *int   `json:"id"`
		Method string `json:"method"`
		Params struct {
			Name      string `json:"name"`
			Arguments struct {
				Text string `json:"text"`
			} `json:"arguments"`
		} `json:"params"`
	}
	if err := json.Unmarshal(line, &req); err != nil {
		return
	}

	switch req.Method {
	case "initialize":
		writeTestMcpMessage(out, req.ID, map[string]any{
			"protocolVersion": "2025-06-18",
			"capabilities":    map[string]any{"tools": map[string]any{}},
			"serverInfo":      map[string]any{"name": name, "version": "0.0.1-test"},
		})
	case "notifications/initialized":
		// 通知不回應。
	case "tools/list":
		decls := make([]map[string]any, 0, len(tools))
		for _, toolName := range tools {
			decls = append(decls, map[string]any{
				"name":        toolName,
				"description": "把收到的 text 原樣回覆（" + name + " 的測試工具）",
				"inputSchema": map[string]any{
					"type":       "object",
					"properties": map[string]any{"text": map[string]any{"type": "string"}},
				},
			})
		}
		writeTestMcpMessage(out, req.ID, map[string]any{"tools": decls})
	case "tools/call":
		writeTestMcpMessage(out, req.ID, map[string]any{
			"content": []any{map[string]any{
				"type": "text",
				"text": name + "/" + req.Params.Name + " 收到：" + req.Params.Arguments.Text,
			}},
		})
	}
}

func writeTestMcpMessage(out io.Writer, id *int, result any) {
	line, err := json.Marshal(map[string]any{"jsonrpc": "2.0", "id": id, "result": result})
	if err != nil {
		return
	}
	_, _ = out.Write(append(line, '\n'))
}
