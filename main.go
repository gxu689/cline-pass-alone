package main

import (
	"bufio"
	"bytes"
	"context"
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	_ "embed"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"math"
	"net"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

//go:embed public/index.html
var publicIndex []byte

const (
	logRetention          = 48 * time.Hour
	maxLogPageSize        = 100
	maskedAccountKey      = "••••••••"
	defaultListenHost     = "0.0.0.0"
	defaultListenPort     = 3123
	maxRequestBodySize    = 4 << 20
	maxUpstreamBodySize   = 8 << 20
	maxUpstreamStreamSize = 64 << 20
	maxKnownModels        = 1000
	maxPerModelConfigs    = 1000
	maxAccounts           = 200
	maxAuthKeyBytes       = 512
	maxAccountKeyBytes    = 1024
	maxConfigBytes        = 1 << 20
	maxMetadataBytes      = 16 << 20
	maxRequestLogBytes    = 64 << 20
)

var (
	modelIDPattern    = regexp.MustCompile(`^[a-z0-9][a-z0-9._:/+@-]*$`)
	upstreamPattern   = regexp.MustCompile(`^[a-z0-9][a-z0-9-]*$`)
	providerListError = regexp.MustCompile(`Available providers are:\s*([^.]+)`)
	rateLimitPattern  = regexp.MustCompile(`(?i)(\b429\b|rate[ -]?limit|too many requests)`)
)

type Account struct {
	Name    string `json:"name"`
	Key     string `json:"key"`
	Enabled bool   `json:"enabled"`
}

type ModelConfig struct {
	Upstream  string   `json:"upstream,omitempty"`
	Upstreams []string `json:"upstreams,omitempty"`
	Exclude   []string `json:"exclude,omitempty"`
	PinMode   string   `json:"pinMode,omitempty"`
	Sort      string   `json:"sort,omitempty"`
}

type Config struct {
	Port                   int                    `json:"port"`
	AdminKey               string                 `json:"adminKey"`
	ProxyKey               string                 `json:"proxyKey"`
	PublicBaseURL          string                 `json:"publicBaseUrl"`
	ExposeCatalog          bool                   `json:"exposeCatalog"`
	UpstreamBase           string                 `json:"upstreamBase"`
	Accounts               []Account              `json:"accounts"`
	AccountMode            string                 `json:"accountMode"`
	ActiveAccount          int                    `json:"activeAccount"`
	AccountConcurrency     int                    `json:"accountConcurrency"`
	GlobalConcurrency      int                    `json:"globalConcurrency"`
	GlobalRPM              int                    `json:"globalRpm"`
	MaxRetries             int                    `json:"maxRetries"`
	RateLimitSwitchRetries int                    `json:"rateLimitSwitchRetries"`
	RetryIntervalMS        int                    `json:"retryIntervalMs"`
	StreamIdleTimeoutMS    int                    `json:"streamIdleTimeoutMs"`
	TransportRetryStrategy string                 `json:"transportRetryStrategy"`
	KnownModels            []string               `json:"knownModels"`
	PerModel               map[string]ModelConfig `json:"perModel"`
}

type Metadata struct {
	Models              map[string]map[string]any `json:"models"`
	Stats               map[string]map[string]any `json:"stats"`
	AccountUsage        map[string]map[string]any `json:"accountUsage"`
	Catalog             []string                  `json:"catalog,omitempty"`
	CatalogFetchedAt    int64                     `json:"catalogFetchedAt,omitempty"`
	OfficialModelsFetch map[string]any            `json:"officialModelsFetch,omitempty"`
}

type TraceEntry struct {
	Upstream string `json:"upstream,omitempty"`
	Account  string `json:"account,omitempty"`
	Status   int    `json:"status,omitempty"`
	MS       int64  `json:"ms,omitempty"`
	Note     string `json:"note,omitempty"`
}

type LogEntry struct {
	TS           int64        `json:"ts"`
	Model        string       `json:"model,omitempty"`
	Status       int          `json:"status"`
	Provider     string       `json:"provider,omitempty"`
	Canonical    string       `json:"canonical,omitempty"`
	Account      string       `json:"account,omitempty"`
	AccountID    string       `json:"accountId,omitempty"`
	MS           int64        `json:"ms"`
	Stream       bool         `json:"stream,omitempty"`
	InputTokens  float64      `json:"inputTokens"`
	OutputTokens float64      `json:"outputTokens"`
	TotalTokens  float64      `json:"totalTokens"`
	Cost         *float64     `json:"cost,omitempty"`
	Attempts     int          `json:"attempts"`
	Trace        []TraceEntry `json:"trace,omitempty"`
	Error        string       `json:"error,omitempty"`
	Endpoint     string       `json:"endpoint,omitempty"`
	IP           string       `json:"ip,omitempty"`
	UserAgent    string       `json:"userAgent,omitempty"`
}

type RequestLogStore struct {
	mu        sync.Mutex
	file      string
	rows      []LogEntry
	lastPrune time.Time
}

type schedulerState struct {
	active      int
	queued      int
	starts      []time.Time
	byKey       map[string]int
	activeByKey map[string]int
}

type Scheduler struct {
	mu          sync.Mutex
	state       schedulerState
	stateConfig func() (int, int, int)
}

type authFailure struct {
	windowStart time.Time
	lastSeen    time.Time
	count       int
	blockedTill time.Time
}

type AuthLimiter struct {
	mu         sync.Mutex
	entries    map[string]authFailure
	limit      int
	window     time.Duration
	block      time.Duration
	maxEntries int
}

func newAuthLimiter() *AuthLimiter {
	return &AuthLimiter{entries: map[string]authFailure{}, limit: 20, window: time.Minute, block: 5 * time.Minute, maxEntries: 4096}
}

func (limiter *AuthLimiter) allow(key string, now time.Time) bool {
	limiter.mu.Lock()
	defer limiter.mu.Unlock()
	entry, exists := limiter.entries[key]
	if exists && entry.blockedTill.After(now) {
		entry.lastSeen = now
		limiter.entries[key] = entry
		return false
	}
	if !exists || now.Sub(entry.windowStart) >= limiter.window {
		entry = authFailure{windowStart: now}
	}
	entry.lastSeen = now
	limiter.entries[key] = entry
	limiter.cleanupLocked(now)
	return true
}

func (limiter *AuthLimiter) failure(key string, now time.Time) {
	limiter.mu.Lock()
	defer limiter.mu.Unlock()
	entry := limiter.entries[key]
	if entry.windowStart.IsZero() || now.Sub(entry.windowStart) >= limiter.window {
		entry.windowStart = now
		entry.count = 0
	}
	entry.count++
	entry.lastSeen = now
	if entry.count >= limiter.limit {
		entry.blockedTill = now.Add(limiter.block)
	}
	limiter.entries[key] = entry
	limiter.cleanupLocked(now)
}

func (limiter *AuthLimiter) success(key string) {
	limiter.mu.Lock()
	delete(limiter.entries, key)
	limiter.mu.Unlock()
}

func (limiter *AuthLimiter) cleanupLocked(now time.Time) {
	cutoff := now.Add(-limiter.window)
	for key, entry := range limiter.entries {
		if entry.lastSeen.Before(cutoff) {
			delete(limiter.entries, key)
		}
	}
	if len(limiter.entries) <= limiter.maxEntries {
		return
	}
	for key := range limiter.entries {
		delete(limiter.entries, key)
		if len(limiter.entries) <= limiter.maxEntries {
			break
		}
	}
}

type State struct {
	mu           sync.RWMutex
	dataDir      string
	configPath   string
	metadataPath string
	config       Config
	metadata     Metadata
	logs         *RequestLogStore
	scheduler    *Scheduler
	authLimiter  *AuthLimiter
	corsOrigin   string
	rrCounter    atomic.Uint64
	client       *http.Client
}

type apiError struct {
	Status  int
	Message string
}

func (e *apiError) Error() string { return e.Message }

func defaultConfig() Config {
	return Config{
		Port:                   defaultListenPort,
		UpstreamBase:           "https://api.cline.bot/api/v1",
		AccountMode:            "single",
		AccountConcurrency:     1,
		GlobalConcurrency:      3,
		MaxRetries:             2,
		RateLimitSwitchRetries: 1,
		StreamIdleTimeoutMS:    120000,
		TransportRetryStrategy: "switch",
		KnownModels: []string{
			"cline-pass/glm-5.3-flash", "cline-pass/kimi-k3", "cline-pass/deepseek-v4-flash",
			"cline-pass/deepseek-v4.1-flash", "cline-pass/qwen3.8-max", "cline-pass/minimax-m3",
			"cline-pass/glm-5.3", "cline-pass/glm-5.2", "cline-pass/deepseek-v4-pro",
			"cline-pass/mimo-v2.5-pro", "cline-pass/mimo-v2.5", "cline-pass/kimi-k2.6",
			"cline-pass/qwen3.7-plus", "cline-pass/kimi-k2.7-code", "cline-pass/qwen3.7-max",
		},
		PerModel: map[string]ModelConfig{},
	}
}

func loadJSON(path string, target any) error {
	b, err := readLimitedFile(path, maxMetadataBytes)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}
	return json.Unmarshal(b, target)
}

func loadConfig(path string) (Config, map[string]json.RawMessage, error) {
	cfg := defaultConfig()
	raw := map[string]json.RawMessage{}
	b, err := readLimitedFile(path, maxConfigBytes)
	if errors.Is(err, os.ErrNotExist) {
		return cfg, raw, nil
	}
	if err != nil {
		return cfg, raw, err
	}
	if err := json.Unmarshal(b, &raw); err != nil {
		return cfg, raw, err
	}
	if err := json.Unmarshal(b, &cfg); err != nil {
		return cfg, raw, err
	}
	return cfg, raw, nil
}

func readLimitedFile(path string, maxBytes int64) ([]byte, error) {
	file, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer file.Close()
	info, err := file.Stat()
	if err != nil {
		return nil, err
	}
	if info.Size() > maxBytes {
		return nil, fmt.Errorf("%s exceeds the %d-byte limit", path, maxBytes)
	}
	data, err := io.ReadAll(io.LimitReader(file, maxBytes+1))
	if err != nil {
		return nil, err
	}
	if int64(len(data)) > maxBytes {
		return nil, fmt.Errorf("%s exceeds the %d-byte limit", path, maxBytes)
	}
	return data, nil
}

func defaultMetadata() Metadata {
	return Metadata{Models: map[string]map[string]any{}, Stats: map[string]map[string]any{}, AccountUsage: map[string]map[string]any{}}
}

func ensureState(cfg *Config, raw map[string]json.RawMessage) (bool, error) {
	dirty := false
	if cfg.PerModel == nil {
		cfg.PerModel = map[string]ModelConfig{}
		dirty = true
	}
	knownModels := make([]string, 0, minInt(len(cfg.KnownModels), maxKnownModels))
	for _, model := range cfg.KnownModels {
		model = strings.ToLower(strings.TrimSpace(model))
		if validModelID(model) && !contains(knownModels, model) {
			knownModels = append(knownModels, model)
		}
	}
	if len(knownModels) == 0 {
		cfg.KnownModels = defaultConfig().KnownModels
		dirty = true
	} else if len(knownModels) != len(cfg.KnownModels) || len(knownModels) > maxKnownModels {
		cfg.KnownModels = knownModels[:minInt(len(knownModels), maxKnownModels)]
		dirty = true
	} else {
		cfg.KnownModels = knownModels
	}
	if len(cfg.Accounts) > maxAccounts {
		cfg.Accounts = cfg.Accounts[:maxAccounts]
		dirty = true
	}
	if cfg.Port <= 0 {
		cfg.Port = defaultListenPort
		dirty = true
	}
	if cfg.UpstreamBase == "" {
		cfg.UpstreamBase = defaultConfig().UpstreamBase
		dirty = true
	}
	cfg.UpstreamBase = strings.TrimRight(strings.TrimSpace(cfg.UpstreamBase), "/")
	if err := validateUpstreamBase(cfg.UpstreamBase); err != nil {
		return dirty, err
	}
	if cfg.AccountMode != "roundrobin" {
		cfg.AccountMode = "single"
	}
	if cfg.AccountConcurrency < 1 || cfg.AccountConcurrency > 32 {
		cfg.AccountConcurrency = clamp(cfg.AccountConcurrency, 1, 32, 1)
		dirty = true
	}
	cfg.GlobalConcurrency = clamp(cfg.GlobalConcurrency, 1, 128, 3)
	cfg.GlobalRPM = clamp(cfg.GlobalRPM, 0, 60000, 0)
	cfg.MaxRetries = clamp(cfg.MaxRetries, 0, 10, 2)
	cfg.RateLimitSwitchRetries = clamp(cfg.RateLimitSwitchRetries, 0, 10, 1)
	cfg.RetryIntervalMS = clamp(cfg.RetryIntervalMS, 0, 30000, 0)
	cfg.StreamIdleTimeoutMS = clamp(cfg.StreamIdleTimeoutMS, 1000, 600000, 120000)
	if cfg.TransportRetryStrategy != "sticky" {
		cfg.TransportRetryStrategy = "switch"
	}
	cfg.PublicBaseURL = strings.TrimRight(strings.TrimSpace(cfg.PublicBaseURL), "/")
	if value, ok := raw["apiKey"]; ok && len(cfg.Accounts) == 0 {
		var key string
		if json.Unmarshal(value, &key) == nil && strings.TrimSpace(key) != "" {
			cfg.Accounts = []Account{{Name: "默认账号", Key: strings.TrimSpace(key), Enabled: true}}
			dirty = true
		}
	}
	if _, ok := raw["apiKey"]; ok {
		dirty = true
	}
	if accountsValue, ok := raw["accounts"]; ok {
		var rawAccounts []map[string]any
		if json.Unmarshal(accountsValue, &rawAccounts) == nil {
			for index := range cfg.Accounts {
				if index < len(rawAccounts) {
					if _, hasEnabled := rawAccounts[index]["enabled"]; !hasEnabled {
						cfg.Accounts[index].Enabled = true
						dirty = true
					}
				}
			}
		}
	}
	for i := range cfg.Accounts {
		if cfg.Accounts[i].Name == "" {
			cfg.Accounts[i].Name = fmt.Sprintf("账号%d", i+1)
			dirty = true
		}
		cfg.Accounts[i].Name = truncate(cfg.Accounts[i].Name, 50)
		cfg.Accounts[i].Key = strings.TrimSpace(cfg.Accounts[i].Key)
		if len(cfg.Accounts[i].Key) > maxAccountKeyBytes {
			return dirty, fmt.Errorf("account %d key exceeds the %d-byte limit", i+1, maxAccountKeyBytes)
		}
		if err := validateHeaderValue(cfg.Accounts[i].Key); err != nil {
			return dirty, fmt.Errorf("account %d key is invalid: %w", i+1, err)
		}
	}
	if len(cfg.PerModel) > maxPerModelConfigs {
		keys := make([]string, 0, len(cfg.PerModel))
		for model := range cfg.PerModel {
			keys = append(keys, model)
		}
		sort.Strings(keys)
		for _, model := range keys[maxPerModelConfigs:] {
			delete(cfg.PerModel, model)
		}
		dirty = true
	}
	for model, mc := range cfg.PerModel {
		if !validModelID(model) {
			delete(cfg.PerModel, model)
			dirty = true
			continue
		}
		if len(mc.Upstreams) == 0 && validUpstream(mc.Upstream) {
			mc.Upstreams = []string{mc.Upstream}
			dirty = true
		}
		mc.Upstreams = normalizeUpstreams(mc.Upstreams)
		mc.Exclude = normalizeUpstreams(mc.Exclude)
		mc.Upstreams = without(mc.Upstreams, mc.Exclude)
		if mc.PinMode != "preferred" {
			mc.PinMode = "strict"
		}
		if mc.Sort != "cost" && mc.Sort != "ttft" && mc.Sort != "tps" {
			mc.Sort = ""
		}
		cfg.PerModel[model] = mc
	}
	cfg.AdminKey = strings.TrimSpace(cfg.AdminKey)
	cfg.ProxyKey = strings.TrimSpace(cfg.ProxyKey)
	if cfg.AdminKey != "" {
		if err := validateAuthKey(cfg.AdminKey); err != nil {
			return dirty, fmt.Errorf("admin key is invalid: %w", err)
		}
	}
	if cfg.ProxyKey != "" {
		if err := validateAuthKey(cfg.ProxyKey); err != nil {
			return dirty, fmt.Errorf("proxy key is invalid: %w", err)
		}
	}
	if cfg.AdminKey == "" {
		key, err := randomKey()
		if err != nil {
			return dirty, fmt.Errorf("generate admin key: %w", err)
		}
		cfg.AdminKey = key
		dirty = true
		log.Printf("[安全] 已生成控制台管理员密钥（仅此一次显示）：%s", cfg.AdminKey)
	}
	if cfg.ProxyKey == "" {
		key, err := randomKey()
		if err != nil {
			return dirty, fmt.Errorf("generate proxy key: %w", err)
		}
		cfg.ProxyKey = key
		dirty = true
	}
	if cfg.AdminKey == cfg.ProxyKey {
		key, err := randomKey()
		if err != nil {
			return dirty, fmt.Errorf("regenerate proxy key: %w", err)
		}
		cfg.ProxyKey = key
		dirty = true
	}
	return dirty, nil
}

func clamp(value, min, max, _ int) int {
	if value < min {
		return min
	}
	if value > max {
		return max
	}
	return value
}

func randomKey() (string, error) {
	b := make([]byte, 24)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	return base64.RawURLEncoding.EncodeToString(b), nil
}

func validateHeaderValue(value string) error {
	for _, char := range value {
		if char < 0x20 || char == 0x7f {
			return errors.New("contains control characters")
		}
	}
	return nil
}

func validateAuthKey(value string) error {
	if value == "" {
		return errors.New("must not be empty")
	}
	if len(value) > maxAuthKeyBytes {
		return fmt.Errorf("exceeds the %d-byte limit", maxAuthKeyBytes)
	}
	return validateHeaderValue(value)
}

func validateAccountKey(value string) error {
	if value == "" {
		return errors.New("must not be empty")
	}
	if len(value) > maxAccountKeyBytes {
		return fmt.Errorf("exceeds the %d-byte limit", maxAccountKeyBytes)
	}
	return validateHeaderValue(value)
}

func validateUpstreamBase(value string) error {
	parsed, err := url.Parse(value)
	if err != nil {
		return fmt.Errorf("invalid upstream URL: %w", err)
	}
	if parsed.Scheme != "http" && parsed.Scheme != "https" {
		return errors.New("upstream URL must use http or https")
	}
	if parsed.Host == "" {
		return errors.New("upstream URL must include a host")
	}
	if parsed.User != nil {
		return errors.New("upstream URL must not include user credentials")
	}
	if parsed.Fragment != "" {
		return errors.New("upstream URL must not include a fragment")
	}
	return nil
}

func atomicWriteJSON(path string, value any) error {
	b, err := json.MarshalIndent(value, "", "  ")
	if err != nil {
		return err
	}
	return atomicWriteBytes(path, b)
}

func atomicWriteBytes(path string, data []byte) error {
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0700); err != nil {
		return err
	}
	tmp, err := os.CreateTemp(dir, ".codex-pass-*.tmp")
	if err != nil {
		return err
	}
	tmpName := tmp.Name()
	defer os.Remove(tmpName)
	if err := tmp.Chmod(0600); err != nil {
		_ = tmp.Close()
		return err
	}
	if _, err := tmp.Write(data); err != nil {
		_ = tmp.Close()
		return err
	}
	if err := tmp.Sync(); err != nil {
		_ = tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	return os.Rename(tmpName, path)
}

func newState() (*State, error) {
	dataDir := os.Getenv("DATA_DIR")
	if dataDir == "" {
		dataDir, _ = os.Getwd()
	}
	if err := os.MkdirAll(dataDir, 0700); err != nil {
		return nil, err
	}
	if err := os.Chmod(dataDir, 0700); err != nil {
		return nil, err
	}
	transport := &http.Transport{
		Proxy:                 http.ProxyFromEnvironment,
		DialContext:           (&net.Dialer{Timeout: 10 * time.Second, KeepAlive: 30 * time.Second}).DialContext,
		TLSHandshakeTimeout:   10 * time.Second,
		ResponseHeaderTimeout: 30 * time.Second,
		ExpectContinueTimeout: 1 * time.Second,
		MaxIdleConns:          100,
		IdleConnTimeout:       90 * time.Second,
	}
	state := &State{
		dataDir:      dataDir,
		configPath:   filepath.Join(dataDir, "config.json"),
		metadataPath: filepath.Join(dataDir, "metadata.json"),
		client: &http.Client{
			Transport: transport,
			CheckRedirect: func(_ *http.Request, _ []*http.Request) error {
				return http.ErrUseLastResponse
			},
		},
		authLimiter: newAuthLimiter(),
		corsOrigin:  strings.TrimSpace(os.Getenv("CORS_ORIGIN")),
	}
	if state.corsOrigin != "" {
		if err := validateHeaderValue(state.corsOrigin); err != nil {
			return nil, fmt.Errorf("invalid CORS_ORIGIN: %w", err)
		}
	}
	cfg, raw, err := loadConfig(state.configPath)
	if err != nil {
		return nil, fmt.Errorf("load config: %w", err)
	}
	if _, statErr := os.Stat(state.configPath); statErr == nil {
		if chmodErr := os.Chmod(state.configPath, 0600); chmodErr != nil {
			return nil, chmodErr
		}
	}
	state.config = cfg
	if err := loadJSON(state.metadataPath, &state.metadata); err != nil {
		return nil, fmt.Errorf("load metadata: %w", err)
	}
	if _, statErr := os.Stat(state.metadataPath); statErr == nil {
		if chmodErr := os.Chmod(state.metadataPath, 0600); chmodErr != nil {
			return nil, chmodErr
		}
	}
	if state.metadata.Models == nil {
		state.metadata = defaultMetadata()
	}
	if state.metadata.Stats == nil {
		state.metadata.Stats = map[string]map[string]any{}
	}
	if state.metadata.AccountUsage == nil {
		state.metadata.AccountUsage = map[string]map[string]any{}
	}
	cleanMetadata(&state.metadata)
	dirty, err := ensureState(&state.config, raw)
	if err != nil {
		return nil, fmt.Errorf("validate config: %w", err)
	}
	if dirty {
		if err := atomicWriteJSON(state.configPath, state.config); err != nil {
			return nil, err
		}
	}
	if err := atomicWriteJSON(state.metadataPath, state.metadata); err != nil {
		return nil, err
	}
	state.logs, err = newRequestLogStore(filepath.Join(dataDir, "request-logs.jsonl"), time.Now())
	if err != nil {
		return nil, err
	}
	state.scheduler = &Scheduler{stateConfig: func() (int, int, int) {
		state.mu.RLock()
		defer state.mu.RUnlock()
		return state.config.GlobalConcurrency, state.config.AccountConcurrency, state.config.GlobalRPM
	}, state: schedulerState{byKey: map[string]int{}, activeByKey: map[string]int{}}}
	return state, nil
}

func cleanMetadata(metadata *Metadata) {
	for _, model := range metadata.Models {
		for _, key := range []string{"account", "accountId", "trace", "usage", "error", "attempts", "stream", "status", "ms", "ip", "userAgent", "endpoint", "provider", "canonical"} {
			delete(model, key)
		}
	}
}

func (s *State) configSnapshot() Config { s.mu.RLock(); defer s.mu.RUnlock(); return s.config }

func (s *State) enabledAccounts() []Account {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return enabledAccounts(s.config)
}

func enabledAccounts(cfg Config) []Account {
	result := make([]Account, 0, len(cfg.Accounts))
	for _, account := range cfg.Accounts {
		if strings.TrimSpace(account.Key) != "" && account.Enabled {
			result = append(result, account)
		}
	}
	return result
}

func (s *State) pickAccount() (Account, error) {
	s.mu.RLock()
	cfg := s.config
	s.mu.RUnlock()
	accounts := enabledAccounts(cfg)
	if len(accounts) == 0 {
		return Account{}, &apiError{Status: 503, Message: "没有已启用的上游账号"}
	}
	if cfg.AccountMode == "roundrobin" && len(accounts) > 1 {
		index := (s.rrCounter.Add(1) - 1) % uint64(len(accounts))
		return accounts[index], nil
	}
	if cfg.ActiveAccount >= 0 && cfg.ActiveAccount < len(cfg.Accounts) && cfg.Accounts[cfg.ActiveAccount].Enabled && cfg.Accounts[cfg.ActiveAccount].Key != "" {
		return cfg.Accounts[cfg.ActiveAccount], nil
	}
	return accounts[0], nil
}

func (s *State) nextPoolAccount(current Account) (Account, bool) {
	s.mu.RLock()
	cfg := s.config
	s.mu.RUnlock()
	if cfg.AccountMode != "roundrobin" {
		return Account{}, false
	}
	accounts := enabledAccounts(cfg)
	if len(accounts) < 2 {
		return Account{}, false
	}
	for i, account := range accounts {
		if account.Key == current.Key {
			return accounts[(i+1)%len(accounts)], true
		}
	}
	return accounts[0], true
}

func (s *State) acquireSlot(ctx context.Context, accountKey string) (func(), error) {
	return s.scheduler.acquire(ctx, accountKey)
}

func (scheduler *Scheduler) acquire(ctx context.Context, key string) (func(), error) {
	scheduler.mu.Lock()
	scheduler.state.queued++
	scheduler.state.byKey[key]++
	scheduler.mu.Unlock()
	queued := true
	dequeueLocked := func() {
		if !queued {
			return
		}
		scheduler.state.queued--
		scheduler.state.byKey[key]--
		if scheduler.state.byKey[key] <= 0 {
			delete(scheduler.state.byKey, key)
		}
		queued = false
	}
	dequeue := func() {
		scheduler.mu.Lock()
		dequeueLocked()
		scheduler.mu.Unlock()
	}
	for {
		global, perAccount, rpm := scheduler.stateConfig()
		now := time.Now()
		scheduler.mu.Lock()
		for len(scheduler.state.starts) > 0 && now.Sub(scheduler.state.starts[0]) >= time.Minute {
			scheduler.state.starts = scheduler.state.starts[1:]
		}
		if scheduler.state.active < global && (rpm == 0 || len(scheduler.state.starts) < rpm) && scheduler.state.activeByKey[key] < perAccount {
			dequeueLocked()
			scheduler.state.active++
			scheduler.state.activeByKey[key]++
			if rpm > 0 {
				scheduler.state.starts = append(scheduler.state.starts, now)
			}
			scheduler.mu.Unlock()
			var once sync.Once
			return func() {
				once.Do(func() {
					scheduler.mu.Lock()
					scheduler.state.active--
					scheduler.state.activeByKey[key]--
					scheduler.mu.Unlock()
				})
			}, nil
		}
		var wait time.Duration = 25 * time.Millisecond
		if rpm > 0 && len(scheduler.state.starts) >= rpm {
			wait = time.Until(scheduler.state.starts[0].Add(time.Minute))
			if wait < time.Millisecond {
				wait = time.Millisecond
			}
		}
		scheduler.mu.Unlock()
		select {
		case <-ctx.Done():
			dequeue()
			return nil, ctx.Err()
		case <-time.After(wait):
		}
	}
}

func (scheduler *Scheduler) snapshot(key string, limit int) map[string]int {
	scheduler.mu.Lock()
	defer scheduler.mu.Unlock()
	return map[string]int{"active": scheduler.state.activeByKey[key], "queued": scheduler.state.byKey[key], "limit": limit}
}

func (scheduler *Scheduler) totals() (int, int) {
	scheduler.mu.Lock()
	defer scheduler.mu.Unlock()
	return scheduler.state.active, scheduler.state.queued
}

func minInt(a, b int) int {
	if a < b {
		return a
	}
	return b
}

func accountID(key string) string {
	sum := sha256.Sum256([]byte(key))
	return fmt.Sprintf("%x", sum[:])
}
func maskKey(key string) string {
	if key == "" {
		return ""
	}
	return maskedAccountKey
}
func validModelID(value string) bool {
	return len(value) > 0 && len(value) <= 200 && modelIDPattern.MatchString(value) && value != "constructor" && value != "prototype" && value != "__proto__"
}
func validUpstream(value string) bool {
	return len(value) > 0 && len(value) <= 80 && upstreamPattern.MatchString(strings.ToLower(value))
}
func normalizeUpstreams(values []string) []string {
	seen := map[string]bool{}
	result := []string{}
	for _, value := range values {
		value = strings.TrimSpace(value)
		if validUpstream(value) && !seen[value] {
			seen[value] = true
			result = append(result, value)
		}
		if len(result) == 10 {
			break
		}
	}
	return result
}
func without(values, excluded []string) []string {
	set := map[string]bool{}
	for _, value := range excluded {
		set[value] = true
	}
	result := []string{}
	for _, value := range values {
		if !set[value] {
			result = append(result, value)
		}
	}
	return result
}
func truncate(value string, limit int) string {
	r := []rune(value)
	if len(r) > limit {
		return string(r[:limit])
	}
	return value
}
func firstNumber(values ...any) float64 {
	for _, value := range values {
		switch v := value.(type) {
		case float64:
			if !math.IsNaN(v) {
				return v
			}
		case float32:
			return float64(v)
		case int:
			return float64(v)
		case int64:
			return float64(v)
		case json.Number:
			n, _ := v.Float64()
			return n
		case string:
			n, err := strconv.ParseFloat(strings.TrimSpace(v), 64)
			if err == nil {
				return n
			}
		}
	}
	return 0
}
func numberOrNil(values ...any) *float64 {
	for _, value := range values {
		switch v := value.(type) {
		case nil:
			continue
		case int:
			n := float64(v)
			return &n
		case int64:
			n := float64(v)
			return &n
		case int32:
			n := float64(v)
			return &n
		case json.Number:
			n, err := v.Float64()
			if err == nil {
				return &n
			}
		case float64:
			return &v
		case float32:
			n := float64(v)
			return &n
		case string:
			n, err := strconv.ParseFloat(strings.TrimSpace(v), 64)
			if err == nil {
				return &n
			}
		}
	}
	return nil
}
func stringify(value any) string {
	if value == nil {
		return ""
	}
	if text, ok := value.(string); ok {
		return text
	}
	b, _ := json.Marshal(value)
	return string(b)
}
func mapValue(value any) map[string]any { result, _ := value.(map[string]any); return result }
func stringValue(value any) string      { text, _ := value.(string); return text }
func boolValue(value any) bool          { result, _ := value.(bool); return result }
func arrayStrings(value any) []string {
	if values, ok := value.([]string); ok {
		return append([]string{}, values...)
	}
	values, _ := value.([]any)
	result := make([]string, 0, len(values))
	for _, v := range values {
		if text, ok := v.(string); ok {
			result = append(result, text)
		}
	}
	return result
}

func (store *RequestLogStore) flushLocked() error {
	var buf bytes.Buffer
	for _, row := range store.rows {
		b, _ := json.Marshal(row)
		buf.Write(b)
		buf.WriteByte('\n')
	}
	return atomicWriteBytes(store.file, buf.Bytes())
}

func normalizeLog(row map[string]any, now time.Time) LogEntry {
	usage := mapValue(row["usage"])
	input := firstNumber(row["inputTokens"], usage["prompt_tokens"], usage["input_tokens"], usage["inputTokens"])
	output := firstNumber(row["outputTokens"], usage["completion_tokens"], usage["output_tokens"], usage["outputTokens"])
	total := numberOrNil(row["totalTokens"], usage["total_tokens"], usage["totalTokens"])
	if total == nil {
		value := input + output
		total = &value
	}
	status := int(firstNumber(row["status"]))
	if status == 0 {
		if row["error"] != nil {
			status = 502
		} else {
			status = 200
		}
	}
	attempts := int(firstNumber(row["attempts"]))
	if attempts == 0 {
		if traces, ok := row["trace"].([]any); ok {
			attempts = len(traces)
		} else {
			attempts = 1
		}
	}
	trace := []TraceEntry{}
	if traces, ok := row["trace"].([]any); ok {
		for _, raw := range traces {
			if t := mapValue(raw); t != nil {
				trace = append(trace, TraceEntry{Upstream: truncate(stringValue(t["upstream"]), 80), Account: truncate(stringValue(t["account"]), 80), Status: int(firstNumber(t["status"])), MS: int64(firstNumber(t["ms"])), Note: truncate(stringValue(t["note"]), 160)})
			}
		}
	}
	var cost *float64 = numberOrNil(row["cost"], usage["total_cost"], usage["totalCost"], usage["cost"], usage["amount"])
	ts := int64(firstNumber(row["ts"]))
	if ts == 0 {
		ts = now.UnixMilli()
	}
	return LogEntry{TS: ts, Model: truncate(stringValue(row["model"]), 200), Status: status, Provider: truncate(stringValue(row["provider"]), 80), Canonical: truncate(stringValue(row["canonical"]), 200), Account: truncate(stringValue(row["account"]), 80), AccountID: truncate(stringValue(row["accountId"]), 64), MS: maxInt64(0, int64(firstNumber(row["ms"]))), Stream: boolValue(row["stream"]), InputTokens: input, OutputTokens: output, TotalTokens: *total, Cost: cost, Attempts: attempts, Trace: trace, Error: truncate(stringValue(row["error"]), 300), Endpoint: truncate(defaultString(stringValue(row["endpoint"]), "/v1/chat/completions"), 100), IP: truncate(stringValue(row["ip"]), 64), UserAgent: truncate(stringValue(row["userAgent"]), 200)}
}

func defaultString(value, fallback string) string {
	if value == "" {
		return fallback
	}
	return value
}
func maxInt64(a, b int64) int64 {
	if a > b {
		return a
	}
	return b
}

func newRequestLogStore(file string, now time.Time) (*RequestLogStore, error) {
	store := &RequestLogStore{file: file, lastPrune: now, rows: []LogEntry{}}
	if b, err := readLimitedFile(file, maxRequestLogBytes); err == nil {
		scanner := bufio.NewScanner(bytes.NewReader(b))
		for scanner.Scan() {
			var row map[string]any
			if json.Unmarshal(scanner.Bytes(), &row) == nil {
				store.rows = append(store.rows, normalizeLog(row, now))
			}
		}
	} else if !errors.Is(err, os.ErrNotExist) {
		return nil, err
	}
	store.pruneLocked(now)
	if err := store.flushLocked(); err != nil {
		return nil, err
	}
	return store, nil
}

func (store *RequestLogStore) pruneLocked(now time.Time) {
	cutoff := now.Add(-logRetention).UnixMilli()
	filtered := store.rows[:0]
	for _, row := range store.rows {
		if row.TS >= cutoff && row.TS <= now.Add(time.Minute).UnixMilli() {
			filtered = append(filtered, row)
		}
	}
	store.rows = filtered
	store.lastPrune = now
}
func (store *RequestLogStore) prune(now time.Time) {
	store.mu.Lock()
	defer store.mu.Unlock()
	if now.Sub(store.lastPrune) >= time.Minute {
		store.pruneLocked(now)
		_ = store.flushLocked()
	}
}

func (store *RequestLogStore) Append(row map[string]any) LogEntry {
	now := time.Now()
	store.mu.Lock()
	defer store.mu.Unlock()
	if now.Sub(store.lastPrune) >= time.Minute {
		store.pruneLocked(now)
	}
	row["ts"] = now.UnixMilli()
	item := normalizeLog(row, now)
	store.rows = append(store.rows, item)
	b, _ := json.Marshal(item)
	f, err := os.OpenFile(store.file, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0600)
	if err == nil {
		_, _ = f.Write(append(b, '\n'))
		_ = f.Close()
	}
	return item
}

func (store *RequestLogStore) Query(params url.Values) map[string]any {
	now := time.Now()
	store.prune(now)
	store.mu.Lock()
	defer store.mu.Unlock()
	lower := now.Add(-logRetention).UnixMilli()
	upper := now.Add(time.Minute).UnixMilli()
	if value, err := strconv.ParseInt(params.Get("from"), 10, 64); err == nil {
		if value > lower {
			lower = value
		}
	}
	if value, err := strconv.ParseInt(params.Get("to"), 10, 64); err == nil {
		if value < upper {
			upper = value
		}
	}
	status, model, account, endpoint, stream, search := params.Get("status"), params.Get("model"), params.Get("accountId"), params.Get("endpoint"), params.Get("stream"), strings.ToLower(truncate(params.Get("q"), 120))
	if status == "" {
		status = "all"
	}
	if stream == "" {
		stream = "all"
	}
	base := []LogEntry{}
	for _, row := range store.rows {
		if row.TS < lower || row.TS > upper || model != "" && row.Model != model || account != "" && row.AccountID != account || endpoint != "" && row.Endpoint != endpoint || stream == "stream" && !row.Stream || stream == "sync" && row.Stream {
			continue
		}
		if search != "" && !strings.Contains(strings.ToLower(strings.Join([]string{row.Model, row.Account, row.Provider, row.Canonical, row.Error, row.IP, row.UserAgent, row.Endpoint, strconv.Itoa(row.Status)}, " ")), search) {
			continue
		}
		base = append(base, row)
	}
	counts := map[string]int{"all": len(base), "success": 0, "error": 0, "4xx": 0, "5xx": 0, "401": 0, "429": 0, "499": 0, "retries": 0}
	for _, row := range base {
		if row.Status >= 200 && row.Status < 400 {
			counts["success"]++
		} else {
			counts["error"]++
		}
		if row.Status >= 400 && row.Status < 500 {
			counts["4xx"]++
		}
		if row.Status >= 500 {
			counts["5xx"]++
		}
		if row.Status == 401 {
			counts["401"]++
		}
		if row.Status == 429 {
			counts["429"]++
		}
		if row.Status == 499 {
			counts["499"]++
		}
		if row.Attempts > 1 {
			counts["retries"]++
		}
	}
	matches := func(row LogEntry) bool {
		switch status {
		case "all":
			return true
		case "success":
			return row.Status >= 200 && row.Status < 400
		case "error":
			return row.Status >= 400
		case "4xx":
			return row.Status >= 400 && row.Status < 500
		case "5xx":
			return row.Status >= 500
		default:
			return strconv.Itoa(row.Status) == status
		}
	}
	filtered := []LogEntry{}
	for i := len(base) - 1; i >= 0; i-- {
		if matches(base[i]) {
			filtered = append(filtered, base[i])
		}
	}
	pageSize := 50
	if value, err := strconv.Atoi(params.Get("pageSize")); err == nil && value > 0 {
		pageSize = value
	}
	if pageSize > maxLogPageSize {
		pageSize = maxLogPageSize
	}
	page := 1
	if value, err := strconv.Atoi(params.Get("page")); err == nil && value > 0 {
		page = value
	}
	pages := int(math.Max(1, math.Ceil(float64(len(filtered))/float64(pageSize))))
	if page > pages {
		page = pages
	}
	start := (page - 1) * pageSize
	end := start + pageSize
	if start > len(filtered) {
		start = len(filtered)
	}
	if end > len(filtered) {
		end = len(filtered)
	}
	history := filtered[start:end]
	models := map[string]bool{}
	endpoints := map[string]bool{}
	accountMap := map[string]map[string]string{}
	for _, row := range store.rows {
		if row.Model != "" {
			models[row.Model] = true
		}
		if row.Endpoint != "" {
			endpoints[row.Endpoint] = true
		}
		if row.AccountID != "" {
			accountMap[row.AccountID] = map[string]string{"id": row.AccountID, "name": row.Account}
		}
	}
	modelList := mapKeys(models)
	endpointList := mapKeys(endpoints)
	accounts := []map[string]string{}
	for _, value := range accountMap {
		accounts = append(accounts, value)
	}
	sort.Slice(accounts, func(i, j int) bool { return accounts[i]["name"] < accounts[j]["name"] })
	return map[string]any{"history": history, "total": len(filtered), "page": page, "pageSize": pageSize, "pages": pages, "counts": counts, "models": modelList, "accounts": accounts, "endpoints": endpointList, "retentionHours": 48}
}

func mapKeys(values map[string]bool) []string {
	result := []string{}
	for key := range values {
		result = append(result, key)
	}
	sort.Strings(result)
	return result
}

func (store *RequestLogStore) Stats(tzOffset int) map[string]any {
	now := time.Now()
	store.prune(now)
	store.mu.Lock()
	defer store.mu.Unlock()
	offset := time.Duration(clamp(tzOffset, -840, 840, 0)) * time.Minute
	localNow := now.Add(offset)
	dayStart := time.Date(localNow.Year(), localNow.Month(), localNow.Day(), 0, 0, 0, 0, time.UTC).Add(-offset)
	today := []LogEntry{}
	recent := []LogEntry{}
	for _, row := range store.rows {
		at := time.UnixMilli(row.TS)
		if !at.Before(dayStart) {
			today = append(today, row)
		}
		if !at.Before(now.Add(-time.Minute)) {
			recent = append(recent, row)
		}
	}
	totals := func(rows []LogEntry) map[string]any {
		result := map[string]any{"requests": 0, "success": 0, "errors": 0, "inputTokens": 0.0, "outputTokens": 0.0, "tokens": 0.0, "ms": int64(0), "stream": 0, "retries": 0, "cost": 0.0, "costSamples": 0}
		for _, row := range rows {
			result["requests"] = result["requests"].(int) + 1
			if row.Status >= 200 && row.Status < 400 {
				result["success"] = result["success"].(int) + 1
			} else if row.Status >= 400 {
				result["errors"] = result["errors"].(int) + 1
			}
			result["inputTokens"] = result["inputTokens"].(float64) + row.InputTokens
			result["outputTokens"] = result["outputTokens"].(float64) + row.OutputTokens
			result["tokens"] = result["tokens"].(float64) + row.TotalTokens
			result["ms"] = result["ms"].(int64) + row.MS
			if row.Stream {
				result["stream"] = result["stream"].(int) + 1
			}
			if row.Attempts > 1 {
				result["retries"] = result["retries"].(int) + 1
			}
			if row.Cost != nil {
				result["cost"] = result["cost"].(float64) + *row.Cost
				result["costSamples"] = result["costSamples"].(int) + 1
			}
		}
		return result
	}
	day, twoDays, minute := totals(today), totals(store.rows), totals(recent)
	grouped := map[string][]LogEntry{}
	for _, row := range today {
		grouped[row.Model] = append(grouped[row.Model], row)
	}
	models := []map[string]any{}
	for model, rows := range grouped {
		item := totals(rows)
		item["model"] = model
		models = append(models, item)
	}
	sort.Slice(models, func(i, j int) bool { return models[i]["requests"].(int) > models[j]["requests"].(int) })
	requests := day["requests"].(int)
	errorRate := 0.0
	averageMS := int64(0)
	if requests > 0 {
		errorRate = float64(day["errors"].(int)) / float64(requests) * 100
		averageMS = day["ms"].(int64) / int64(requests)
	}
	return map[string]any{"today": day, "twoDays": twoDays, "rpm": minute["requests"], "tpm": minute["tokens"], "errorRate": errorRate, "averageMs": averageMS, "models": models, "updatedAt": now.UnixMilli(), "retentionHours": 48}
}

func (s *State) fetchJSON(ctx context.Context, method, endpoint string, headers map[string]string, body []byte, timeout time.Duration) (int, map[string]any, []byte, error) {
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, method, endpoint, bytes.NewReader(body))
	if err != nil {
		return 0, nil, nil, err
	}
	for key, value := range headers {
		req.Header.Set(key, value)
	}
	response, err := s.client.Do(req)
	if err != nil {
		return 0, nil, nil, err
	}
	defer response.Body.Close()
	data, err := readLimitedBody(response.Body, maxUpstreamBodySize)
	if err != nil {
		return response.StatusCode, nil, data, err
	}
	var payload map[string]any
	if json.Unmarshal(data, &payload) != nil {
		payload = map[string]any{"raw": string(data)}
	}
	return response.StatusCode, payload, data, nil
}

func readLimitedBody(reader io.Reader, maxBytes int64) ([]byte, error) {
	data, err := io.ReadAll(io.LimitReader(reader, maxBytes+1))
	if err != nil {
		return data, err
	}
	if int64(len(data)) > maxBytes {
		return data[:maxBytes], fmt.Errorf("upstream response exceeds the %d-byte limit", maxBytes)
	}
	return data, nil
}

func (s *State) chatHeaders(key string) map[string]string {
	return map[string]string{"Content-Type": "application/json", "Authorization": "Bearer " + key}
}

func parseRouting(payload map[string]any) map[string]any {
	d := payload
	if nested := mapValue(payload["data"]); nested != nil && nested["choices"] != nil {
		d = nested
	}
	choices, _ := d["choices"].([]any)
	var message map[string]any
	if len(choices) > 0 {
		if choice, ok := choices[0].(map[string]any); ok {
			message = mapValue(choice["message"])
		}
	}
	meta := mapValue(d["provider_metadata"])
	if message != nil {
		if value := mapValue(message["provider_metadata"]); value != nil {
			meta = value
		}
	}
	gateway := mapValue(meta["gateway"])
	routing := mapValue(gateway["routing"])
	direct := stringValue(d["provider"])
	final := stringValue(routing["finalProvider"])
	canonical := stringValue(routing["canonicalSlug"])
	if canonical == "" {
		if value := stringValue(d["model"]); strings.Contains(value, "/") {
			canonical = value
		}
	}
	pipeline := ""
	if final != "" {
		pipeline = "planner"
	} else if direct != "" {
		pipeline = "direct"
		final = slugify(direct)
	}
	result := map[string]any{"content": nil, "usage": d["usage"], "pipeline": pipeline, "canonicalSlug": canonical, "finalProvider": final, "finalProviderName": final, "fallbacks": arrayStrings(routing["fallbacksAvailable"]), "plan": stringValue(routing["planningReasoning"])}
	if direct != "" {
		result["finalProviderName"] = direct
	}
	if message != nil {
		result["content"] = message["content"]
	}
	return result
}

func unwrapPayload(payload map[string]any) map[string]any {
	if nested := mapValue(payload["data"]); nested != nil && nested["choices"] != nil {
		return nested
	}
	return payload
}

func slugify(value string) string {
	return strings.ToLower(strings.ReplaceAll(strings.Join(strings.Fields(value), "-"), " ", "-"))
}

func (s *State) modelMeta(model string) map[string]any {
	s.mu.RLock()
	defer s.mu.RUnlock()
	result := map[string]any{}
	for key, value := range s.metadata.Models[model] {
		result[key] = value
	}
	return result
}
func (s *State) saveModelMeta(model string, value map[string]any) {
	s.mu.Lock()
	if s.metadata.Models == nil {
		s.metadata.Models = map[string]map[string]any{}
	}
	s.metadata.Models[model] = value
	_ = atomicWriteJSON(s.metadataPath, s.metadata)
	s.mu.Unlock()
}

func cloneBody(body map[string]any) map[string]any {
	data, _ := json.Marshal(body)
	var cloned map[string]any
	_ = json.Unmarshal(data, &cloned)
	return cloned
}

func (s *State) injectPrefs(body map[string]any, model string, attempt map[string]any) map[string]any {
	b := cloneBody(body)
	upstream := stringValue(attempt["upstream"])
	strict := boolValue(attempt["strict"])
	sortName := stringValue(attempt["sort"])
	orderRest := arrayStrings(attempt["orderRest"])
	exclude := arrayStrings(attempt["excludeList"])
	meta := s.modelMeta(model)
	known := arrayStrings(meta["upstreams"])
	allow := []string{}
	for _, value := range known {
		if !contains(exclude, value) {
			allow = append(allow, value)
		}
	}
	if upstream == "" && sortName == "" && len(allow) == 0 {
		return b
	}
	pipeline := stringValue(meta["pipeline"])
	usePlanner, useOpenRouter := pipeline == "planner" || pipeline == "", pipeline == "direct" || pipeline == ""
	if usePlanner {
		gateway := mapValue(mapValue(b["providerOptions"])["gateway"])
		if gateway == nil {
			gateway = map[string]any{}
		}
		if upstream != "" {
			if strict {
				gateway["only"] = []any{upstream}
			} else {
				gateway["order"] = toAny(append([]string{upstream}, orderRest...))
				if len(allow) > 0 {
					gateway["only"] = toAny(allow)
				}
			}
		} else if len(allow) > 0 {
			gateway["only"] = toAny(allow)
		}
		if sortName != "" {
			gateway["sort"] = sortName
		}
		providerOptions := mapValue(b["providerOptions"])
		if providerOptions == nil {
			providerOptions = map[string]any{}
		}
		providerOptions["gateway"] = gateway
		b["providerOptions"] = providerOptions
	}
	if useOpenRouter {
		provider := mapValue(b["provider"])
		if provider == nil {
			provider = map[string]any{}
		}
		if upstream != "" {
			if strict {
				provider["only"] = []any{upstream}
			} else {
				provider["order"] = toAny(append([]string{upstream}, orderRest...))
				if len(allow) > 0 {
					provider["only"] = toAny(allow)
				}
			}
		} else if len(allow) > 0 {
			provider["only"] = toAny(allow)
		}
		if sortName != "" {
			mapping := map[string]string{"cost": "price", "ttft": "latency", "tps": "throughput"}
			if value, ok := mapping[sortName]; ok {
				sortName = value
			}
			provider["sort"] = sortName
		}
		b["provider"] = provider
	}
	return b
}

func toAny(values []string) []any {
	result := make([]any, len(values))
	for i, value := range values {
		result[i] = value
	}
	return result
}
func contains(values []string, target string) bool {
	for _, value := range values {
		if value == target {
			return true
		}
	}
	return false
}

func buildAttempts(cfg ModelConfig) []map[string]any {
	listed := normalizeUpstreams(cfg.Upstreams)
	excluded := normalizeUpstreams(cfg.Exclude)
	wanted := without(listed, excluded)
	strict := cfg.PinMode != "preferred"
	base := func(upstream string, rest []string) map[string]any {
		return map[string]any{"upstream": upstream, "orderRest": toAny(rest), "excludeList": toAny(excluded), "strict": strict, "sort": cfg.Sort}
	}
	if len(wanted) == 0 {
		return []map[string]any{base("", nil)}
	}
	attempts := []map[string]any{}
	for i, upstream := range wanted {
		rest := []string{}
		if !strict {
			rest = append(rest, wanted[:i]...)
			rest = append(rest, wanted[i+1:]...)
		}
		attempts = append(attempts, base(upstream, rest))
	}
	return attempts
}

func errText(value any) string { return truncate(stringify(value), 500) }
func rateLimited(status int, message string) bool {
	return status == 429 || rateLimitPattern.MatchString(message)
}

type chatResult struct {
	Status   int
	Payload  map[string]any
	Raw      []byte
	Routing  map[string]any
	Account  Account
	Trace    []TraceEntry
	Response *http.Response
	Release  func()
	Cancel   context.CancelFunc
	Started  time.Time
}

func attemptContext(parent context.Context, timeout time.Duration, stream bool) (context.Context, context.CancelFunc, func()) {
	if !stream {
		ctx, cancel := context.WithTimeout(parent, timeout)
		return ctx, cancel, func() {}
	}
	ctx, cancel := context.WithCancel(parent)
	timer := time.AfterFunc(timeout, cancel)
	return ctx, cancel, func() { timer.Stop() }
}

func (s *State) runChatChain(req *http.Request, body map[string]any, model string, cfg ModelConfig, stream bool, timeout time.Duration) (*chatResult, error) {
	attempts := buildAttempts(cfg)
	meta := s.modelMeta(model)
	known := arrayStrings(meta["upstreams"])
	for _, attempt := range attempts {
		excluded := arrayStrings(attempt["excludeList"])
		if len(excluded) > 0 && stringValue(attempt["upstream"]) == "" && len(without(known, excluded)) == 0 {
			return &chatResult{Status: 409, Payload: map[string]any{"error": map[string]any{"message": "无法安全执行渠道排除：请先探测模型并保留至少一个可用渠道", "type": "routing_error"}}, Started: time.Now()}, nil
		}
	}
	started := time.Now()
	var last *chatResult
	for _, attempt := range attempts {
		var account Account
		for retry := 0; retry <= s.configSnapshot().MaxRetries; retry++ {
			rateSwitch := retry > 0 && last != nil && len(last.Trace) > 0 && last.Trace[len(last.Trace)-1].Status == 429
			if retry == 0 || (!rateSwitch && s.configSnapshot().TransportRetryStrategy != "sticky") {
				selected, err := s.pickAccount()
				if err != nil {
					return &chatResult{Status: 503, Payload: map[string]any{"error": map[string]any{"message": err.Error(), "type": "auth_error"}}, Started: started}, nil
				}
				account = selected
			} else if rateSwitch {
				if next, ok := s.nextPoolAccount(account); ok {
					account = next
				}
			}
			ctx, cancel, stopHeader := attemptContext(req.Context(), timeout, stream)
			send := s.injectPrefs(body, model, attempt)
			encoded, _ := json.Marshal(send)
			release, err := s.acquireSlot(ctx, account.Key)
			if err != nil {
				stopHeader()
				cancel()
				if req.Context().Err() != nil {
					return nil, req.Context().Err()
				}
				last = &chatResult{Status: 502, Payload: map[string]any{"error": map[string]any{"message": err.Error(), "type": "upstream_error"}}, Account: account, Started: started}
				continue
			}
			upstreamURL := strings.TrimRight(s.configSnapshot().UpstreamBase, "/") + "/chat/completions"
			upstreamReq, err := http.NewRequestWithContext(ctx, http.MethodPost, upstreamURL, bytes.NewReader(encoded))
			if err != nil {
				stopHeader()
				release()
				cancel()
				last = &chatResult{Status: 502, Payload: map[string]any{"error": map[string]any{"message": err.Error(), "type": "upstream_error"}}, Account: account, Trace: append(lastTrace(last), TraceEntry{Upstream: stringValue(attempt["upstream"]), Account: account.Name, Status: 502, Note: err.Error()}), Started: started}
				break
			}
			for key, value := range s.chatHeaders(account.Key) {
				upstreamReq.Header.Set(key, value)
			}
			response, requestErr := s.client.Do(upstreamReq)
			stopHeader()
			elapsed := time.Since(started)
			if requestErr != nil {
				release()
				cancel()
				trace := TraceEntry{Upstream: stringValue(attempt["upstream"]), Account: account.Name, Status: 502, MS: elapsed.Milliseconds(), Note: requestErr.Error()}
				last = &chatResult{Status: 502, Payload: map[string]any{"error": map[string]any{"message": "upstream fetch failed: " + requestErr.Error(), "type": "upstream_error"}}, Account: account, Trace: append(lastTrace(last), trace), Started: started}
				if s.shouldRetry(retry, 502, requestErr.Error()) {
					s.sleepRetry(req.Context())
					continue
				}
				break
			}
			if stream && response.StatusCode >= 200 && response.StatusCode < 300 && strings.Contains(strings.ToLower(response.Header.Get("Content-Type")), "text/event-stream") {
				if response.ContentLength > maxUpstreamStreamSize {
					response.Body.Close()
					release()
					cancel()
					last = &chatResult{Status: http.StatusBadGateway, Payload: map[string]any{"error": map[string]any{"message": "upstream stream response too large", "type": "upstream_error"}}, Account: account, Trace: append(lastTrace(last), TraceEntry{Upstream: stringValue(attempt["upstream"]), Account: account.Name, Status: http.StatusBadGateway, MS: elapsed.Milliseconds(), Note: "stream response too large"}), Started: started}
					break
				}
				return &chatResult{Status: response.StatusCode, Account: account, Response: response, Release: release, Cancel: cancel, Routing: map[string]any{}, Trace: append(lastTrace(last), TraceEntry{Upstream: stringValue(attempt["upstream"]), Account: account.Name, Status: response.StatusCode, MS: elapsed.Milliseconds()}), Started: started}, nil
			}
			data, readErr := readLimitedBody(response.Body, maxUpstreamBodySize)
			response.Body.Close()
			release()
			cancel()
			payload := map[string]any{}
			if json.Unmarshal(data, &payload) != nil {
				payload = map[string]any{"error": map[string]any{"message": "upstream returned non-JSON", "type": "upstream_error"}}
			}
			payload = unwrapPayload(payload)
			status := response.StatusCode
			if readErr != nil {
				status = http.StatusBadGateway
			}
			routing := parseRouting(payload)
			if status == 200 && rateLimited(status, errText(mapValue(payload["error"])["message"])) {
				status = 429
			}
			if status == 200 && mapValue(payload["error"]) != nil && mapValue(payload["choices"]) == nil && mapValue(payload["data"]) == nil {
				status = 502
			}
			note := ""
			if value := mapValue(payload["error"]); value != nil {
				note = truncate(stringValue(value["message"]), 160)
				if note == "" {
					note = truncate(stringify(value), 160)
				}
			}
			trace := TraceEntry{Upstream: stringValue(attempt["upstream"]), Account: account.Name, Status: status, MS: elapsed.Milliseconds(), Note: note}
			current := &chatResult{Status: status, Payload: payload, Raw: data, Routing: routing, Account: account, Trace: append(lastTrace(last), trace), Started: started}
			last = current
			if status >= 200 && status < 300 && readErr == nil && mapValue(payload["error"]) == nil {
				return current, nil
			}
			if note != "" {
				s.learnUpstreamStatus(model, stringValue(attempt["upstream"]), note)
			}
			if s.shouldRetry(retry, status, note) {
				s.sleepRetry(req.Context())
				continue
			}
			break
		}
	}
	if last == nil {
		last = &chatResult{Status: 502, Payload: map[string]any{"error": map[string]any{"message": "no upstream response", "type": "upstream_error"}}, Started: started}
	}
	return last, nil
}

func lastTrace(result *chatResult) []TraceEntry {
	if result == nil {
		return nil
	}
	return append([]TraceEntry{}, result.Trace...)
}
func (s *State) shouldRetry(retry, status int, message string) bool {
	cfg := s.configSnapshot()
	if retry >= cfg.MaxRetries {
		return false
	}
	if status == 429 {
		return retry < cfg.RateLimitSwitchRetries
	}
	return status >= 500 || status == 0 || strings.Contains(strings.ToLower(message), "timeout")
}
func (s *State) sleepRetry(ctx context.Context) {
	delay := s.configSnapshot().RetryIntervalMS
	if delay <= 0 {
		return
	}
	timer := time.NewTimer(time.Duration(delay) * time.Millisecond)
	defer timer.Stop()
	select {
	case <-ctx.Done():
	case <-timer.C:
	}
}

func providersFromError(message string) []string {
	match := providerListError.FindStringSubmatch(message)
	if match == nil {
		return nil
	}
	seen := map[string]bool{}
	result := []string{}
	for _, part := range strings.Split(match[1], ",") {
		value := strings.ToLower(strings.Trim(strings.TrimSpace(part), "\"'`"))
		if !validUpstream(value) || seen[value] {
			continue
		}
		seen[value] = true
		result = append(result, value)
	}
	return result
}

func (s *State) learnUpstreamStatus(model, upstream, message string) {
	listed := providersFromError(message)
	status := classifyUpstreamError(message)
	if upstream == "" || status == "unknown" {
		status = ""
	}
	if status == "" && len(listed) == 0 {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	meta := s.metadata.Models[model]
	if meta == nil {
		meta = map[string]any{}
		s.metadata.Models[model] = meta
	}
	if status != "" {
		statuses := mapValue(meta["upstreamStatus"])
		if statuses == nil {
			statuses = map[string]any{}
		}
		statuses[upstream] = map[string]any{"status": status, "note": truncate(message, 160), "checkedAt": time.Now().UnixMilli()}
		meta["upstreamStatus"] = statuses
	}
	if len(listed) > 0 {
		meta["upstreams"] = unique(append(arrayStrings(meta["upstreams"]), listed...))
	}
	_ = atomicWriteJSON(s.metadataPath, s.metadata)
}

func (s *State) writeLog(model string, result *chatResult, req *http.Request, status int, stream bool, extra map[string]any) {
	row := map[string]any{"model": model, "status": status, "provider": stringValue(result.Routing["finalProvider"]), "canonical": stringValue(result.Routing["canonicalSlug"]), "ms": time.Since(result.Started).Milliseconds(), "stream": stream, "attempts": len(result.Trace), "trace": traceAsAny(result.Trace), "account": result.Account.Name, "accountId": accountID(result.Account.Key), "endpoint": req.URL.Path, "ip": req.RemoteAddr, "userAgent": req.UserAgent()}
	if value := result.Routing["usage"]; value != nil {
		row["usage"] = value
	}
	for key, value := range extra {
		row[key] = value
	}
	s.logs.Append(row)
	s.mu.Lock()
	if s.metadata.Stats == nil {
		s.metadata.Stats = map[string]map[string]any{}
	}
	stat := s.metadata.Stats[accountID(result.Account.Key)]
	if stat == nil {
		stat = map[string]any{"requests": 0.0, "lastUsed": 0.0, "tokens": 0.0}
		s.metadata.Stats[accountID(result.Account.Key)] = stat
	}
	stat["requests"] = firstNumber(stat["requests"]) + 1
	stat["lastUsed"] = float64(time.Now().UnixMilli())
	if usage := mapValue(result.Routing["usage"]); usage != nil {
		stat["tokens"] = firstNumber(stat["tokens"]) + firstNumber(usage["total_tokens"], usage["totalTokens"])
	}
	stat["lastError"] = extra["error"]
	_ = atomicWriteJSON(s.metadataPath, s.metadata)
	s.mu.Unlock()
}
func traceAsAny(trace []TraceEntry) []any {
	result := make([]any, len(trace))
	for i, value := range trace {
		result[i] = map[string]any{"upstream": value.Upstream, "account": value.Account, "status": value.Status, "ms": value.MS, "note": value.Note}
	}
	return result
}

func (s *State) sendJSON(w http.ResponseWriter, status int, value any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	s.setCORS(w)
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(value)
}
func (s *State) unauthorized(w http.ResponseWriter) {
	s.sendJSON(w, http.StatusUnauthorized, map[string]any{"error": map[string]any{"message": "unauthorized: 密钥缺失或错误", "type": "auth_error"}})
}

func (s *State) setCORS(w http.ResponseWriter) {
	origin := s.corsOrigin
	if origin != "" {
		w.Header().Set("Access-Control-Allow-Origin", origin)
	}
}

func clientAddress(r *http.Request) string {
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err == nil && host != "" {
		return host
	}
	return truncate(r.RemoteAddr, 128)
}
func tokenMatches(actual, expected string) bool {
	if actual == "" || expected == "" || len(actual) != len(expected) {
		return false
	}
	return subtle.ConstantTimeCompare([]byte(actual), []byte(expected)) == 1
}
func (s *State) clientAuth(r *http.Request) bool {
	auth := strings.TrimSpace(r.Header.Get("Authorization"))
	if len(auth) < 7 || !strings.EqualFold(auth[:7], "Bearer ") {
		return false
	}
	s.mu.RLock()
	key := s.config.ProxyKey
	s.mu.RUnlock()
	return tokenMatches(strings.TrimSpace(auth[7:]), key)
}
func (s *State) adminAuth(r *http.Request) bool {
	s.mu.RLock()
	key := s.config.AdminKey
	s.mu.RUnlock()
	return tokenMatches(strings.TrimSpace(r.Header.Get("X-Admin-Key")), key)
}
func (s *State) publicProxyBase() string {
	s.mu.RLock()
	cfg := s.config
	s.mu.RUnlock()
	if cfg.PublicBaseURL != "" {
		return cfg.PublicBaseURL + "/v1"
	}
	return fmt.Sprintf("http://127.0.0.1:%d/v1", cfg.Port)
}

func readBody(r *http.Request) ([]byte, error) {
	return io.ReadAll(io.LimitReader(r.Body, maxRequestBodySize+1))
}
func decodeBody(r *http.Request, target any) error {
	data, err := readBody(r)
	if err != nil {
		return err
	}
	if len(data) > maxRequestBodySize {
		return &apiError{Status: 413, Message: "body too large"}
	}
	if len(bytes.TrimSpace(data)) == 0 {
		return io.EOF
	}
	return json.Unmarshal(data, target)
}

func (s *State) serveStatic(w http.ResponseWriter) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write(publicIndex)
}

func (s *State) handleChat(w http.ResponseWriter, r *http.Request) {
	var body map[string]any
	if err := decodeBody(r, &body); err != nil {
		s.sendError(w, err)
		return
	}
	model := stringValue(body["model"])
	if !validModelID(model) {
		s.sendJSON(w, 400, map[string]any{"error": map[string]any{"message": "invalid model id"}})
		return
	}
	stream := boolValue(body["stream"])
	s.mu.RLock()
	cfg := s.config.PerModel[model]
	timeout := time.Duration(s.config.StreamIdleTimeoutMS) * time.Millisecond
	s.mu.RUnlock()
	result, err := s.runChatChain(r, body, model, cfg, stream, timeout)
	if err != nil {
		s.sendError(w, err)
		return
	}
	if stream && result.Response != nil {
		s.streamResult(w, r, model, result)
		return
	}
	status := result.Status
	if status == 0 {
		status = 502
	}
	extra := map[string]any{}
	if status >= 400 {
		if value := mapValue(result.Payload["error"]); value != nil {
			extra["error"] = stringValue(value["message"])
		}
	}
	s.writeLog(model, result, r, status, false, extra)
	if status == 200 && strings.HasPrefix(model, "cline-pass/") {
		s.mu.Lock()
		if len(s.config.KnownModels) < maxKnownModels && !contains(s.config.KnownModels, model) {
			s.config.KnownModels = append(s.config.KnownModels, model)
			_ = atomicWriteJSON(s.configPath, s.config)
		}
		s.mu.Unlock()
	}
	s.setChatHeaders(w, result, false)
	s.sendJSON(w, status, result.Payload)
}

func (s *State) setChatHeaders(w http.ResponseWriter, result *chatResult, stream bool) {
	s.setCORS(w)
	if stream {
		w.Header().Set("Content-Type", "text/event-stream")
		w.Header().Set("Cache-Control", "no-cache")
		w.Header().Set("Connection", "keep-alive")
	} else {
		w.Header().Set("Content-Type", "application/json")
	}
	targets := []string{}
	for _, trace := range result.Trace {
		if trace.Upstream != "" {
			targets = append(targets, trace.Upstream)
		}
	}
	if len(targets) == 0 {
		targets = append(targets, "auto")
	}
	w.Header().Set("X-Cline-Target-Upstream", headerSafe(strings.Join(targets, ">")))
	w.Header().Set("X-Cline-Actual-Upstream", headerSafe(stringValue(result.Routing["finalProvider"])))
	w.Header().Set("X-Cline-Canonical-Model", headerSafe(stringValue(result.Routing["canonicalSlug"])))
	w.Header().Set("X-Cline-Attempts", strconv.Itoa(len(result.Trace)))
	w.Header().Set("X-Cline-Account", headerSafe(result.Account.Name))
}
func headerSafe(value string) string {
	value = strings.TrimSpace(value)
	result := strings.Map(func(r rune) rune {
		if r >= 0x20 && r <= 0x7e {
			return r
		}
		return -1
	}, value)
	if result == "" {
		return "-"
	}
	return truncate(result, 80)
}

func (s *State) streamResult(w http.ResponseWriter, r *http.Request, model string, result *chatResult) {
	s.setChatHeaders(w, result, true)
	w.WriteHeader(result.Status)
	flusher, _ := w.(http.Flusher)
	if flusher != nil {
		flusher.Flush()
	}
	defer result.Response.Body.Close()
	defer result.Release()
	defer result.Cancel()
	idle := time.NewTimer(time.Duration(s.configSnapshot().StreamIdleTimeoutMS) * time.Millisecond)
	defer idle.Stop()
	resetIdle := func() {
		if !idle.Stop() {
			select {
			case <-idle.C:
			default:
			}
		}
		idle.Reset(time.Duration(s.configSnapshot().StreamIdleTimeoutMS) * time.Millisecond)
	}
	type readResult struct {
		data []byte
		err  error
	}
	readCh := make(chan readResult, 1)
	var tail bytes.Buffer
	var forwarded int64
	streamStatus := 200
	streamError := ""
	for {
		go func() {
			buffer := make([]byte, 32*1024)
			n, err := result.Response.Body.Read(buffer)
			copyData := append([]byte(nil), buffer[:n]...)
			readCh <- readResult{copyData, err}
		}()
		select {
		case <-r.Context().Done():
			streamStatus = 499
			streamError = "client disconnected"
			goto done
		case <-idle.C:
			streamStatus = 504
			streamError = "stream idle timeout"
			goto done
		case read := <-readCh:
			if len(read.data) > 0 {
				if forwarded+int64(len(read.data)) > maxUpstreamStreamSize {
					streamStatus = 502
					streamError = "stream response too large"
					goto done
				}
				_, writeErr := w.Write(read.data)
				if writeErr != nil {
					streamStatus = 499
					streamError = writeErr.Error()
					goto done
				}
				if flusher != nil {
					flusher.Flush()
				}
				tail.Write(read.data)
				forwarded += int64(len(read.data))
				if tail.Len() > 65536 {
					trimmed := tail.Bytes()
					tail.Reset()
					tail.Write(trimmed[len(trimmed)-65536:])
				}
				resetIdle()
			}
			if read.err != nil {
				if read.err != io.EOF {
					streamStatus = 502
					streamError = read.err.Error()
				}
				goto done
			}
		}
	}
done:
	provider, canonical, streamErrorFromEvent, usage := inspectSSE(tail.Bytes())
	if provider != "" {
		result.Routing["finalProvider"] = provider
	}
	if canonical != "" {
		result.Routing["canonicalSlug"] = canonical
	}
	if usage != nil {
		result.Routing["usage"] = usage
	}
	if streamErrorFromEvent != "" {
		streamStatus = 502
		if streamError == "" {
			streamError = streamErrorFromEvent
		}
	}
	extra := map[string]any{}
	if streamError != "" {
		extra["error"] = streamError
	}
	s.writeLog(model, result, r, streamStatus, true, extra)
}

func inspectSSE(data []byte) (provider, canonical, streamError string, usage map[string]any) {
	scanner := bufio.NewScanner(bytes.NewReader(data))
	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		if !strings.HasPrefix(line, "data:") {
			continue
		}
		text := strings.TrimSpace(strings.TrimPrefix(line, "data:"))
		if text == "" || text == "[DONE]" {
			continue
		}
		var event map[string]any
		if json.Unmarshal([]byte(text), &event) != nil {
			continue
		}
		if value := mapValue(event["error"]); value != nil {
			streamError = stringValue(value["message"])
			if streamError == "" {
				streamError = stringify(value)
			}
		}
		if provider == "" {
			provider = stringValue(event["provider"])
			if provider != "" {
				provider = slugify(provider)
			}
		}
		if canonical == "" {
			canonical = stringValue(event["model"])
		}
		if usage == nil {
			usage = mapValue(event["usage"])
		}
		routing := parseRouting(event)
		if provider == "" {
			provider = stringValue(routing["finalProvider"])
		}
		if canonical == "" {
			canonical = stringValue(routing["canonicalSlug"])
		}
	}
	return provider, canonical, streamError, usage
}

func (s *State) sendError(w http.ResponseWriter, err error) {
	status := 500
	if value, ok := err.(*apiError); ok {
		status = value.Status
	}
	s.sendJSON(w, status, map[string]any{"error": map[string]any{"message": err.Error()}})
}

func (s *State) catalog() []string {
	s.mu.RLock()
	if len(s.metadata.Catalog) > 0 && time.Since(time.UnixMilli(s.metadata.CatalogFetchedAt)) < time.Hour {
		result := append([]string{}, s.metadata.Catalog...)
		s.mu.RUnlock()
		return result
	}
	accounts := enabledAccounts(s.config)
	base := s.config.UpstreamBase
	s.mu.RUnlock()
	if len(accounts) == 0 {
		return nil
	}
	status, payload, _, err := s.fetchJSON(context.Background(), http.MethodGet, strings.TrimRight(base, "/")+"/models", s.chatHeaders(accounts[0].Key), nil, 10*time.Second)
	if err != nil || status != 200 {
		return nil
	}
	values, _ := payload["data"].([]any)
	ids := []string{}
	for _, value := range values {
		if model := mapValue(value); validModelID(stringValue(model["id"])) {
			ids = append(ids, stringValue(model["id"]))
		}
	}
	if len(ids) > 0 {
		s.mu.Lock()
		s.metadata.Catalog = ids
		s.metadata.CatalogFetchedAt = time.Now().UnixMilli()
		_ = atomicWriteJSON(s.metadataPath, s.metadata)
		s.mu.Unlock()
	}
	return ids
}

func (s *State) accountUsage(account Account, force bool) map[string]any {
	id := accountID(account.Key)
	s.mu.RLock()
	cached := s.metadata.AccountUsage[id]
	base := s.config.UpstreamBase
	s.mu.RUnlock()
	if !force && cached != nil && time.Since(time.UnixMilli(int64(firstNumber(cached["checkedAt"])))) < 30*time.Second {
		result := cloneMap(cached)
		result["account"] = account.Name
		result["id"] = id
		return result
	}
	status, user, _, err := s.fetchJSON(context.Background(), http.MethodGet, strings.TrimRight(base, "/")+"/users/me", s.chatHeaders(account.Key), nil, 10*time.Second)
	result := map[string]any{"account": account.Name, "id": id, "checkedAt": time.Now().UnixMilli()}
	if err != nil {
		result["ok"] = false
		result["error"] = err.Error()
	} else if status == 401 || status == 403 {
		result["ok"] = false
		result["error"] = "此 API Key 无权读取 Cline 账户资料；需账户登录令牌才能查询官方余额"
	} else {
		userData := mapValue(user["data"])
		if userData == nil {
			userData = user
		}
		uid := stringValue(userData["uid"])
		if uid == "" {
			uid = stringValue(userData["id"])
		}
		if uid == "" {
			result["ok"] = false
			result["error"] = fmt.Sprintf("账户资料接口不可用（HTTP %d）", status)
		} else {
			found := false
			userBase := strings.TrimRight(base, "/") + "/users/" + url.PathEscape(uid)
			targets := []struct{ source, endpoint string }{
				{"/balance", userBase + "/balance"},
				{"/usage", userBase + "/usage"},
			}
			if orgID := activeOrganizationID(userData); orgID != "" {
				targets = append([]struct{ source, endpoint string }{{"/organizations/balance", strings.TrimRight(base, "/") + "/organizations/" + url.PathEscape(orgID) + "/balance"}}, targets...)
			}
			for _, target := range targets {
				code, payload, _, requestErr := s.fetchJSON(context.Background(), http.MethodGet, target.endpoint, s.chatHeaders(account.Key), nil, 10*time.Second)
				if requestErr != nil {
					continue
				}
				periods := normalizeUsagePeriods(payload)
				summary := normalizeUsage(payload, target.source)
				if code == 200 && (len(periods) > 0 || summary != nil) {
					result["ok"] = true
					for key, value := range summary {
						result[key] = value
					}
					result["periods"] = periods
					result["source"] = target.source
					found = true
					break
				}
			}
			if !found {
				result["ok"] = false
				result["error"] = "官方用量接口不可用"
			}
		}
	}
	s.mu.Lock()
	s.metadata.AccountUsage[id] = cloneMap(result)
	_ = atomicWriteJSON(s.metadataPath, s.metadata)
	s.mu.Unlock()
	return result
}

func (s *State) testAccount(key string) map[string]any {
	started := time.Now()
	if err := validateAccountKey(key); err != nil {
		return map[string]any{"ok": false, "ms": 0, "error": "账号 key 无效"}
	}
	cfg := s.configSnapshot()
	model := "cline-pass/glm-5.3-flash"
	if len(cfg.KnownModels) > 0 {
		model = cfg.KnownModels[0]
	}
	body, _ := json.Marshal(map[string]any{
		"model":      model,
		"messages":   []any{map[string]any{"role": "user", "content": "Say OK"}},
		"max_tokens": 512,
	})
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	release, err := s.acquireSlot(ctx, key)
	if err != nil {
		return map[string]any{"ok": false, "ms": time.Since(started).Milliseconds(), "error": err.Error(), "model": model}
	}
	defer release()
	status, payload, _, err := s.fetchJSON(ctx, http.MethodPost, strings.TrimRight(cfg.UpstreamBase, "/")+"/chat/completions", s.chatHeaders(key), body, 2*time.Minute)
	if err != nil {
		return map[string]any{"ok": false, "ms": time.Since(started).Milliseconds(), "error": err.Error(), "model": model}
	}
	if status == http.StatusUnauthorized || status == http.StatusForbidden {
		return map[string]any{"ok": false, "ms": time.Since(started).Milliseconds(), "error": "密钥无效或未授权", "model": model}
	}
	if value := mapValue(payload["error"]); value != nil {
		message := stringValue(value["message"])
		if message == "" {
			message = stringify(value)
		}
		return map[string]any{"ok": status >= 200 && status < 300 && !strings.Contains(strings.ToLower(message), "unauthorized"), "ms": time.Since(started).Milliseconds(), "error": truncate(message, 220), "model": model}
	}
	choices, _ := payload["choices"].([]any)
	if nested := mapValue(payload["data"]); nested != nil {
		choices, _ = nested["choices"].([]any)
	}
	if status < 200 || status >= 300 || len(choices) == 0 {
		return map[string]any{"ok": false, "ms": time.Since(started).Milliseconds(), "error": fmt.Sprintf("网关响应异常（HTTP %d）", status), "model": model}
	}
	return map[string]any{"ok": true, "ms": time.Since(started).Milliseconds(), "model": model}
}

func cloneMap(value map[string]any) map[string]any {
	result := map[string]any{}
	for key, item := range value {
		result[key] = item
	}
	return result
}
func normalizeUsage(value map[string]any, source string) map[string]any {
	root := value
	if data := mapValue(value["data"]); data != nil {
		root = data
	}
	result := map[string]any{}
	for _, key := range []string{"used", "limit", "remaining", "resetAt", "unit"} {
		if item, ok := root[key]; ok {
			result[key] = item
		}
	}
	if _, ok := result["remaining"]; !ok {
		if balance := numberOrNil(root["balance"]); balance != nil {
			result["remaining"] = *balance / 100
			if _, ok := result["unit"]; !ok {
				result["unit"] = "credits"
			}
		}
	}
	if len(result) == 0 {
		return nil
	}
	result["source"] = source
	return result
}
func activeOrganizationID(user map[string]any) string {
	orgs, _ := user["organizations"].([]any)
	for _, raw := range orgs {
		item := mapValue(raw)
		if item == nil || item["active"] != true {
			continue
		}
		if id := stringValue(item["organizationId"]); id != "" {
			return id
		}
		if id := stringValue(item["id"]); id != "" {
			return id
		}
	}
	return ""
}
func normalizeUsagePeriods(value map[string]any) []map[string]any {
	root := value
	if data := mapValue(value["data"]); data != nil {
		root = data
	}
	source := root
	for _, key := range []string{"periods", "windows", "limits", "usage"} {
		if item := mapValue(root[key]); item != nil {
			source = item
			break
		}
	}
	result := []map[string]any{}
	periods := []struct {
		label string
		names []string
	}{
		{"5h", []string{"5h", "fiveHour", "five_hour", "rolling5h", "rolling_5h"}},
		{"7d", []string{"7d", "7day", "sevenDay", "seven_day", "weekly", "week"}},
		{"30d", []string{"30d", "monthly", "month", "rolling30d", "rolling_30d"}},
	}
	for _, period := range periods {
		for _, name := range period.names {
			if raw, ok := source[name]; ok {
				item := mapValue(raw)
				percent := usagePercent(raw)
				if percent != nil {
					row := map[string]any{"label": period.label, "percent": *percent}
					if item != nil {
						row["used"] = item["used"]
						row["limit"] = item["limit"]
						row["unit"] = item["unit"]
						row["resetAt"] = item["resetAt"]
					}
					result = append(result, row)
				}
				break
			}
		}
	}
	return result
}
func usagePercent(value any) *float64 {
	item := mapValue(value)
	if item != nil {
		direct := numberOrNil(item["percent"], item["percentage"], item["usedPercent"], item["usagePercent"], item["utilization"])
		if direct != nil {
			n := *direct
			if n <= 1 {
				n *= 100
			}
			n = math.Max(0, math.Min(100, n))
			return &n
		}
		used, limit := firstNumber(item["used"], item["usedAmount"], item["consumed"], item["usage"], item["value"], item["current"]), firstNumber(item["limit"], item["max"], item["quota"], item["total"], item["capacity"])
		if limit > 0 {
			n := used / limit * 100
			return &n
		}
	}
	return nil
}

func (s *State) probeModel(model string) map[string]any {
	body := map[string]any{"model": model, "messages": []any{map[string]any{"role": "user", "content": "Reply with the word OK"}}, "max_tokens": 256}
	probeRequest, _ := http.NewRequestWithContext(context.Background(), http.MethodPost, "/api/probe", nil)
	result, runErr := s.runChatChain(probeRequest, body, model, ModelConfig{}, false, 3*time.Minute)
	if runErr != nil || result.Status < 200 || result.Status >= 300 {
		if runErr != nil {
			return map[string]any{"ok": false, "error": runErr.Error()}
		}
		return map[string]any{"ok": false, "error": stringify(mapValue(result.Payload["error"]))}
	}
	routing := result.Routing
	meta := s.modelMeta(model)
	meta["ok"], meta["pipeline"], meta["pinnable"], meta["canonicalSlug"], meta["lastProvider"], meta["lastMs"], meta["probedAt"] = true, routing["pipeline"], stringValue(routing["pipeline"]) != "", routing["canonicalSlug"], routing["finalProvider"], time.Since(result.Started).Milliseconds(), time.Now().UnixMilli()
	if len(arrayStrings(routing["fallbacks"])) > 0 {
		meta["upstreams"] = arrayStrings(routing["fallbacks"])
	}
	s.saveModelMeta(model, meta)
	return map[string]any{"ok": true, "ms": time.Since(result.Started).Milliseconds(), "pipeline": routing["pipeline"], "pinnable": routing["pipeline"] != "", "canonicalSlug": routing["canonicalSlug"], "lastProvider": routing["finalProvider"]}
}

func (s *State) validateUpstreams(model string) map[string]map[string]any {
	meta := s.modelMeta(model)
	upstreams := arrayStrings(meta["upstreams"])
	result := map[string]map[string]any{}
	for _, upstream := range upstreams {
		body := map[string]any{"model": model, "messages": []any{map[string]any{"role": "user", "content": "hi"}}, "max_tokens": 16}
		cfg := ModelConfig{Upstreams: []string{upstream}, PinMode: "strict"}
		start := time.Now()
		validateRequest, _ := http.NewRequestWithContext(context.Background(), http.MethodPost, "/api/validate-upstreams", nil)
		response, err := s.runChatChain(validateRequest, body, model, cfg, false, time.Minute)
		status, note := "unknown", ""
		if err != nil {
			note = err.Error()
		} else if response.Status >= 200 && response.Status < 300 {
			status = "ok"
		} else {
			note = stringify(mapValue(response.Payload["error"]))
			status = classifyUpstreamError(note)
		}
		result[upstream] = map[string]any{"status": status, "ms": time.Since(start).Milliseconds(), "note": truncate(note, 160)}
	}
	s.mu.Lock()
	saved := s.metadata.Models[model]
	if saved == nil {
		saved = map[string]any{}
		s.metadata.Models[model] = saved
	}
	statuses := mapValue(saved["upstreamStatus"])
	if statuses == nil {
		statuses = map[string]any{}
	}
	for upstream, item := range result {
		statuses[upstream] = item
	}
	saved["upstreamStatus"] = statuses
	_ = atomicWriteJSON(s.metadataPath, s.metadata)
	s.mu.Unlock()
	return result
}
func classifyUpstreamError(message string) string {
	lower := strings.ToLower(message)
	switch {
	case strings.Contains(lower, "empty response content"):
		return "ok"
	case strings.Contains(lower, "429") || strings.Contains(lower, "rate limit"):
		return "limited"
	case strings.Contains(lower, "invalid_request") || strings.Contains(lower, "modelid") || strings.Contains(lower, "no allowed providers") || strings.Contains(lower, "not found") || strings.Contains(lower, "unsupported"):
		return "bad"
	case strings.Contains(lower, "unauthorized") || strings.Contains(lower, "re-authenticate") || strings.Contains(lower, "401"):
		return "auth"
	default:
		return "unknown"
	}
}

func (s *State) router() http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodOptions {
			s.setCORS(w)
			w.Header().Set("Access-Control-Allow-Methods", "GET,POST,OPTIONS")
			w.Header().Set("Access-Control-Allow-Headers", "Authorization, Content-Type, X-Admin-Key")
			w.WriteHeader(http.StatusNoContent)
			return
		}
		path := r.URL.Path
		if r.Method == http.MethodGet && (path == "/" || path == "/index.html") {
			s.serveStatic(w)
			return
		}
		if r.Method == http.MethodGet && path == "/api/meta" {
			s.mu.RLock()
			configured := len(enabledAccounts(s.config)) > 0
			s.mu.RUnlock()
			s.sendJSON(w, 200, map[string]any{"authRequired": true, "proxyBase": s.publicProxyBase(), "configured": configured})
			return
		}
		clientRoute := path == "/chat/completions" || path == "/v1/chat/completions" || path == "/api/v1/chat/completions" || strings.HasPrefix(path, "/v1/") || path == "/models" || path == "/api/v1/models"
		adminRoute := strings.HasPrefix(path, "/api/") && path != "/api/meta"
		protectedRoute := clientRoute || adminRoute
		limiterKey := clientAddress(r)
		if adminRoute {
			limiterKey = "admin:" + limiterKey
		} else if clientRoute {
			limiterKey = "client:" + limiterKey
		}
		if protectedRoute && s.authLimiter != nil && !s.authLimiter.allow(limiterKey, time.Now()) {
			s.unauthorized(w)
			return
		}
		if clientRoute {
			if !s.clientAuth(r) {
				if s.authLimiter != nil {
					s.authLimiter.failure(limiterKey, time.Now())
				}
				s.unauthorized(w)
				return
			}
			if s.authLimiter != nil {
				s.authLimiter.success(limiterKey)
			}
		}
		if adminRoute {
			if !s.adminAuth(r) {
				if s.authLimiter != nil {
					s.authLimiter.failure(limiterKey, time.Now())
				}
				s.unauthorized(w)
				return
			}
			if s.authLimiter != nil {
				s.authLimiter.success(limiterKey)
			}
		}
		s.routeAuthed(w, r)
	})
}

func (s *State) routeAuthed(w http.ResponseWriter, r *http.Request) {
	path := r.URL.Path
	if r.Method == http.MethodGet && path == "/api/models" {
		cat := s.catalog()
		cfg := s.configSnapshot()
		subscription := []map[string]any{}
		known := map[string]bool{}
		for _, model := range cfg.KnownModels {
			known[model] = true
			subscription = append(subscription, map[string]any{"id": model, "config": cfg.PerModel[model], "meta": s.modelMeta(model)})
		}
		s.mu.RLock()
		ids := make([]string, 0, len(s.metadata.Models))
		for id := range s.metadata.Models {
			if !known[id] {
				ids = append(ids, id)
			}
		}
		official := s.metadata.OfficialModelsFetch
		s.mu.RUnlock()
		metas := map[string]any{}
		for _, id := range ids {
			metas[id] = s.modelMeta(id)
		}
		s.sendJSON(w, 200, map[string]any{"subscription": subscription, "catalogCount": len(cat), "catalog": cat, "meta": metas, "proxyBase": s.publicProxyBase(), "officialFetch": official})
		return
	}
	if r.Method == http.MethodPost && path == "/api/probe" {
		var body struct {
			Model string `json:"model"`
		}
		if err := decodeBody(r, &body); err != nil {
			s.sendError(w, err)
			return
		}
		if !validModelID(body.Model) {
			s.sendJSON(w, 400, map[string]any{"error": "invalid model id"})
			return
		}
		result := s.probeModel(body.Model)
		if !boolValue(result["ok"]) {
			s.sendJSON(w, 502, result)
		} else {
			s.sendJSON(w, 200, result)
		}
		return
	}
	if r.Method == http.MethodPost && path == "/api/test" {
		var body struct {
			Model     string   `json:"model"`
			Upstream  string   `json:"upstream"`
			Upstreams []string `json:"upstreams"`
			Exclude   []string `json:"exclude"`
		}
		if err := decodeBody(r, &body); err != nil {
			s.sendError(w, err)
			return
		}
		if !validModelID(body.Model) {
			s.sendJSON(w, 400, map[string]any{"error": "invalid model id"})
			return
		}
		cfg := s.configSnapshot().PerModel[body.Model]
		if body.Upstreams != nil {
			cfg.Upstreams = normalizeUpstreams(body.Upstreams)
		} else if body.Upstream != "" {
			cfg.Upstreams = normalizeUpstreams([]string{body.Upstream})
		}
		if body.Exclude != nil {
			cfg.Exclude = normalizeUpstreams(body.Exclude)
		}
		result, err := s.runChatChain(r, map[string]any{"model": body.Model, "messages": []any{map[string]any{"role": "user", "content": "Reply with the word OK"}}, "max_tokens": 256}, body.Model, cfg, false, 3*time.Minute)
		if err != nil {
			s.sendError(w, err)
			return
		}
		if result.Status < 200 || result.Status >= 300 {
			s.sendJSON(w, 200, map[string]any{"ok": false, "error": truncate(stringify(mapValue(result.Payload["error"])), 400), "targets": cfg.Upstreams, "exclude": cfg.Exclude, "trace": result.Trace})
			return
		}
		routing := result.Routing
		s.sendJSON(w, 200, map[string]any{"ok": true, "ms": time.Since(result.Started).Milliseconds(), "targets": cfg.Upstreams, "exclude": cfg.Exclude, "actual": routing["finalProvider"], "actualName": routing["finalProviderName"], "pipeline": routing["pipeline"], "pinnable": routing["pipeline"] != "", "canonicalSlug": routing["canonicalSlug"], "fallbacks": routing["fallbacks"], "content": truncate(stringValue(routing["content"]), 120), "account": result.Account.Name, "trace": result.Trace})
		return
	}
	if r.Method == http.MethodGet && path == "/api/accounts" {
		cfg := s.configSnapshot()
		active, queued := s.scheduler.totals()
		accounts := []map[string]any{}
		for _, account := range cfg.Accounts {
			snapshot := s.scheduler.snapshot(account.Key, cfg.AccountConcurrency)
			accounts = append(accounts, map[string]any{"id": accountID(account.Key), "name": account.Name, "key": maskKey(account.Key), "enabled": account.Enabled, "concurrency": snapshot})
		}
		s.mu.RLock()
		stats := s.metadata.Stats
		s.mu.RUnlock()
		s.sendJSON(w, 200, map[string]any{"accounts": accounts, "mode": cfg.AccountMode, "active": cfg.ActiveAccount, "accountConcurrency": cfg.AccountConcurrency, "globalConcurrency": cfg.GlobalConcurrency, "globalRpm": cfg.GlobalRPM, "maxRetries": cfg.MaxRetries, "rateLimitSwitchRetries": cfg.RateLimitSwitchRetries, "retryIntervalMs": cfg.RetryIntervalMS, "transportRetryStrategy": cfg.TransportRetryStrategy, "scheduling": map[string]int{"active": active, "queued": queued}, "stats": stats})
		return
	}
	if r.Method == http.MethodPost && path == "/api/accounts/test" {
		var body map[string]any
		if err := decodeBody(r, &body); err != nil {
			s.sendError(w, err)
			return
		}
		key := strings.TrimSpace(stringValue(body["key"]))
		if key == maskedAccountKey {
			cfg := s.configSnapshot()
			for _, account := range cfg.Accounts {
				if accountID(account.Key) == stringValue(body["id"]) {
					key = account.Key
					break
				}
			}
		}
		if key == "" || key == maskedAccountKey {
			s.sendJSON(w, 400, map[string]any{"error": map[string]any{"message": "key required"}})
			return
		}
		s.sendJSON(w, 200, s.testAccount(key))
		return
	}
	if r.Method == http.MethodPost && path == "/api/accounts/test-batch" {
		var body struct {
			Accounts []map[string]any `json:"accounts"`
		}
		if err := decodeBody(r, &body); err != nil {
			s.sendError(w, err)
			return
		}
		if len(body.Accounts) == 0 || len(body.Accounts) > 50 {
			s.sendJSON(w, 400, map[string]any{"error": map[string]any{"message": "一次最多测试 50 个账号"}})
			return
		}
		cfg := s.configSnapshot()
		results := make([]map[string]any, 0, len(body.Accounts))
		var wg sync.WaitGroup
		var resultsMu sync.Mutex
		for index, raw := range body.Accounts {
			key := strings.TrimSpace(stringValue(raw["key"]))
			if key == maskedAccountKey {
				for _, account := range cfg.Accounts {
					if accountID(account.Key) == stringValue(raw["id"]) {
						key = account.Key
						break
					}
				}
			}
			if key == "" || key == maskedAccountKey {
				continue
			}
			if err := validateAccountKey(key); err != nil {
				s.sendJSON(w, 400, map[string]any{"error": map[string]any{"message": "账号 key 无效"}})
				return
			}
			wg.Add(1)
			go func(index int, key string) {
				defer wg.Done()
				item := s.testAccount(key)
				item["index"] = index
				resultsMu.Lock()
				results = append(results, item)
				resultsMu.Unlock()
			}(index, key)
		}
		wg.Wait()
		sort.Slice(results, func(i, j int) bool {
			return int(firstNumber(results[i]["index"])) < int(firstNumber(results[j]["index"]))
		})
		s.sendJSON(w, 200, map[string]any{"ok": true, "results": results})
		return
	}
	if r.Method == http.MethodGet && path == "/api/accounts/usage" {
		id, force := r.URL.Query().Get("id"), r.URL.Query().Get("refresh") == "1"
		cfg := s.configSnapshot()
		accounts := []Account{}
		for _, account := range cfg.Accounts {
			if id == "" || accountID(account.Key) == id {
				accounts = append(accounts, account)
			}
		}
		if id != "" && len(accounts) == 0 {
			s.sendJSON(w, 404, map[string]any{"error": map[string]any{"message": "account not found"}})
			return
		}
		usage := make([]map[string]any, len(accounts))
		var wg sync.WaitGroup
		for i, account := range accounts {
			wg.Add(1)
			go func(index int, item Account) { defer wg.Done(); usage[index] = s.accountUsage(item, force) }(i, account)
		}
		wg.Wait()
		s.sendJSON(w, 200, map[string]any{"ok": true, "usage": usage, "checkedAt": time.Now().UnixMilli()})
		return
	}
	if r.Method == http.MethodPost && path == "/api/accounts" {
		var body struct {
			Accounts               []map[string]any `json:"accounts"`
			Mode                   string           `json:"mode"`
			Active                 int              `json:"active"`
			AccountConcurrency     *int             `json:"accountConcurrency"`
			GlobalConcurrency      *int             `json:"globalConcurrency"`
			GlobalRPM              *int             `json:"globalRpm"`
			MaxRetries             *int             `json:"maxRetries"`
			RateLimitSwitchRetries *int             `json:"rateLimitSwitchRetries"`
			RetryIntervalMS        *int             `json:"retryIntervalMs"`
			TransportRetryStrategy string           `json:"transportRetryStrategy"`
		}
		if err := decodeBody(r, &body); err != nil {
			s.sendError(w, err)
			return
		}
		if len(body.Accounts) == 0 || len(body.Accounts) > 200 {
			s.sendJSON(w, 400, map[string]any{"error": map[string]any{"message": "账号数量必须在 1–200 之间"}})
			return
		}
		cfg := s.configSnapshot()
		accounts := []Account{}
		for i, raw := range body.Accounts {
			key := strings.TrimSpace(stringValue(raw["key"]))
			id := stringValue(raw["id"])
			if key == maskedAccountKey {
				for _, old := range cfg.Accounts {
					if accountID(old.Key) == id {
						key = old.Key
						break
					}
				}
				if key == maskedAccountKey {
					s.sendJSON(w, 409, map[string]any{"error": map[string]any{"message": "账号列表已变化，请刷新页面后再保存"}})
					return
				}
			}
			if key == "" {
				continue
			}
			if err := validateAccountKey(key); err != nil {
				s.sendJSON(w, 400, map[string]any{"error": map[string]any{"message": "账号 key 无效"}})
				return
			}
			enabled := true
			if value, ok := raw["enabled"].(bool); ok {
				enabled = value
			}
			name := stringValue(raw["name"])
			if name == "" {
				name = fmt.Sprintf("账号%d", i+1)
			}
			accounts = append(accounts, Account{Name: truncate(name, 50), Key: truncate(key, 1024), Enabled: enabled})
		}
		if len(accounts) == 0 {
			s.sendJSON(w, 400, map[string]any{"error": map[string]any{"message": "至少需要一个有效账号（key 非空）"}})
			return
		}
		cfg.Accounts, cfg.AccountMode, cfg.ActiveAccount = accounts, "single", body.Active
		if body.Mode == "roundrobin" {
			cfg.AccountMode = body.Mode
		}
		if cfg.ActiveAccount < 0 {
			cfg.ActiveAccount = 0
		}
		if cfg.ActiveAccount >= len(accounts) {
			cfg.ActiveAccount = len(accounts) - 1
		}
		if body.AccountConcurrency != nil {
			cfg.AccountConcurrency = clamp(*body.AccountConcurrency, 1, 32, 1)
		}
		if body.GlobalConcurrency != nil {
			cfg.GlobalConcurrency = clamp(*body.GlobalConcurrency, 1, 128, 3)
		}
		if body.GlobalRPM != nil {
			cfg.GlobalRPM = clamp(*body.GlobalRPM, 0, 60000, 0)
		}
		if body.MaxRetries != nil {
			cfg.MaxRetries = clamp(*body.MaxRetries, 0, 10, 2)
		}
		if body.RateLimitSwitchRetries != nil {
			cfg.RateLimitSwitchRetries = clamp(*body.RateLimitSwitchRetries, 0, 10, 1)
		}
		if body.RetryIntervalMS != nil {
			cfg.RetryIntervalMS = clamp(*body.RetryIntervalMS, 0, 30000, 0)
		}
		if body.TransportRetryStrategy == "sticky" {
			cfg.TransportRetryStrategy = "sticky"
		} else if body.TransportRetryStrategy != "" {
			cfg.TransportRetryStrategy = "switch"
		}
		s.mu.Lock()
		s.config = cfg
		_ = atomicWriteJSON(s.configPath, s.config)
		s.mu.Unlock()
		s.sendJSON(w, 200, map[string]any{"ok": true, "accounts": len(accounts), "mode": cfg.AccountMode, "active": cfg.ActiveAccount, "accountConcurrency": cfg.AccountConcurrency, "globalConcurrency": cfg.GlobalConcurrency, "globalRpm": cfg.GlobalRPM})
		return
	}
	if r.Method == http.MethodGet && path == "/api/security" {
		cfg := s.configSnapshot()
		s.sendJSON(w, 200, map[string]any{"adminKey": cfg.AdminKey, "proxyKey": cfg.ProxyKey, "publicBaseUrl": cfg.PublicBaseURL, "authRequired": true, "exposeCatalog": cfg.ExposeCatalog})
		return
	}
	if r.Method == http.MethodPost && path == "/api/security" {
		var body map[string]any
		if err := decodeBody(r, &body); err != nil {
			s.sendError(w, err)
			return
		}
		cfg := s.configSnapshot()
		nextAdmin, nextProxy := cfg.AdminKey, cfg.ProxyKey
		if value, ok := body["adminKey"]; ok {
			nextAdmin = strings.TrimSpace(stringValue(value))
		}
		if value, ok := body["proxyKey"]; ok {
			nextProxy = strings.TrimSpace(stringValue(value))
		}
		if nextAdmin == "" || nextProxy == "" {
			s.sendJSON(w, 400, map[string]any{"error": map[string]any{"message": "控制台密钥和客户端 API Key 均不能为空"}})
			return
		}
		if nextAdmin == nextProxy {
			s.sendJSON(w, 400, map[string]any{"error": map[string]any{"message": "控制台密钥与客户端 API Key 必须不同"}})
			return
		}
		if err := validateAuthKey(nextAdmin); err != nil {
			s.sendJSON(w, 400, map[string]any{"error": map[string]any{"message": "控制台密钥格式无效"}})
			return
		}
		if err := validateAuthKey(nextProxy); err != nil {
			s.sendJSON(w, 400, map[string]any{"error": map[string]any{"message": "客户端 API Key 格式无效"}})
			return
		}
		cfg.AdminKey, cfg.ProxyKey = nextAdmin, nextProxy
		if value, ok := body["publicBaseUrl"]; ok {
			cfg.PublicBaseURL = strings.TrimRight(strings.TrimSpace(stringValue(value)), "/")
		}
		if value, ok := body["exposeCatalog"]; ok {
			cfg.ExposeCatalog = boolValue(value)
		}
		s.mu.Lock()
		s.config = cfg
		_ = atomicWriteJSON(s.configPath, cfg)
		s.mu.Unlock()
		s.sendJSON(w, 200, map[string]any{"ok": true, "adminKey": cfg.AdminKey, "proxyKey": cfg.ProxyKey, "publicBaseUrl": cfg.PublicBaseURL, "authRequired": true, "proxyBase": s.publicProxyBase(), "exposeCatalog": cfg.ExposeCatalog})
		return
	}
	if r.Method == http.MethodPost && path == "/api/validate-upstreams" {
		var body struct {
			Model string `json:"model"`
		}
		if err := decodeBody(r, &body); err != nil {
			s.sendError(w, err)
			return
		}
		if !validModelID(body.Model) {
			s.sendJSON(w, 400, map[string]any{"error": map[string]any{"message": "invalid model id"}})
			return
		}
		results := s.validateUpstreams(body.Model)
		summary := map[string]int{"ok": 0, "limited": 0, "bad": 0, "auth": 0, "unknown": 0}
		for _, item := range results {
			summary[stringValue(item["status"])]++
		}
		s.sendJSON(w, 200, map[string]any{"ok": true, "summary": summary, "results": results, "upstreams": arrayStrings(s.modelMeta(body.Model)["upstreams"])})
		return
	}
	if r.Method == http.MethodPost && path == "/api/fetch-official-models" {
		result := s.fetchOfficialModels()
		s.sendJSON(w, 200, result)
		return
	}
	if r.Method == http.MethodGet && path == "/api/history" {
		status := r.URL.Query().Get("status")
		if status == "" {
			status = "all"
		}
		if !contains([]string{"all", "success", "error", "4xx", "5xx", "401", "429", "499", "500"}, status) {
			s.sendJSON(w, 400, map[string]any{"error": map[string]any{"message": "invalid status filter"}})
			return
		}
		stream := r.URL.Query().Get("stream")
		if stream == "" {
			stream = "all"
		}
		if !contains([]string{"all", "stream", "sync"}, stream) {
			s.sendJSON(w, 400, map[string]any{"error": map[string]any{"message": "invalid stream filter"}})
			return
		}
		s.sendJSON(w, 200, s.logs.Query(r.URL.Query()))
		return
	}
	if r.Method == http.MethodGet && path == "/api/stats" {
		offset, _ := strconv.Atoi(r.URL.Query().Get("tzOffset"))
		s.sendJSON(w, 200, s.logs.Stats(offset))
		return
	}
	if r.Method == http.MethodGet && path == "/api/config" {
		cfg := s.configSnapshot()
		s.sendJSON(w, 200, map[string]any{"port": cfg.Port, "perModel": cfg.PerModel, "knownModels": cfg.KnownModels})
		return
	}
	if r.Method == http.MethodPost && path == "/api/config" {
		var body struct {
			PerModel map[string]ModelConfig `json:"perModel"`
		}
		if err := decodeBody(r, &body); err != nil {
			s.sendError(w, err)
			return
		}
		if len(body.PerModel) > maxPerModelConfigs {
			s.sendJSON(w, http.StatusRequestEntityTooLarge, map[string]any{"error": map[string]any{"message": "模型配置数量超过上限"}})
			return
		}
		cfg := s.configSnapshot()
		for model, value := range body.PerModel {
			if !validModelID(model) {
				s.sendJSON(w, 400, map[string]any{"error": map[string]any{"message": "invalid model config"}})
				return
			}
			value.Upstreams = without(normalizeUpstreams(value.Upstreams), normalizeUpstreams(value.Exclude))
			value.Exclude = normalizeUpstreams(value.Exclude)
			if value.PinMode != "preferred" {
				value.PinMode = "strict"
			}
			if value.Sort != "cost" && value.Sort != "ttft" && value.Sort != "tps" {
				value.Sort = ""
			}
			if len(value.Upstreams) > 0 {
				value.Upstream = value.Upstreams[0]
			} else {
				value.Upstream = ""
			}
			cfg.PerModel[model] = value
		}
		if len(cfg.PerModel) > maxPerModelConfigs {
			s.sendJSON(w, http.StatusRequestEntityTooLarge, map[string]any{"error": map[string]any{"message": "模型配置数量超过上限"}})
			return
		}
		s.mu.Lock()
		s.config = cfg
		_ = atomicWriteJSON(s.configPath, cfg)
		s.mu.Unlock()
		s.sendJSON(w, 200, map[string]any{"ok": true})
		return
	}
	if r.Method == http.MethodGet && (path == "/v1/models" || path == "/api/v1/models" || path == "/models") {
		cfg := s.configSnapshot()
		ids := append([]string{}, cfg.KnownModels...)
		if cfg.ExposeCatalog {
			ids = append(ids, s.catalog()...)
		}
		ids = unique(ids)
		data := []map[string]string{}
		for _, id := range ids {
			data = append(data, map[string]string{"id": id, "object": "model"})
		}
		s.sendJSON(w, 200, map[string]any{"object": "list", "data": data})
		return
	}
	if (path == "/chat/completions" || path == "/v1/chat/completions" || path == "/api/v1/chat/completions") && r.Method == http.MethodPost {
		s.handleChat(w, r)
		return
	}
	s.sendJSON(w, 404, map[string]any{"error": map[string]any{"message": "no route: " + r.Method + " " + path}})
}

func unique(values []string) []string {
	seen := map[string]bool{}
	result := []string{}
	for _, value := range values {
		if value != "" && !seen[value] {
			seen[value] = true
			result = append(result, value)
		}
	}
	return result
}

func (s *State) fetchOfficialModels() map[string]any {
	cfg := s.configSnapshot()
	found := map[string]bool{}
	sources := []string{}
	for _, item := range []struct{ URL, Source string }{{"https://api.cline.bot/api/v1/ai/cline/recommended-models", "cline.api"}, {"https://models.dev/api.json", "models.dev"}} {
		status, payload, raw, err := s.fetchJSON(context.Background(), http.MethodGet, item.URL, nil, nil, 30*time.Second)
		if err != nil || status != 200 {
			continue
		}
		queue := []any{payload}
		for visited := 0; len(queue) > 0 && visited < 100000; visited++ {
			value := queue[0]
			queue = queue[1:]
			switch v := value.(type) {
			case string:
				lower := strings.ToLower(strings.TrimSpace(v))
				if strings.HasPrefix(lower, "cline-pass/") && validModelID(lower) {
					found[lower] = true
				}
			case []any:
				if len(queue)+len(v) <= 100000 {
					queue = append(queue, v...)
				}
			case map[string]any:
				if len(queue)+len(v) <= 100000 {
					for key, child := range v {
						if strings.HasPrefix(strings.ToLower(key), "cline-pass/") {
							found[strings.ToLower(key)] = true
						}
						queue = append(queue, child)
					}
				}
			}
		}
		if len(raw) > 0 {
			sources = append(sources, item.Source)
		}
	}
	added := []string{}
	for model := range found {
		if !contains(cfg.KnownModels, model) {
			added = append(added, model)
		}
	}
	sort.Strings(added)
	if len(added) > maxKnownModels-len(cfg.KnownModels) {
		added = added[:maxKnownModels-len(cfg.KnownModels)]
	}
	cfg.KnownModels = unique(append(cfg.KnownModels, added...))
	s.mu.Lock()
	s.config = cfg
	s.metadata.OfficialModelsFetch = map[string]any{"ts": time.Now().UnixMilli(), "sources": sources, "found": len(found), "added": added, "total": len(cfg.KnownModels)}
	_ = atomicWriteJSON(s.configPath, cfg)
	_ = atomicWriteJSON(s.metadataPath, s.metadata)
	s.mu.Unlock()
	return map[string]any{"ok": true, "sources": sources, "found": len(found), "added": added, "knownModels": cfg.KnownModels, "total": len(cfg.KnownModels)}
}

func main() {
	state, err := newState()
	if err != nil {
		log.Fatal(err)
	}
	if len(state.enabledAccounts()) == 0 {
		log.Print("[提示] 尚未配置上游 API Key：打开控制台「账号管理」添加账号并保存即可；服务已启动。")
	}
	host := os.Getenv("BIND_HOST")
	if host == "" {
		host = defaultListenHost
	}
	if value := os.Getenv("PORT"); value != "" {
		if port, parseErr := strconv.Atoi(value); parseErr == nil && port > 0 {
			state.mu.Lock()
			state.config.Port = port
			state.mu.Unlock()
		}
	}
	if value := os.Getenv("CLINE_PASS_KEY"); strings.TrimSpace(value) != "" {
		state.mu.Lock()
		key := strings.TrimSpace(value)
		if err := validateAccountKey(key); err != nil {
			state.mu.Unlock()
			log.Fatal("CLINE_PASS_KEY 无效: ", err)
		}
		exists := false
		for _, account := range state.config.Accounts {
			if account.Key == key {
				exists = true
			}
		}
		if !exists {
			state.config.Accounts = append([]Account{{Name: "env-account", Key: key, Enabled: true}}, state.config.Accounts...)
			if len(state.config.Accounts) > maxAccounts {
				state.config.Accounts = state.config.Accounts[:maxAccounts]
			}
		}
		state.mu.Unlock()
	}
	if value := os.Getenv("PROXY_KEY"); strings.TrimSpace(value) != "" {
		key := strings.TrimSpace(value)
		if err := validateAuthKey(key); err != nil {
			log.Fatal("PROXY_KEY 无效: ", err)
		}
		state.mu.Lock()
		state.config.ProxyKey = key
		state.mu.Unlock()
	}
	if value := os.Getenv("ADMIN_KEY"); strings.TrimSpace(value) != "" {
		key := strings.TrimSpace(value)
		if err := validateAuthKey(key); err != nil {
			log.Fatal("ADMIN_KEY 无效: ", err)
		}
		state.mu.Lock()
		state.config.AdminKey = key
		state.mu.Unlock()
	}
	if value := os.Getenv("PUBLIC_BASE_URL"); value != "" {
		state.mu.Lock()
		state.config.PublicBaseURL = strings.TrimRight(strings.TrimSpace(value), "/")
		state.mu.Unlock()
	}
	finalConfig := state.configSnapshot()
	if finalConfig.AdminKey == finalConfig.ProxyKey {
		log.Fatal("ADMIN_KEY 与 PROXY_KEY 必须不同")
	}
	address := host + ":" + strconv.Itoa(state.configSnapshot().Port)
	server := &http.Server{Addr: address, Handler: state.router(), ReadHeaderTimeout: 10 * time.Second, ReadTimeout: 5 * time.Minute, WriteTimeout: 0, IdleTimeout: 2 * time.Minute, MaxHeaderBytes: 1 << 20}
	log.Printf("Cline Pass Go 控制台: http://%s/", address)
	log.Printf("OpenAI 兼容代理地址: http://%s/v1", address)
	if err := server.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
		log.Fatal(err)
	}
}
