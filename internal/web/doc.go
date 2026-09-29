// Package web 實作核心能力五（Web Service）：以標準庫 net/http 的 ServeMux 路由，提供核心
// 10 個 REST 端點的 handler、統一的 JSON 錯誤形狀、CORS 與請求日誌。
//
// 它不為了測試另立介面：測試 seam 在命令層的 server 執行函式（spec #73 Testing Decisions），
// 從最外層發真實的 HTTP 請求驅動，本 package 的行為全部從那裡看得到。
package web
