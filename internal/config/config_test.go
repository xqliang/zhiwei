package config

import (
	"os"
	"testing"
)

func TestLoadDefaults(t *testing.T) {
	t.Setenv("ARK_API_KEY", "test-key")
	// 其余走默认值
	c, err := Load()
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if c.Port != "8080" {
		t.Errorf("Port = %s, want 8080", c.Port)
	}
	if c.LLMFastModel != "doubao-seed-1-6-flash-250828" {
		t.Errorf("LLMFastModel = %s", c.LLMFastModel)
	}
	if c.ASRModel != "doubao-seed-asr-2-0" {
		t.Errorf("ASRModel = %s", c.ASRModel)
	}
	if c.ExtractWindow != 10 {
		t.Errorf("ExtractWindow = %d, want 10", c.ExtractWindow)
	}
	if c.QualityMinConf != 0.6 {
		t.Errorf("QualityMinConf = %v, want 0.6", c.QualityMinConf)
	}
	if c.QualityTodoConf != 0.85 {
		t.Errorf("QualityTodoConf = %v, want 0.85", c.QualityTodoConf)
	}
}

func TestLoadRequiresARKKey(t *testing.T) {
	t.Setenv("ARK_API_KEY", "")
	if _, err := Load(); err == nil {
		t.Fatal("want error when ARK_API_KEY empty")
	}
}

func TestLoadExtractOverrides(t *testing.T) {
	t.Setenv("ARK_API_KEY", "test-key")
	t.Setenv("ZW_EXTRACT_WINDOW", "5")
	t.Setenv("ZW_QUALITY_MIN_CONF", "0.7")
	t.Setenv("ZW_QUALITY_TODO_CONF", "0.9")
	c, err := Load()
	if err != nil {
		t.Fatal(err)
	}
	if c.ExtractWindow != 5 || c.QualityMinConf != 0.7 || c.QualityTodoConf != 0.9 {
		t.Fatalf("got window=%d minConf=%v todoConf=%v", c.ExtractWindow, c.QualityMinConf, c.QualityTodoConf)
	}
}

// TestLoadExtractInvalidFallback 验证无效 env 值时回退默认值：
// 整数解析失败/非正数 → 回退；浮点越界/解析失败 → 回退。
// 注：t.Setenv 不能在并行子测试中使用，故在同一测试内顺序覆盖各场景。
func TestLoadExtractInvalidFallback(t *testing.T) {
	t.Setenv("ARK_API_KEY", "test-key")

	cases := []struct {
		name  string
		key   string
		value string
		check func(c *Config) bool // 返回 true 表示符合默认值预期
	}{
		{"整数解析失败回退默认10", "ZW_EXTRACT_WINDOW", "abc", func(c *Config) bool { return c.ExtractWindow == 10 }},
		{"非正整数回退默认10", "ZW_EXTRACT_WINDOW", "0", func(c *Config) bool { return c.ExtractWindow == 10 }},
		{"浮点越界回退默认0.6", "ZW_QUALITY_MIN_CONF", "1.5", func(c *Config) bool { return c.QualityMinConf == 0.6 }},
		{"浮点解析失败回退默认0.85", "ZW_QUALITY_TODO_CONF", "not-a-number", func(c *Config) bool { return c.QualityTodoConf == 0.85 }},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Setenv(tc.key, tc.value)
			c, err := Load()
			if err != nil {
				t.Fatal(err)
			}
			if !tc.check(c) {
				t.Errorf("%s=%s 未回退默认值：got window=%d minConf=%v todoConf=%v",
					tc.key, tc.value, c.ExtractWindow, c.QualityMinConf, c.QualityTodoConf)
			}
		})
	}
}

func TestAgentConfigDefaults(t *testing.T) {
	t.Setenv("ARK_API_KEY", "test-key") // Load 要求 ARK_API_KEY
	cfg, err := Load()
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if !cfg.AgentEnabled {
		t.Error("AgentEnabled 默认应为 true")
	}
	if cfg.AgentCordisConfig != "services/agent-sidecar/cordis.agent.yml" {
		t.Errorf("AgentCordisConfig 默认错误: %q", cfg.AgentCordisConfig)
	}
	if cfg.AgentSidecarCmd != "node services/agent-sidecar/node_modules/.bin/dsh-jsonrpc-agent" {
		t.Errorf("AgentSidecarCmd 默认错误: %q", cfg.AgentSidecarCmd)
	}
	if cfg.AgentMCPURL != "http://127.0.0.1:8080/internal/mcp" {
		t.Errorf("AgentMCPURL 默认错误: %q", cfg.AgentMCPURL)
	}
	if cfg.DSHSessionRoot != "./data/dsh-sessions" {
		t.Errorf("DSHSessionRoot 默认错误: %q", cfg.DSHSessionRoot)
	}
	if cfg.AgentRetrieveTopK != 10 {
		t.Errorf("AgentRetrieveTopK 默认应为 10, got %d", cfg.AgentRetrieveTopK)
	}
	if cfg.ReviewDailyCron != "0 22 * * *" {
		t.Errorf("ReviewDailyCron 默认错误: %q", cfg.ReviewDailyCron)
	}
}

func TestAgentConfigOverride(t *testing.T) {
	t.Setenv("ARK_API_KEY", "test-key")
	t.Setenv("ZW_AGENT_ENABLED", "false")
	t.Setenv("ZW_AGENT_MODEL", "deepseek-v3-250324")
	t.Setenv("ZW_AGENT_RETRIEVE_TOPK", "20")
	cfg, err := Load()
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if cfg.AgentEnabled {
		t.Error("ZW_AGENT_ENABLED=false 应关闭")
	}
	if cfg.AgentModel != "deepseek-v3-250324" {
		t.Errorf("AgentModel 覆盖失败: %q", cfg.AgentModel)
	}
	if cfg.AgentRetrieveTopK != 20 {
		t.Errorf("AgentRetrieveTopK 覆盖失败: %d", cfg.AgentRetrieveTopK)
	}
}

func TestGetenvBool(t *testing.T) {
	t.Run("unset_returns_default", func(t *testing.T) {
		os.Unsetenv("ZW_TEST_BOOL")
		if !getenvBool("ZW_TEST_BOOL", true) {
			t.Error("未设置应返回 def=true")
		}
		if getenvBool("ZW_TEST_BOOL", false) {
			t.Error("未设置应返回 def=false")
		}
	})
	cases := []struct {
		val  string
		want bool
	}{
		{"true", true}, {"false", false}, {"1", true}, {"0", false},
		{"True", true}, {"False", false}, {"TRUE", true},
	}
	for _, c := range cases {
		t.Run(c.val, func(t *testing.T) {
			t.Setenv("ZW_TEST_BOOL", c.val)
			if got := getenvBool("ZW_TEST_BOOL", !c.want); got != c.want {
				t.Errorf("getenvBool(%q) = %v, want %v", c.val, got, c.want)
			}
		})
	}
	t.Run("garbage_returns_default", func(t *testing.T) {
		t.Setenv("ZW_TEST_BOOL", "notabool")
		if !getenvBool("ZW_TEST_BOOL", true) {
			t.Error("无法解析应回退 def=true")
		}
		if getenvBool("ZW_TEST_BOOL", false) {
			t.Error("无法解析应回退 def=false")
		}
	})
}
