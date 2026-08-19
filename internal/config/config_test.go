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
