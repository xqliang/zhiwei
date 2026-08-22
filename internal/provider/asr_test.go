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

func TestParseFileASRResult(t *testing.T) {
	// StepFun 异步文件 ASR /file/query 的真实响应结构：result[].utterances[]，每句带 ms 时间戳 + speaker.id。
	raw := []byte(`{
	  "status":"SUCCEEDED","duration":6.2,
	  "result":[{
	    "text":"你好。我咨询一下。",
	    "utterances":[
	      {"text":"你好。","start_time":2000,"end_time":4500,"speaker":{"id":"spk_1"}},
	      {"text":"我咨询一下。","start_time":4500,"end_time":6200,"speaker":{"id":"spk_2"}}
	    ]
	  }]
	}`)
	pieces := ParseFileASRResult(raw)
	if len(pieces) != 2 {
		t.Fatalf("len=%d", len(pieces))
	}
	if pieces[0].SpeakerLabel != "1" { // spk_1 去前缀得 "1"
		t.Fatalf("label=%q", pieces[0].SpeakerLabel)
	}
	if pieces[0].StartMS != 2000 || pieces[0].EndMS != 4500 {
		t.Fatalf("ms=%d-%d", pieces[0].StartMS, pieces[0].EndMS)
	}
	if pieces[1].SpeakerLabel != "2" || pieces[1].Text != "我咨询一下。" || pieces[1].StartMS != 4500 || pieces[1].EndMS != 6200 {
		t.Fatalf("p1=%+v", pieces[1])
	}
}

func TestParseFileASRResultNoSpeaker(t *testing.T) {
	// 未开 speaker_info：speaker 字段缺失，label 空；时间戳仍解析。
	raw := []byte(`{"status":"SUCCEEDED","result":[{"text":"x","utterances":[
	  {"text":"x","start_time":0,"end_time":100}]}]}`)
	pieces := ParseFileASRResult(raw)
	if len(pieces) != 1 || pieces[0].SpeakerLabel != "" || pieces[0].StartMS != 0 || pieces[0].EndMS != 100 {
		t.Fatalf("%+v", pieces)
	}
}

func TestParseFileASRResultMalformed(t *testing.T) {
	// 非法 JSON / 空结果：返回 nil，不 panic。
	if got := ParseFileASRResult([]byte(`{bad`)); got != nil {
		t.Fatalf("want nil, got %+v", got)
	}
	if got := ParseFileASRResult([]byte(`{"status":"SUCCEEDED","result":[]}`)); len(got) != 0 {
		t.Fatalf("want empty, got %+v", got)
	}
}
