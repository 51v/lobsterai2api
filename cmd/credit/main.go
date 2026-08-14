// credit.go — LobsterAI 积分查询（全部账号 + 总计），JSON 输出到 stdout。
//
// 用法:
//
//	go run ./cmd/credit        # 或编译后 ./credit
//	go run ./cmd/credit -pretty  # 人类可读
//
// 接口: GET {upstream}/api/user/quota
// 响应: {code:0, data:{freeCreditsTotal, freeCreditsUsed, ...}}
package main

import (
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"
)

func serverBase() string {
	if v := os.Getenv("LB2A_UPSTREAM_BASE"); v != "" {
		return strings.TrimRight(v, "/")
	}
	fmt.Fprintln(os.Stderr, "credit: LB2A_UPSTREAM_BASE env not set")
	os.Exit(1)
	return ""
}

type authFile struct {
	Auth struct {
		AccessToken string `json:"accessToken"`
		RefreshToken string `json:"refreshToken"`
		ExpiresAt    int64  `json:"expiresAt"`
	} `json:"auth"`
	Account struct {
		UID      string `json:"uid"`
		UserId   string `json:"userId"`
		Nickname string `json:"nickname"`
	} `json:"account"`
}

type accountResult struct {
	UID      string `json:"uid"`
	Nickname string `json:"nickname"`
	Remain   *int64 `json:"remain"`
	Total    *int64 `json:"total"`
	OK       bool   `json:"ok"`
	Error    string `json:"error,omitempty"`
}

// fetchQuota 查单账号积分（用 profile-summary，包含 free + campaign 活动积分）。
// quota 接口只显示 freeCreditsTotal=300，profile-summary 才有 totalCreditsRemaining（如 5297.72）。
func fetchQuota(af *authFile) (remain, total int64, err error) {
	req, err := http.NewRequest(http.MethodGet, serverBase()+"/api/user/profile-summary", nil)
	if err != nil {
		return 0, 0, err
	}
	req.Header.Set("Authorization", "Bearer "+af.Auth.AccessToken)
	req.Header.Set("Accept", "application/json")
	req.Header.Set("User-Agent", "LobsterAI/0.1.0")
	client := &http.Client{Timeout: 20 * time.Second}
	resp, err := client.Do(req)
	if err != nil {
		return 0, 0, err
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 400 {
		return 0, 0, fmt.Errorf("http %d", resp.StatusCode)
	}
	var env struct {
		Code int    `json:"code"`
		Msg  string `json:"msg"`
		Data struct {
			TotalCreditsRemaining float64 `json:"totalCreditsRemaining"`
			CreditItems           []struct {
				Type             string  `json:"type"`
				CreditsRemaining float64 `json:"creditsRemaining"`
				ExpiresAt        string  `json:"expiresAt"`
			} `json:"creditItems"`
		} `json:"data"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&env); err != nil {
		return 0, 0, err
	}
	if env.Code != 0 {
		return 0, 0, fmt.Errorf("code=%d %s", env.Code, env.Msg)
	}
	clamp := func(v float64) int64 {
		if v < 0 {
			return 0
		}
		return int64(v)
	}
	if env.Data.TotalCreditsRemaining > 0 {
		return clamp(env.Data.TotalCreditsRemaining), 0, nil
	}
	return 0, 0, fmt.Errorf("profile-summary: no credits")
}

func main() {
	pretty := len(os.Args) > 1 && os.Args[1] == "-pretty"
	authDir := "./auths"
	if v := os.Getenv("LB2A_AUTH_DIR"); v != "" {
		authDir = v
	}
	files, _ := filepath.Glob(filepath.Join(authDir, "lobsterai-*.json"))
	sort.Strings(files)

	accounts := make([]accountResult, 0, len(files))
	for _, f := range files {
		var af authFile
		raw, err := os.ReadFile(f)
		if err != nil || json.Unmarshal(raw, &af) != nil {
			continue
		}
		res := accountResult{UID: af.Account.UID, Nickname: af.Account.Nickname}
		if af.Auth.AccessToken == "" {
			res.Error = "no accessToken"
			accounts = append(accounts, res)
			continue
		}
		remain, total, err := fetchQuota(&af)
		if err != nil {
			res.Error = err.Error()
		} else {
			res.Remain = &remain
			res.Total = &total
			res.OK = true
		}
		accounts = append(accounts, res)
		time.Sleep(200 * time.Millisecond)
	}

	var totalRemain, totalSize int64
	okCount := 0
	for _, a := range accounts {
		if a.OK {
			okCount++
			if a.Remain != nil {
				totalRemain += *a.Remain
			}
			if a.Total != nil {
				totalSize += *a.Total
			}
		}
	}
	out := map[string]any{
		"service": "lobsterai",
		"ts":      time.Now().Unix(),
		"total": map[string]any{
			"remain":   totalRemain,
			"size":     totalSize,
			"accounts": len(accounts),
			"ok":       okCount,
			"failed":   len(accounts) - okCount,
		},
		"accounts": accounts,
	}
	if pretty {
		printPretty(accounts, totalRemain, totalSize, okCount)
		return
	}
	raw, _ := json.Marshal(out)
	fmt.Println(string(raw))
}

// printPretty 人类可读汇总。
func printPretty(accounts []accountResult, totalRemain, totalSize int64, okCount int) {
	withBalance := 0
	var failed []string
	for _, a := range accounts {
		if a.OK && a.Remain != nil && *a.Remain > 0 {
			withBalance++
		}
		if !a.OK {
			name := a.Nickname
			if name == "" && len(a.UID) >= 8 {
				name = a.UID[:8]
			}
			failed = append(failed, name+" "+a.Error)
		}
	}
	pct := int64(0)
	if totalSize > 0 {
		pct = totalRemain * 100 / totalSize
	}
	fmt.Printf("🦞 LobsterAI 积分日报\n")
	fmt.Printf("账号: %d/%d\n", withBalance, len(accounts))
	fmt.Printf("总计: %d/%d\n", totalRemain, totalSize)
	fmt.Printf("剩余: %d%%\n", pct)
	for _, f := range failed {
		fmt.Printf("⚠️ %s\n", f)
	}
}
