package main

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func testState(t *testing.T, upstream string) *State {
	t.Helper()
	dir := t.TempDir()
	cfg := defaultConfig()
	cfg.AdminKey = "admin-test-key"
	cfg.ProxyKey = "proxy-test-key"
	cfg.UpstreamBase = upstream
	cfg.Accounts = []Account{{Name: "测试账号", Key: "upstream-test-key", Enabled: true}}
	state := &State{
		dataDir:      dir,
		configPath:   filepath.Join(dir, "config.json"),
		metadataPath: filepath.Join(dir, "metadata.json"),
		config:       cfg,
		metadata:     defaultMetadata(),
		authLimiter:  newAuthLimiter(),
		client:       &http.Client{},
	}
	var err error
	state.logs, err = newRequestLogStore(filepath.Join(dir, "request-logs.jsonl"), time.Now())
	if err != nil {
		t.Fatal(err)
	}
	state.scheduler = &Scheduler{stateConfig: func() (int, int, int) {
		state.mu.RLock()
		defer state.mu.RUnlock()
		return state.config.GlobalConcurrency, state.config.AccountConcurrency, state.config.GlobalRPM
	}, state: schedulerState{byKey: map[string]int{}, activeByKey: map[string]int{}}}
	return state
}

func TestRequestLogStoreQueryAndStats(t *testing.T) {
	store, err := newRequestLogStore(filepath.Join(t.TempDir(), "request-logs.jsonl"), time.Now())
	if err != nil {
		t.Fatal(err)
	}
	store.Append(map[string]any{"model": "cline-pass/test", "status": 200, "usage": map[string]any{"total_tokens": 7}})
	store.Append(map[string]any{"model": "cline-pass/test", "status": 429, "error": "rate limited", "trace": []any{map[string]any{"status": 429}, map[string]any{"status": 200}}})
	result := store.Query(urlValues("status", "error"))
	if result["total"] != 1 {
		t.Fatalf("expected one error, got %#v", result["total"])
	}
	stats := store.Stats(0)
	today := stats["today"].(map[string]any)
	if today["requests"] != 2 || today["tokens"] != 7.0 {
		t.Fatalf("unexpected stats: %#v", today)
	}
}

func TestSchedulerEnforcesPerAccountLimit(t *testing.T) {
	state := testState(t, "http://127.0.0.1")
	state.config.GlobalConcurrency = 2
	state.config.AccountConcurrency = 1
	release, err := state.acquireSlot(context.Background(), "account-a")
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 40*time.Millisecond)
	defer cancel()
	if _, err := state.acquireSlot(ctx, "account-a"); err == nil {
		t.Fatal("second request should wait for the per-account slot")
	}
	release()
	if _, err := state.acquireSlot(context.Background(), "account-a"); err != nil {
		t.Fatalf("slot should be reusable: %v", err)
	}
}

func TestHTTPAuthAndChatProxy(t *testing.T) {
	state := testState(t, "http://upstream.invalid")
	state.client.Transport = roundTripFunc(func(request *http.Request) (*http.Response, error) {
		if request.Header.Get("Authorization") != "Bearer upstream-test-key" {
			t.Fatalf("upstream authorization header missing")
		}
		payload, _ := json.Marshal(map[string]any{"choices": []any{map[string]any{"message": map[string]any{"content": "OK"}}}, "usage": map[string]any{"total_tokens": 3}})
		return &http.Response{StatusCode: http.StatusOK, Header: http.Header{"Content-Type": []string{"application/json"}}, Body: io.NopCloser(strings.NewReader(string(payload))), Request: request}, nil
	})

	unauthorizedRequest := httptest.NewRequest(http.MethodGet, "/api/models", nil)
	unauthorizedRecorder := httptest.NewRecorder()
	state.router().ServeHTTP(unauthorizedRecorder, unauthorizedRequest)
	if unauthorizedRecorder.Code != http.StatusUnauthorized {
		t.Fatalf("admin endpoint should reject unauthenticated requests: status=%d", unauthorizedRecorder.Code)
	}

	request := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(`{"model":"cline-pass/test","messages":[]}`))
	request.Header.Set("Authorization", "Bearer proxy-test-key")
	request.Header.Set("Content-Type", "application/json")
	recorder := httptest.NewRecorder()
	state.router().ServeHTTP(recorder, request)
	if recorder.Code != http.StatusOK {
		t.Fatalf("proxy status = %d body=%s", recorder.Code, recorder.Body.String())
	}
	if recorder.Header().Get("X-Cline-Attempts") != "1" {
		t.Fatalf("expected one attempt, got %q", recorder.Header().Get("X-Cline-Attempts"))
	}

	state.client.Transport = roundTripFunc(func(request *http.Request) (*http.Response, error) {
		return &http.Response{
			StatusCode: http.StatusOK,
			Header:     http.Header{"Content-Type": []string{"text/event-stream"}},
			Body:       io.NopCloser(strings.NewReader("data: {\"provider\":\"Demo\",\"model\":\"demo/model\",\"usage\":{\"total_tokens\":4}}\n\ndata: [DONE]\n\n")),
			Request:    request,
		}, nil
	})
	streamRequest := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(`{"model":"cline-pass/test","messages":[],"stream":true}`))
	streamRequest.Header.Set("Authorization", "Bearer proxy-test-key")
	streamRequest.Header.Set("Content-Type", "application/json")
	streamRecorder := httptest.NewRecorder()
	state.router().ServeHTTP(streamRecorder, streamRequest)
	if streamRecorder.Code != http.StatusOK || !strings.Contains(streamRecorder.Body.String(), "[DONE]") {
		t.Fatalf("unexpected stream response: status=%d body=%s", streamRecorder.Code, streamRecorder.Body.String())
	}
}

func TestSecurityBoundaries(t *testing.T) {
	limiter := newAuthLimiter()
	now := time.Now()
	key := "198.51.100.20"
	for i := 0; i < limiter.limit; i++ {
		if !limiter.allow(key, now) {
			t.Fatalf("request %d should be allowed before the threshold", i)
		}
		limiter.failure(key, now)
	}
	if limiter.allow(key, now) {
		t.Fatal("authentication limiter should block repeated failures")
	}
	limiter.success(key)
	if !limiter.allow(key, now) {
		t.Fatal("successful authentication should clear the failure bucket")
	}

	request := httptest.NewRequest(http.MethodPost, "/api/config", bytes.NewReader(bytes.Repeat([]byte("x"), maxRequestBodySize+1)))
	var payload map[string]any
	if err := decodeBody(request, &payload); err == nil {
		t.Fatal("oversized request body should be rejected")
	}

	path := filepath.Join(t.TempDir(), "private.json")
	if err := atomicWriteJSON(path, map[string]string{"secret": "value"}); err != nil {
		t.Fatal(err)
	}
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Perm() != 0600 {
		t.Fatalf("private file mode = %o, want 600", info.Mode().Perm())
	}
}

func TestLegacyAccountsDefaultToEnabled(t *testing.T) {
	cfg := defaultConfig()
	cfg.Accounts = []Account{{Name: "legacy", Key: "secret"}}
	raw := map[string]json.RawMessage{}
	raw["accounts"] = json.RawMessage(`[{"name":"legacy","key":"secret"}]`)
	ensureState(&cfg, raw)
	if !cfg.Accounts[0].Enabled {
		t.Fatal("legacy accounts without enabled should remain enabled")
	}
}

func TestSecurityInputValidation(t *testing.T) {
	if err := validateAuthKey(strings.Repeat("a", maxAuthKeyBytes+1)); err == nil {
		t.Fatal("oversized auth key should be rejected")
	}
	if err := validateAccountKey("key\nvalue"); err == nil {
		t.Fatal("account key containing a control character should be rejected")
	}
	for _, value := range []string{"file:///tmp/upstream", "ftp://upstream.example", "https://user:pass@upstream.example/api"} {
		if err := validateUpstreamBase(value); err == nil {
			t.Fatalf("upstream URL should be rejected: %s", value)
		}
	}
	if data, err := readLimitedBody(strings.NewReader("12345"), 4); err == nil || string(data) != "1234" {
		t.Fatalf("oversized upstream body should be truncated and rejected: data=%q err=%v", data, err)
	}
}

func TestAuthLimiterScopesDoNotInterfere(t *testing.T) {
	state := testState(t, "http://127.0.0.1")
	for index := 0; index < state.authLimiter.limit; index++ {
		request := httptest.NewRequest(http.MethodGet, "/api/models", nil)
		recorder := httptest.NewRecorder()
		state.router().ServeHTTP(recorder, request)
		if recorder.Code != http.StatusUnauthorized {
			t.Fatalf("admin failure %d returned status %d", index, recorder.Code)
		}
	}
	proxyRequest := httptest.NewRequest(http.MethodGet, "/v1/models", nil)
	proxyRequest.Header.Set("Authorization", "Bearer proxy-test-key")
	proxyRecorder := httptest.NewRecorder()
	state.router().ServeHTTP(proxyRecorder, proxyRequest)
	if proxyRecorder.Code != http.StatusOK {
		t.Fatalf("valid proxy request returned status %d", proxyRecorder.Code)
	}
	blockedAdmin := httptest.NewRequest(http.MethodGet, "/api/models", nil)
	blockedRecorder := httptest.NewRecorder()
	state.router().ServeHTTP(blockedRecorder, blockedAdmin)
	if blockedRecorder.Code != http.StatusUnauthorized {
		t.Fatalf("admin limiter should remain blocked after proxy success, got %d", blockedRecorder.Code)
	}
}

type roundTripFunc func(*http.Request) (*http.Response, error)

func (roundTrip roundTripFunc) RoundTrip(request *http.Request) (*http.Response, error) {
	return roundTrip(request)
}

func TestRoutingRetryAndValidate(t *testing.T) {
	direct := parseRouting(map[string]any{"provider": "GMICloud", "choices": []any{map[string]any{"message": map[string]any{"content": "OK"}}}})
	if direct["pipeline"] != "direct" || direct["finalProvider"] != "gmicloud" {
		t.Fatalf("direct pipeline misread: %#v", direct)
	}
	planner := parseRouting(map[string]any{"provider_metadata": map[string]any{"gateway": map[string]any{"routing": map[string]any{"finalProvider": "alibaba"}}}})
	if planner["pipeline"] != "planner" || planner["finalProvider"] != "alibaba" {
		t.Fatalf("planner pipeline misread: %#v", planner)
	}

	state := testState(t, "http://upstream.invalid")
	state.config.MaxRetries = 2
	state.config.RateLimitSwitchRetries = 0
	if state.shouldRetry(0, 429, "rate limit") {
		t.Fatal("429 retried despite zero switch budget")
	}
	state.config.AccountMode = "roundrobin"
	state.config.RateLimitSwitchRetries = 1
	state.config.Accounts = []Account{{Name: "a", Key: "key-a", Enabled: true}, {Name: "b", Key: "key-b", Enabled: true}}
	var seen []string
	state.client.Transport = roundTripFunc(func(request *http.Request) (*http.Response, error) {
		seen = append(seen, request.Header.Get("Authorization"))
		body := `{"choices":[{"message":{"content":"OK"}}]}`
		status := http.StatusOK
		if len(seen) == 1 {
			body = `{"error":{"message":"rate limit"}}`
			status = http.StatusTooManyRequests
		}
		return &http.Response{StatusCode: status, Header: http.Header{"Content-Type": []string{"application/json"}}, Body: io.NopCloser(strings.NewReader(body)), Request: request}, nil
	})
	request := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(`{"model":"cline-pass/test"}`))
	result, err := state.runChatChain(request, map[string]any{"model": "cline-pass/test", "messages": []any{}}, "cline-pass/test", ModelConfig{}, false, time.Minute)
	if err != nil || result.Status != http.StatusOK || len(seen) != 2 || seen[0] == seen[1] || !state.blocked(Account{Key: "key-a"}, "", time.Now()) {
		t.Fatalf("429 did not switch account once: status=%v err=%v seen=%v", result, err, seen)
	}

	state.metadata.Models["cline-pass/test"] = map[string]any{"upstreams": []any{"alpha"}}
	state.client.Transport = roundTripFunc(func(request *http.Request) (*http.Response, error) {
		payload := `{"error":{"message":"model not found. Available providers are: Alpha, beta-2."}}`
		return &http.Response{StatusCode: http.StatusBadRequest, Header: http.Header{"Content-Type": []string{"application/json"}}, Body: io.NopCloser(strings.NewReader(payload)), Request: request}, nil
	})
	validated := state.validateUpstreams("cline-pass/test")
	if validated["alpha"]["status"] != "bad" {
		t.Fatalf("validate status: %#v", validated["alpha"])
	}
	saved := state.metadata.Models["cline-pass/test"]
	if mapValue(mapValue(saved["upstreamStatus"])["alpha"])["status"] != "bad" || !contains(arrayStrings(saved["upstreams"]), "beta-2") {
		t.Fatalf("validate result not stored: %#v", saved)
	}

	save := httptest.NewRequest(http.MethodPost, "/api/accounts", strings.NewReader(`{"accounts":[{"name":"a","key":"upstream-test-key","enabled":true}],"mode":"single","active":0,"maxRetries":0,"rateLimitSwitchRetries":0}`))
	save.Header.Set("X-Admin-Key", "admin-test-key")
	recorder := httptest.NewRecorder()
	state.router().ServeHTTP(recorder, save)
	if recorder.Code != http.StatusOK || state.config.MaxRetries != 0 || state.config.RateLimitSwitchRetries != 0 {
		t.Fatalf("explicit zero retries not kept: status=%d max=%d rate=%d body=%s", recorder.Code, state.config.MaxRetries, state.config.RateLimitSwitchRetries, recorder.Body.String())
	}
	kept := len(state.config.Accounts)
	schedule := httptest.NewRequest(http.MethodPost, "/api/accounts", strings.NewReader(`{"globalConcurrency":2}`))
	schedule.Header.Set("X-Admin-Key", "admin-test-key")
	scheduleRecorder := httptest.NewRecorder()
	state.router().ServeHTTP(scheduleRecorder, schedule)
	if scheduleRecorder.Code != http.StatusOK || state.config.GlobalConcurrency != 2 || len(state.config.Accounts) != kept || state.config.Accounts[0].Key != "upstream-test-key" {
		t.Fatalf("schedule save changed accounts: status=%d global=%d accounts=%d body=%s", scheduleRecorder.Code, state.config.GlobalConcurrency, len(state.config.Accounts), scheduleRecorder.Body.String())
	}
}

func TestMaskedAccountKeyIsNotSaved(t *testing.T) {
	state := testState(t, "https://upstream.invalid")
	real := "sk_abcdefghijklmnopqrstuvwxyz0123"
	state.config.Accounts = []Account{{Name: "jm01", Key: real, Enabled: true}}
	id := accountID(real)
	save := func(key string) int {
		body, _ := json.Marshal(map[string]any{
			"accounts": []map[string]any{{"id": id, "name": "jm01", "key": key, "enabled": true}},
			"mode":     "single", "active": 0, "accountConcurrency": 1,
		})
		request := httptest.NewRequest(http.MethodPost, "/api/accounts", bytes.NewReader(body))
		request.Header.Set("X-Admin-Key", state.config.AdminKey)
		recorder := httptest.NewRecorder()
		state.router().ServeHTTP(recorder, request)
		return recorder.Code
	}
	// 掩码的圆点数量被输入法/截断改动，或换成半角星号时，仍必须回退到已保存的真实 key
	masks := []string{"••••••••", "•••••••", "••••••••••", "•••••••••••", "••••••••••••••••", "********", "******", " ··· "}
	for _, masked := range masks {
		state.config.Accounts[0].Key = real
		if code := save(masked); code != http.StatusOK || state.config.Accounts[0].Key != real {
			t.Fatalf("掩码 %q 覆盖了真实 key: code=%d key=%q", masked, code, state.config.Accounts[0].Key)
		}
	}
	// 真正的新 key 仍要生效，且以星号开头的长 key 不能被误判为掩码
	for _, fresh := range []string{"sk_brand-new-key-0123456789", strings.Repeat("*", 40), "sk_" + strings.Repeat("•", 40)} {
		if code := save(fresh); code != http.StatusOK || state.config.Accounts[0].Key != fresh {
			t.Fatalf("新 key %q 未保存: code=%d key=%q", fresh, code, state.config.Accounts[0].Key)
		}
	}
}

func TestAccountUsageUsesPassWindows(t *testing.T) {
	reset := time.Now().UTC().Add(2 * time.Hour).Format(time.RFC3339)
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		if r.URL.Path != "/users/me/plan/usage-limits" {
			http.NotFound(w, r)
			return
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"data": map[string]any{"limits": []any{
			map[string]any{"type": "five_hour", "percentUsed": 2, "resetsAt": reset},
			map[string]any{"type": "weekly", "percentUsed": 10, "resetsAt": reset},
			map[string]any{"type": "monthly", "percentUsed": 5, "resetsAt": reset},
		}}})
	}))
	defer upstream.Close()
	state := testState(t, upstream.URL)
	got := state.accountUsage(state.config.Accounts[0], true)
	periods, _ := got["periods"].([]map[string]any)
	if got["ok"] != true || got["source"] != "/users/me/plan/usage-limits" || len(periods) != 3 {
		t.Fatalf("pass usage: %#v", got)
	}
	if periods[0]["label"] != "5小时" || periods[0]["percent"] != 2.0 || periods[1]["percent"] != 10.0 || periods[2]["percent"] != 5.0 || periods[0]["resetsAt"] == nil {
		t.Fatalf("windows: %#v", periods)
	}
}

func TestPassResetUsesExhaustedWindow(t *testing.T) {
	now := time.Date(2026, 9, 30, 10, 0, 0, 0, time.UTC)
	payload := map[string]any{"data": map[string]any{"limits": []any{
		map[string]any{"type": "five_hour", "percentUsed": 100, "resetsAt": "2026-09-30T12:00:00Z"},
		map[string]any{"type": "weekly", "percentUsed": 100, "resetsAt": "2026-10-06T00:00:00Z"},
		map[string]any{"type": "monthly", "percentUsed": 5, "resetsAt": "2026-10-29T00:00:00Z"},
	}}}
	got := passResetTime(payload, now)
	if !got.Equal(time.Date(2026, 10, 6, 0, 0, 0, 0, time.UTC)) {
		t.Fatal(got)
	}
	if !passLimited("You have reached your ClinePass limit. Please try again later.") || passLimited("rate limit") {
		t.Fatal("pass limit detection")
	}
}

func TestCooldownSkipsPassAndChannel(t *testing.T) {
	var calls []string
	state := testState(t, "https://upstream.test")
	state.config.AccountMode = "roundrobin"
	state.config.MaxRetries = 1
	state.config.RateLimitSwitchRetries = 1
	state.config.Accounts = []Account{{Name: "a", Key: "key-a", Enabled: true}, {Name: "b", Key: "key-b", Enabled: true}}
	reset := time.Now().Add(2 * time.Hour).UTC().Format(time.RFC3339)
	state.client.Transport = roundTripFunc(func(r *http.Request) (*http.Response, error) {
		calls = append(calls, r.URL.Path+" "+r.Header.Get("Authorization"))
		if strings.Contains(r.URL.Path, "usage-limits") {
			body := `{"data":{"limits":[{"type":"five_hour","percentUsed":100,"resetsAt":"` + reset + `"}]}}`
			return &http.Response{StatusCode: 200, Body: io.NopCloser(strings.NewReader(body)), Header: make(http.Header)}, nil
		}
		if strings.Contains(r.Header.Get("Authorization"), "key-a") {
			return &http.Response{StatusCode: 429, Body: io.NopCloser(strings.NewReader(`{"error":{"message":"You have reached your ClinePass limit. Please try again later."}}`)), Header: make(http.Header)}, nil
		}
		return &http.Response{StatusCode: 200, Body: io.NopCloser(strings.NewReader(`{"choices":[{"message":{"content":"OK"}}]}`)), Header: make(http.Header)}, nil
	})
	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", nil)
	result, err := state.runChatChain(req, map[string]any{"model": "cline-pass/test"}, "cline-pass/test", ModelConfig{}, false, time.Minute)
	if err != nil || result.Status != 200 || result.Account.Name != "b" {
		t.Fatalf("pass switch: err=%v status=%d account=%s calls=%v", err, result.Status, result.Account.Name, calls)
	}
	if !state.blocked(state.config.Accounts[0], "", time.Now()) {
		t.Fatal("account a not cooled")
	}
	for i := 0; i < 4; i++ {
		got, err := state.pickAccount("")
		if err != nil || got.Name != "b" {
			t.Fatalf("pick cooled: %v %#v", err, got)
		}
	}
	before := len(calls)
	state.markCool(true, accountID("key-b"), "", time.Now().Add(time.Hour))
	blocked, err := state.runChatChain(req, map[string]any{"model": "cline-pass/test"}, "cline-pass/test", ModelConfig{}, false, time.Minute)
	if err != nil || blocked.Status != 429 || len(calls) != before {
		t.Fatalf("all cooled: err=%v status=%d calls=%d", err, blocked.Status, len(calls)-before)
	}

	state.config.Accounts = []Account{{Name: "a", Key: "key-a", Enabled: true}}
	state.accountCool = nil
	state.channelCool = nil
	state.metadata.Models["cline-pass/test"] = map[string]any{"upstreams": []any{"alpha", "beta"}, "pipeline": "direct"}
	state.config.MaxRetries = 0
	state.config.RateLimitSwitchRetries = 0
	state.client.Transport = roundTripFunc(func(r *http.Request) (*http.Response, error) {
		calls = append(calls, string(mustRead(r)))
		if strings.Contains(calls[len(calls)-1], "alpha") {
			header := make(http.Header)
			header.Set("Retry-After", "30")
			return &http.Response{StatusCode: 429, Body: io.NopCloser(strings.NewReader(`{"error":{"message":"rate limit"},"provider":"alpha"}`)), Header: header}, nil
		}
		return &http.Response{StatusCode: 200, Body: io.NopCloser(strings.NewReader(`{"choices":[{"message":{"content":"OK"}}]}`)), Header: make(http.Header)}, nil
	})
	before = len(calls)
	result, err = state.runChatChain(req, map[string]any{"model": "cline-pass/test"}, "cline-pass/test", ModelConfig{Upstreams: []string{"alpha", "beta"}, PinMode: "strict"}, false, time.Minute)
	if err != nil || result.Status != 200 || len(calls)-before != 2 {
		t.Fatalf("channel switch: err=%v status=%d new=%d", err, result.Status, len(calls)-before)
	}
	if _, err := state.pickAccount("alpha"); err == nil {
		t.Fatal("alpha still schedulable")
	}
	if state.blocked(state.config.Accounts[0], "beta", time.Now()) {
		t.Fatal("beta should stay open")
	}
}

func mustRead(r *http.Request) []byte {
	data, _ := io.ReadAll(r.Body)
	r.Body = io.NopCloser(bytes.NewReader(data))
	return data
}

func urlValues(key, value string) url.Values {
	result := url.Values{}
	result.Set(key, value)
	return result
}
