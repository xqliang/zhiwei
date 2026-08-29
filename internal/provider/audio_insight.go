package provider

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"strings"
	"time"
)

// AudioInsight 是一次音频理解的结构化结果（spec §3）。
type AudioInsight struct {
	AcousticScene    string           `json:"acoustic_scene"`
	BackgroundSounds []string         `json:"background_sounds"`
	WeatherCues      string           `json:"weather_cues"`
	OverallMood      string           `json:"overall_mood"`
	Speakers         []SpeakerInsight `json:"speakers"`
}

type SpeakerInsight struct {
	Label        string  `json:"label"`
	Emotion      string  `json:"emotion"`
	MicroEmotion string  `json:"micro_emotion"`
	MentalState  string  `json:"mental_state"`
	Confidence   float64 `json:"confidence"`
}

// AudioInsightProvider 输入一个音频文件（≤ 模型时长上限，长录音已在 stage 分块）+ 已知说话人标签，
// 返回结构化情绪/声学场景。业务只依赖此接口。
type AudioInsightProvider interface {
	Analyze(ctx context.Context, audioPath string, speakerLabels []string) (AudioInsight, error)
}

type StepAudioInsight struct {
	baseURL, apiKey, model string
	client                 *http.Client
}

func NewStepAudioInsight(baseURL, apiKey, model string) *StepAudioInsight {
	return &StepAudioInsight{baseURL: baseURL, apiKey: apiKey, model: model, client: &http.Client{Timeout: 180 * time.Second}}
}

const audioInsightSystem = "你是声学场景与情绪分析器。只输出一个 JSON 对象，不要任何解释、不要 markdown 代码块。"

func audioInsightPrompt(labels []string) string {
	ls := "（未提供，请用说话人1/2…）"
	if len(labels) > 0 {
		ls = strings.Join(labels, "、")
	}
	return "分析这段录音的【声音本身】(非文字内容)。已知说话人标签：" + ls + "。严格按此 JSON 模式输出：" +
		`{"acoustic_scene":"室内|室外|会议室|餐厅|车内|地铁|户外|电梯|厨房|办公室|未知",` +
		`"background_sounds":["键盘|车流|鸟叫|宠物叫|风声|雨声|金属撞击|人声嘈杂|无"],` +
		`"weather_cues":"无|有风|有雨|雷电",` +
		`"overall_mood":"一句话",` +
		`"speakers":[{"label":"与已知标签对应","emotion":"平静|喜悦|焦虑|愤怒|疲惫|…","micro_emotion":"细微语气","mental_state":"精神状态","confidence":0.0}]}`
}

type aiChatReq struct {
	Model       string      `json:"model"`
	Messages    []aiMessage `json:"messages"`
	Temperature float64     `json:"temperature"`
}
type aiMessage struct {
	Role    string `json:"role"`
	Content any    `json:"content"`
}
type aiContentText struct {
	Type string `json:"type"`
	Text string `json:"text"`
}
type aiContentAudio struct {
	Type       string       `json:"type"`
	InputAudio aiAudioInner `json:"input_audio"`
}
type aiAudioInner struct {
	Data   string `json:"data"`
	Format string `json:"format"`
}

func (p *StepAudioInsight) Analyze(ctx context.Context, audioPath string, labels []string) (AudioInsight, error) {
	b, err := os.ReadFile(audioPath)
	if err != nil {
		return AudioInsight{}, fmt.Errorf("读音频: %w", err)
	}
	audio := base64.StdEncoding.EncodeToString(b)
	req := aiChatReq{
		Model:       p.model,
		Temperature: 0.2,
		Messages: []aiMessage{
			{Role: "system", Content: audioInsightSystem},
			{Role: "user", Content: []any{
				aiContentText{Type: "text", Text: audioInsightPrompt(labels)},
				aiContentAudio{Type: "input_audio", InputAudio: aiAudioInner{Data: audio, Format: "wav"}},
			}},
		},
	}
	body, _ := json.Marshal(req)
	httpReq, _ := http.NewRequestWithContext(ctx, "POST", p.baseURL+"/chat/completions", bytes.NewReader(body))
	httpReq.Header.Set("Authorization", "Bearer "+p.apiKey)
	httpReq.Header.Set("Content-Type", "application/json")
	resp, err := p.client.Do(httpReq)
	if err != nil {
		return AudioInsight{}, err
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(resp.Body)
	var cr struct {
		Choices []struct {
			Message struct {
				Content string `json:"content"`
			} `json:"message"`
		} `json:"choices"`
		Error *struct {
			Message string `json:"message"`
		} `json:"error"`
	}
	if err := json.Unmarshal(raw, &cr); err != nil {
		return AudioInsight{}, fmt.Errorf("响应解析(http %d): %s", resp.StatusCode, truncate(raw))
	}
	if cr.Error != nil {
		return AudioInsight{}, fmt.Errorf("audio-insight 错误: %s", cr.Error.Message)
	}
	if len(cr.Choices) == 0 {
		return AudioInsight{}, fmt.Errorf("空响应(http %d): %s", resp.StatusCode, truncate(raw))
	}
	return parseAudioInsight(cr.Choices[0].Message.Content)
}

// parseAudioInsight 清洗并解析模型输出（容忍 ```json 代码块与首尾杂字）。
func parseAudioInsight(s string) (AudioInsight, error) {
	s = strings.TrimSpace(s)
	// 去 ```json / ``` 围栏
	if strings.HasPrefix(s, "```") {
		if i := strings.IndexByte(s, '\n'); i >= 0 {
			s = s[i+1:]
		}
		s = strings.TrimSuffix(strings.TrimSpace(s), "```")
	}
	// 截取第一个 { 到最后一个 }（去掉围栏外杂字）
	if l, r := strings.IndexByte(s, '{'), strings.LastIndexByte(s, '}'); l >= 0 && r > l {
		s = s[l : r+1]
	}
	var ins AudioInsight
	if err := json.Unmarshal([]byte(strings.TrimSpace(s)), &ins); err != nil {
		return AudioInsight{}, fmt.Errorf("audio-insight JSON 解析失败: %w", err)
	}
	return ins, nil
}
