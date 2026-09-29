// request.go 收 JSON 請求 body 的共用解析（spec #73 第八節的輸入限制）。Session 的建立（#78）與發訊息、
// 無狀態呼叫（#79、#80）共用它。
package web

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
)

// maxRequestBodyBytes 是 JSON 請求 body 的上限（spec #73 第八節「整個請求 body 另有一個略大的上限，
// 防止超大的 JSON」）。沒有上限的話，一個帶著幾 GB 字串的 body 會被整個讀進記憶體。
//
// **訂成 256 KiB，是替 #79 的 message 上限（32 KiB）留足 JSON 跳脫的空間**：message 在 body 裡是
// JSON 字串，最壞的情形是控制字元，1 byte 跳脫成 \u00XX 變 6 bytes；中文若被呼叫端轉成 \uXXXX
// （Python json.dumps 的預設）也會從 3 bytes 變 6 bytes。32 KiB × 6 = 192 KiB，再留其餘欄位的
// 空間。第一版訂 64 KiB，只算了「略大於 32 KiB」，沒算跳脫，一個剛好 32 KiB 的中文 message 就會
// 在讀 body 時被擋成 413（Spec 審查指出）。
const maxRequestBodyBytes = 256 << 10

// decodeJSONBody 把請求 body 解進 dst。不合格時已經寫好錯誤回應，
// 呼叫端看到 false 直接返回。
//
//   - **大小有上限**（maxRequestBodyBytes），超過回 413 request_too_large。
//   - **未知欄位回 400，並指出欄位名**：`profle` 這類拼錯要當場被發現，而不是變成「必填欄位缺失」。
//   - **只能有一個 JSON 值**：Decoder 讀完第一個值就停，後面還有東西它不報錯（同 core.decodeToolArgs
//     的理由），所以再讀一次，必須剛好是 io.EOF。
//   - **不要求 Content-Type**：curl 預設的寫法也能用。
func (h *handler) decodeJSONBody(w http.ResponseWriter, r *http.Request, dst any) bool {
	dec := json.NewDecoder(http.MaxBytesReader(w, r.Body, maxRequestBodyBytes))
	dec.DisallowUnknownFields()
	if err := dec.Decode(dst); err != nil {
		h.writeBodyError(w, err)
		return false
	}
	var extra json.RawMessage
	if err := dec.Decode(&extra); !errors.Is(err, io.EOF) {
		if err != nil {
			h.writeBodyError(w, err)
		} else {
			h.writeError(w, http.StatusBadRequest, "invalid_request", "請求 body 在 JSON 物件之後還有其他內容")
		}
		return false
	}
	return true
}

// writeBodyError 把解析 body 的錯誤轉成 400 或 413，訊息指出是哪裡不對。
func (h *handler) writeBodyError(w http.ResponseWriter, err error) {
	var tooLarge *http.MaxBytesError
	var typeErr *json.UnmarshalTypeError
	var syntaxErr *json.SyntaxError
	switch {
	case errors.As(err, &tooLarge):
		h.writeError(w, http.StatusRequestEntityTooLarge, "request_too_large",
			fmt.Sprintf("請求 body 超過 %d bytes 的上限", tooLarge.Limit))
	case errors.Is(err, io.EOF):
		h.writeError(w, http.StatusBadRequest, "invalid_request", "請求 body 是空的，需要一個 JSON 物件")
	case errors.As(err, &typeErr):
		h.writeError(w, http.StatusBadRequest, "invalid_request",
			fmt.Sprintf("欄位 %s 的型別不對，應該是 %s", typeErr.Field, typeErr.Type))
	case errors.As(err, &syntaxErr), errors.Is(err, io.ErrUnexpectedEOF):
		h.writeError(w, http.StatusBadRequest, "invalid_request", "請求 body 不是合法的 JSON："+err.Error())
	default:
		// 未知欄位走這裡：encoding/json 回的是 `json: unknown field "profle"`，沒有專屬的錯誤型別，
		// 但訊息本身已經指名欄位。
		h.writeError(w, http.StatusBadRequest, "invalid_request",
			"請求 body 無法解析："+strings.TrimPrefix(err.Error(), "json: "))
	}
}
