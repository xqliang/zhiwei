package config

import "testing"

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
