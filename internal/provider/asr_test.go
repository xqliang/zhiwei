package provider

import (
	"testing"
)

func TestParseSpeakerTranscript(t *testing.T) {
	text := "[说话人1] 明天记得给 Tom 发邮件\n[说话人1] 还有确认会议时间\n[说话人2] 好的没问题"
	pieces := ParseSpeakerTranscript(text)
	if len(pieces) != 3 {
		t.Fatalf("len = %d, pieces = %+v", len(pieces), pieces)
	}
	if pieces[0].SpeakerLabel != "1" || pieces[0].Text != "明天记得给 Tom 发邮件" {
		t.Fatalf("pieces[0] = %+v", pieces[0])
	}
	if pieces[2].SpeakerLabel != "2" || pieces[2].Text != "好的没问题" {
		t.Fatalf("pieces[2] = %+v", pieces[2])
	}
}

func TestParseSpeakerTranscriptNoLabels(t *testing.T) {
	// 无说话人前缀：整体一段，标签为空
	pieces := ParseSpeakerTranscript("今天学习了 Rust 的 ownership")
	if len(pieces) != 1 {
		t.Fatalf("len = %d", len(pieces))
	}
	if pieces[0].SpeakerLabel != "" || pieces[0].Text != "今天学习了 Rust 的 ownership" {
		t.Fatalf("pieces[0] = %+v", pieces[0])
	}
}

func TestParseSpeakerTranscriptEmpty(t *testing.T) {
	if got := ParseSpeakerTranscript("  \n "); len(got) != 0 {
		t.Fatalf("want empty, got %+v", got)
	}
}

func TestParseSpeakerTranscriptStripsTimePrefix(t *testing.T) {
	// 模型偶尔给行首加时间码，应清洗掉
	pieces := ParseSpeakerTranscript("[00:00] 明天记得发邮件")
	if len(pieces) != 1 || pieces[0].Text != "明天记得发邮件" {
		t.Fatalf("pieces = %+v", pieces)
	}
}
