// Tests for the account-token pool (account_tokens.go): add/list/acquire/
// result-marking/delete, rotation order, and the auto-disable threshold.

package zbridge

import (
    "encoding/json"
    "fmt"
    "net/http/httptest"
    "path/filepath"
    "strings"
    "testing"
)

// setupAccountTokenStore points the pool at a fresh temp DB per test.
func setupAccountTokenStore(t *testing.T) {
    t.Helper()
    path := filepath.Join(t.TempDir(), "accounts-test.sqlite")
    if err := initAccountTokenDB(path); err != nil {
        t.Fatalf("initAccountTokenDB: %v", err)
    }
    t.Cleanup(func() {
        accountDBMu.Lock()
        if accountDB != nil {
            accountDB.Close()
            accountDB = nil
            accountDBPath = ""
        }
        accountDBMu.Unlock()
    })
}

func TestAccountTokenAddListDelete(t *testing.T) {
    setupAccountTokenStore(t)

    id1, err := addAccountToken("token-aaa", "first")
    if err != nil {
        t.Fatalf("add #1: %v", err)
    }
    if _, err := addAccountToken("token-bbb", "second"); err != nil {
        t.Fatalf("add #2: %v", err)
    }
    // duplicate token → same id, no dup row
    dupID, err := addAccountToken("token-aaa", "dup")
    if err != nil {
        t.Fatalf("add dup: %v", err)
    }
    if dupID != id1 {
        t.Fatalf("duplicate add should return existing id %d, got %d", id1, dupID)
    }

    rows, count, err := listAccountTokens()
    if err != nil {
        t.Fatalf("list: %v", err)
    }
    if count != 2 || len(rows) != 2 {
        t.Fatalf("expected 2 rows, got count=%d len=%d", count, len(rows))
    }
    if rows[0].TokenPrev == "token-aaa" {
        t.Fatalf("token must be masked in list output, got raw: %q", rows[0].TokenPrev)
    }

    if err := deleteAccountToken(id1); err != nil {
        t.Fatalf("delete: %v", err)
    }
    if _, count, _ := listAccountTokens(); count != 1 {
        t.Fatalf("after delete expected 1, got %d", count)
    }
    if err := deleteAccountToken(id1); err == nil {
        t.Fatalf("deleting a missing id should error")
    }
    if _, err := addAccountToken("   ", "blank"); err == nil {
        t.Fatalf("blank token should be rejected")
    }
}

func TestAccountTokenAcquireRotatesAndDisables(t *testing.T) {
    setupAccountTokenStore(t)

    idA, _ := addAccountToken("token-A", "A")
    idB, _ := addAccountToken("token-B", "B")

    // 第一次取：两个都 fresh（fail_count 相同、last_used_at 相同 → 按 id）
    gotID, tok, ok := acquireAccountToken()
    if !ok || gotID != idA || tok != "token-A" {
        t.Fatalf("first acquire should be idA, got id=%d tok=%q ok=%v", gotID, tok, ok)
    }
    // A 成功 → fail_count 重置、last_used_at 更新 → 下次该取 B
    markAccountTokenResult(idA, true)
    gotID, tok, ok = acquireAccountToken()
    if !ok || gotID != idB || tok != "token-B" {
        t.Fatalf("after A success, next should be idB, got id=%d tok=%q", gotID, tok)
    }

    // B 连续失败到阈值 → 自动 disabled → 回到 A
    for i := 0; i < accountTokenFailDisableThreshold; i++ {
        markAccountTokenResult(idB, false)
    }
    gotID, tok, ok = acquireAccountToken()
    if !ok || gotID != idA {
        t.Fatalf("B disabled past threshold → should fall back to idA, got id=%d tok=%q ok=%v", gotID, tok, ok)
    }

    // A 也打残 → 池耗尽
    for i := 0; i < accountTokenFailDisableThreshold; i++ {
        markAccountTokenResult(idA, false)
    }
    if _, _, ok := acquireAccountToken(); ok {
        t.Fatalf("all tokens disabled → acquire should report !ok")
    }
    if got := accountTokenCount(); got != 0 {
        t.Fatalf("all disabled → count should be 0, got %d", got)
    }
}

func TestAccountTokenCountAndEmptyStore(t *testing.T) {
    setupAccountTokenStore(t)
    if got := accountTokenCount(); got != 0 {
        t.Fatalf("empty store count should be 0, got %d", got)
    }
    if _, _, ok := acquireAccountToken(); ok {
        t.Fatalf("empty store should not yield a token")
    }
    for i := 0; i < 3; i++ {
        if _, err := addAccountToken(fmt.Sprintf("tok-%d", i), ""); err != nil {
            t.Fatalf("add %d: %v", i, err)
        }
    }
    if got := accountTokenCount(); got != 3 {
        t.Fatalf("count should be 3, got %d", got)
    }
}

// HTTP 层集成：in-process 驱动 /v1/tokens 三个端点（httptest，无需独立 exe）。
func TestAccountTokenHTTPEndpoints(t *testing.T) {
    setupAccountTokenStore(t)
    handler := NewHandler()

    // 未带鉴权 → 401
    req := httptest.NewRequest("POST", "/v1/tokens", strings.NewReader(`{"token":"x"}`))
    rec := httptest.NewRecorder()
    handler.ServeHTTP(rec, req)
    if rec.Code != 401 {
        t.Fatalf("no-auth should be 401, got %d", rec.Code)
    }

    // 加一个 token
    req = httptest.NewRequest("POST", "/v1/tokens", strings.NewReader(`{"token":"jwt-aaa","label":"a"}`))
    req.Header.Set("Authorization", "Bearer "+config.Auth.Token)
    rec = httptest.NewRecorder()
    handler.ServeHTTP(rec, req)
    if rec.Code != 200 {
        t.Fatalf("add should be 200, got %d body=%s", rec.Code, rec.Body.String())
    }
    var addResp struct {
        Success bool  `json:"success"`
        ID      int64 `json:"id"`
        Count   int   `json:"count"`
    }
    if err := json.Unmarshal(rec.Body.Bytes(), &addResp); err != nil || !addResp.Success || addResp.Count != 1 {
        t.Fatalf("bad add response: %v %s", err, rec.Body.String())
    }

    // 列表应含 1 条且 token 被掩码
    req = httptest.NewRequest("GET", "/v1/tokens", nil)
    req.Header.Set("Authorization", "Bearer "+config.Auth.Token)
    rec = httptest.NewRecorder()
    handler.ServeHTTP(rec, req)
    var listResp struct {
        Success bool              `json:"success"`
        Count   int               `json:"count"`
        Tokens  []accountTokenRow `json:"tokens"`
    }
    if err := json.Unmarshal(rec.Body.Bytes(), &listResp); err != nil || !listResp.Success || listResp.Count != 1 {
        t.Fatalf("bad list response: %v %s", err, rec.Body.String())
    }
    if strings.Contains(listResp.Tokens[0].TokenPrev, "jwt-aaa") {
        t.Fatalf("list must not leak the raw token: %q", listResp.Tokens[0].TokenPrev)
    }

    // 删除
    req = httptest.NewRequest("DELETE", fmt.Sprintf("/v1/tokens/%d", addResp.ID), nil)
    req.Header.Set("Authorization", "Bearer "+config.Auth.Token)
    rec = httptest.NewRecorder()
    handler.ServeHTTP(rec, req)
    if rec.Code != 200 {
        t.Fatalf("delete should be 200, got %d body=%s", rec.Code, rec.Body.String())
    }
    if got := accountTokenCount(); got != 0 {
        t.Fatalf("after delete count should be 0, got %d", got)
    }
}
