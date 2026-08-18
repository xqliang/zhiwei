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
	if c.LLMFastModel != "doubao-seed-1.6-flash" {
		t.Errorf("LLMFastModel = %s", c.LLMFastModel)
	}
	if c.ASRModel != "doubao-seed-asr-2-0" {
		t.Errorf("ASRModel = %s", c.ASRModel)
	}
}

func TestLoadRequiresARKKey(t *testing.T) {
	t.Setenv("ARK_API_KEY", "")
	if _, err := Load(); err == nil {
		t.Fatal("want error when ARK_API_KEY empty")
	}
}
