package config

import (
	"encoding/json"
	"os"
	"path/filepath"
)

type Config struct {
	Port              int      `json:"port"`
	Host              string   `json:"host"`
	RetryAttempts     int      `json:"retry_attempts"`
	RetryDelaySec     int      `json:"retry_delay_sec"`
	RequestTimeoutSec int      `json:"request_timeout_sec"`
	GeminiBL          string   `json:"gemini_bl"`
	AuthUser          string   `json:"auth_user"`
	XSRFToken         string   `json:"xsrf_token"`
	DefaultModel      string   `json:"default_model"`
	LogRequests       bool     `json:"log_requests"`
	CookieFile        string   `json:"cookie_file"`
	Proxy             string   `json:"proxy"`
	Proxies           []string `json:"proxies"`
	DirectFallback    bool     `json:"direct_fallback"`
	APIKeys           []string `json:"api_keys"`
	Impersonate       string   `json:"impersonate"`
	TemporaryChats    bool     `json:"temporary_chats"`

	// ToolSchemaMaxBytes is the byte budget for the JSON Schema block injected
	// into the prompt by the tool-calling emulation. When the definitions
	// exceed it, cosmetic keywords and deep properties are dropped in stages;
	// the schema shape is never discarded. Zero/negative means the built-in
	// default (30000).
	ToolSchemaMaxBytes int `json:"tool_schema_max_bytes"`
}

func Default() Config {
	return Config{
		Port:               8081,
		Host:               "0.0.0.0",
		RetryAttempts:      3,
		RetryDelaySec:      2,
		RequestTimeoutSec:  180,
		GeminiBL:           "boq_assistant-bard-web-server_20260716.08_p0",
		AuthUser:           "",
		XSRFToken:          "",
		DefaultModel:       "gemini-4-hydrogen",
		LogRequests:        true,
		CookieFile:         "",
		Proxy:              "",
		Proxies:            []string{},
		DirectFallback:     false,
		APIKeys:            []string{},
		Impersonate:        "",
		TemporaryChats:     false,
		ToolSchemaMaxBytes: 30000,
	}
}

func Load(path string) (Config, error) {
	cfg := Default()
	if path == "" {
		return cfg, nil
	}

	data, err := os.ReadFile(path)
	if err != nil {
		return cfg, err
	}

	// Use temporary struct with pointer fields or interface to handle null values from JSON if needed,
	// but standard unmarshal over struct fields handles JSON null for strings by leaving existing or setting zero-value if unmarshaling map/struct.
	// Let's ensure string fields default properly if JSON has explicitly `null`.
	var aux struct {
		Port               *int      `json:"port"`
		Host               *string   `json:"host"`
		RetryAttempts      *int      `json:"retry_attempts"`
		RetryDelaySec      *int      `json:"retry_delay_sec"`
		RequestTimeoutSec  *int      `json:"request_timeout_sec"`
		GeminiBL           *string   `json:"gemini_bl"`
		AuthUser           *string   `json:"auth_user"`
		XSRFToken          *string   `json:"xsrf_token"`
		DefaultModel       *string   `json:"default_model"`
		LogRequests        *bool     `json:"log_requests"`
		CookieFile         *string   `json:"cookie_file"`
		Proxy              *string   `json:"proxy"`
		Proxies            *[]string `json:"proxies"`
		DirectFallback     *bool     `json:"direct_fallback"`
		APIKeys            *[]string `json:"api_keys"`
		Impersonate        *string   `json:"impersonate"`
		TemporaryChats     *bool     `json:"temporary_chats"`
		ToolSchemaMaxBytes *int      `json:"tool_schema_max_bytes"`
	}

	if err := json.Unmarshal(data, &aux); err != nil {
		return cfg, err
	}

	if aux.Port != nil {
		cfg.Port = *aux.Port
	}
	if aux.Host != nil {
		cfg.Host = *aux.Host
	}
	if aux.RetryAttempts != nil {
		cfg.RetryAttempts = *aux.RetryAttempts
	}
	if aux.RetryDelaySec != nil {
		cfg.RetryDelaySec = *aux.RetryDelaySec
	}
	if aux.RequestTimeoutSec != nil {
		cfg.RequestTimeoutSec = *aux.RequestTimeoutSec
	}
	if aux.GeminiBL != nil {
		cfg.GeminiBL = *aux.GeminiBL
	}
	if aux.AuthUser != nil {
		cfg.AuthUser = *aux.AuthUser
	}
	if aux.XSRFToken != nil {
		cfg.XSRFToken = *aux.XSRFToken
	}
	if aux.DefaultModel != nil {
		cfg.DefaultModel = *aux.DefaultModel
	}
	if aux.LogRequests != nil {
		cfg.LogRequests = *aux.LogRequests
	}
	if aux.CookieFile != nil {
		cfg.CookieFile = *aux.CookieFile
	}
	if aux.Proxy != nil {
		cfg.Proxy = *aux.Proxy
	}
	if aux.Proxies != nil {
		cfg.Proxies = *aux.Proxies
	}
	if aux.DirectFallback != nil {
		cfg.DirectFallback = *aux.DirectFallback
	}
	if aux.APIKeys != nil {
		cfg.APIKeys = *aux.APIKeys
	}
	if aux.Impersonate != nil {
		cfg.Impersonate = *aux.Impersonate
	}
	if aux.TemporaryChats != nil {
		cfg.TemporaryChats = *aux.TemporaryChats
	}
	if aux.ToolSchemaMaxBytes != nil {
		cfg.ToolSchemaMaxBytes = *aux.ToolSchemaMaxBytes
	}

	return cfg, nil
}

func Find() string {
	paths := []string{"./config.json"}
	home, err := os.UserHomeDir()
	if err == nil {
		paths = append(paths, filepath.Join(home, ".config", "gemini-web2api", "config.json"))
	}

	for _, p := range paths {
		if _, err := os.Stat(p); err == nil {
			return p
		}
	}
	return ""
}
