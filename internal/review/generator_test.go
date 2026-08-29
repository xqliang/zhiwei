package review

import (
	"context"
	"errors"
	"testing"
	"time"
)

func TestGenerateDailyOK(t *testing.T) {
	f := &fakeLLM{Reply: `{"headline":"今天很好","highlights":["a"]}`}
	g := &Generator{LLM: f, Model: "m", DailyPrompt: "SYS"}
	c, raw, err := g.generateDaily(context.Background(), DailyInput{Date: time.Now()})
	if err != nil {
		t.Fatal(err)
	}
	if c.Headline != "今天很好" || len(raw) == 0 {
		t.Errorf("内容异常: %+v", c)
	}
	if f.GotReq.System != "SYS" || f.GotReq.Model != "m" {
		t.Errorf("Chat 请求未带 prompt/model: %+v", f.GotReq)
	}
}

func TestGenerateDailyLLMErr(t *testing.T) {
	g := &Generator{LLM: &fakeLLM{Err: errors.New("boom")}, Model: "m"}
	if _, _, err := g.generateDaily(context.Background(), DailyInput{}); err == nil {
		t.Error("LLM 错误应上抛")
	}
}

func TestGenerateDailyParseErr(t *testing.T) {
	g := &Generator{LLM: &fakeLLM{Reply: "模型跑偏没给 JSON"}, Model: "m"}
	if _, _, err := g.generateDaily(context.Background(), DailyInput{}); err == nil {
		t.Error("解析失败应上抛")
	}
}

// ---- P4 报告漫画 ----

// fakeComic 单测用文生图 mock：返回固定 b64。
type fakeComic struct {
	B64 string
	Err error
}

func (f *fakeComic) Generate(_ context.Context, _ string) (string, error) {
	return f.B64, f.Err
}

// fakeTOS 单测用存图 mock：返回固定 URL。
type fakeTOS struct {
	URL string
	Err error
}

func (f *fakeTOS) UploadImage(_ context.Context, _, _ string) (string, error) {
	return f.URL, f.Err
}

// tryAttachComic：全链路（派生→出图→存 TOS）成功挂上 TOS URL。
func TestTryAttachComicOK(t *testing.T) {
	g := &Generator{
		LLM:          &fakeLLM{Reply: "画面描述"},
		Model:        "m",
		Comic:        &fakeComic{B64: "ZmFrZQ=="},
		ComicStorage: &fakeTOS{URL: "https://tos/x.jpeg"},
	}
	comic := g.tryAttachComic(context.Background(), "叙事", nil, nil)
	if comic == nil || comic.ImageURL != "https://tos/x.jpeg" {
		t.Errorf("应返回 TOS URL，实际: %+v", comic)
	}
}

// tryAttachComic：Comic provider 未注入时返回 nil（不生成漫画）。
func TestTryAttachComicDisabled(t *testing.T) {
	g := &Generator{LLM: &fakeLLM{Reply: "x"}, Model: "m"} // Comic == nil
	if comic := g.tryAttachComic(context.Background(), "叙事", nil, nil); comic != nil {
		t.Errorf("Comic 为 nil 时应返回 nil，实际: %+v", comic)
	}
}

// tryAttachComic：无 TOS 时退回 data URL。
func TestTryAttachComicDataURLFallback(t *testing.T) {
	g := &Generator{
		LLM:   &fakeLLM{Reply: "画面描述"},
		Model: "m",
		Comic: &fakeComic{B64: "ZmFrZQ=="}, // ComicStorage == nil
	}
	comic := g.tryAttachComic(context.Background(), "叙事", nil, nil)
	if comic == nil || comic.ImageURL != "data:image/jpeg;base64,ZmFrZQ==" {
		t.Errorf("应退回 data URL，实际: %+v", comic)
	}
}

// tryAttachComic：出图失败静默降级为 nil。
func TestTryAttachComicGenerateErrSilent(t *testing.T) {
	g := &Generator{
		LLM:          &fakeLLM{Reply: "画面描述"},
		Model:        "m",
		Comic:        &fakeComic{Err: errors.New("出图炸了")},
		ComicStorage: &fakeTOS{URL: "https://tos/x.jpeg"},
	}
	if comic := g.tryAttachComic(context.Background(), "叙事", nil, nil); comic != nil {
		t.Errorf("出图失败应静默返回 nil，实际: %+v", comic)
	}
}
