package tool

// MatchToolName 判斷一條**針對 Tool 名的規則**指不指向 registered 這個註冊名。
//
// 比對是**兩端錨定的整名比對**：規則要完整覆蓋整個註冊名才算命中，不是「開頭是它」、
// 也不是「含有它」。
//
// **為什麼要有一個函式，而不是讓每個要比對的地方各寫一次。** MCP 工具的註冊名帶
// server 命名空間前綴（`<server>__<tool>`，見 McpToolName），所以「某個 Tool 名剛好是
// 另一個 Tool 名的前綴」在這個專案裡是**常態而不是邊角**——接一台 MCP server 就會讓
// `srv__read_file` 與內建的 `read_file` 同時存在。前綴式比對之下，一條針對 read_file
// 的規則會把 read_file_v2 與 srv__read_file 一起打中，於是一條規則的實際作用範圍取決於
// 「這台機器上剛好註冊了哪些工具」。那是安全規則最不該有的性質。
//
// 判準與 Sandbox 既有的兩處同源，三者是同一種紀律的三個落點：域名比對不讓
// example.com 命中 evil-example.com（matchDomain）、路徑比對是子樹包含而不是字串前綴
// （withinSubtree）、Tool 名比對是整名而不是前綴（這裡）。
//
// **核心階段還沒有規則來源，這是刻意的。** Profile 的 `tools` 欄位是**子集過濾**而不是
// 規則（`CONTEXT.md` 對 Tool Policy 的界定），走的是 Registry 的名字查表。規則形狀屬
// 擴展階段的 Tool Policy（issue #39）。先把判準與它的表格驅動測試立起來，是為了讓那時
// 接上去的人不必重新決定一次比對形式——而在那之前，這個函式的測試就是擋住「把它優化成
// 前綴比對」那種改法的東西。
//
// **不做萬用字元。** 「對整台 MCP server 整批下規則」是可預見的需求，但那個語法屬
// issue #39，本階段不預先發明（憲法 3.1）。屆時無論加上什麼形狀，錨定這條都不能鬆。
func MatchToolName(rule, registered string) bool {
	return rule == registered
}
