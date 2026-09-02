// denoise.go：ASR 前降噪能力抽象与 DeepFilterNet3 实现。
// 降噪只喂 ASR——声纹切片（speaker stage）继续用原始 transcoded wav：既有声纹库
// 全部从原始音频登记，混入降噪版会改变嵌入分布、破坏跨 session 相似度可比性。
package pipeline

import (
	"context"
	"fmt"
	"log"
	"os"
	"os/exec"
	"path/filepath"
	"time"

	"zhiwei/internal/ids"
	"zhiwei/internal/repo"
)

// Denoiser 音频降噪能力：把 src（16k mono s16 wav）降噪后写至 dst（同格式）。
// 实现须自行保证幂等安全（调用方已做「dst 存在即跳过」，见 stageASR）。
type Denoiser interface {
	Denoise(ctx context.Context, src, dst string, attenLimDB float64) error
}

// DeepFilterNetDenoiser 用 DeepFilterNet3 降噪（子进程调 scripts/denoise_df.py）。
// PythonBin 是装了 DeepFilterNet 包的解释器路径（ZW_DENOISE_PYTHON，默认 python3）；
// ScriptPath 是 wrapper 脚本路径（main 装配时转绝对路径，避免依赖进程 CWD）。
// wrapper 末尾 os._exit(0) 绕过解释器退出挂起（macOS 实测 df 库加载后正常退出
// 路径 4 分钟+不退），本侧 exec.Command 正常拿退出码。
type DeepFilterNetDenoiser struct {
	PythonBin  string
	ScriptPath string
}

// Denoise 子进程降噪。失败返回带 stderr 的错误（调用方降级用原始音频 + trace）。
func (d *DeepFilterNetDenoiser) Denoise(ctx context.Context, src, dst string, attenLimDB float64) error {
	out, err := exec.CommandContext(ctx, d.PythonBin, d.ScriptPath, src, dst, fmt.Sprintf("%g", attenLimDB)).
		CombinedOutput()
	if err != nil {
		return fmt.Errorf("deepfilternet: %w: %s", err, out)
	}
	return nil
}

// denoisedWAVPath 降噪产物路径（与 transcoded 同目录）：{sid}.denoised.wav。
// 存在即复用（幂等）：重提取/重跑 asr stage 不重复降噪；改强度设置后想对旧音频
// 重新降噪，删除该文件即可（设置主要影响新录音）。
func denoisedWAVPath(dataDir string, sessionID ids.ID) string {
	return filepath.Join(dataDir, "transcoded", sessionID.String()+".denoised.wav")
}

// denoiseForASR 按用户设置决定送 ASR 的音频：开关开 + Denoiser 已装配 → 产出（或
// 复用）{sid}.denoised.wav 并返回 (path, true)。全程尽力而为——设置读失败/降噪失败
// 都降级用原始音频（ASR 没有降噪也能跑，DB 抖动或模型环境问题不该挡转写），
// 原因记 log + trace（trace stage 为 "asr:denoise"/"asr"——job stage 仍只有 asr）。
func denoiseForASR(ctx context.Context, d StageDeps, j *repo.Job, sessionID ids.ID, userID int64, wavPath string) (string, bool) {
	if d.AsrSettings == nil || d.Denoise == nil {
		return "", false // 设置/实现未装配（测试/降级）→ 不降噪
	}
	st, err := d.AsrSettings.Get(ctx, userID)
	if err != nil {
		log.Printf("[asr] session=%s 读降噪设置失败（降级不降噪）: %v", sessionID, err)
		appendTrace(j, repo.TraceEntry{Stage: "asr", Error: fmt.Sprintf("读降噪设置失败（降级）: %v", err)})
		return "", false
	}
	if !st.DenoiseEnabled {
		return "", false
	}
	dst := denoisedWAVPath(d.DataDir, sessionID)
	if _, err := os.Stat(dst); err == nil {
		return dst, true // 幂等：产物已存在直接复用
	}
	begin := time.Now()
	if err := d.Denoise.Denoise(ctx, wavPath, dst, st.DenoiseAttenLim); err != nil {
		log.Printf("[asr] session=%s 降噪失败（降级用原始音频）: %v", sessionID, err)
		appendTrace(j, repo.TraceEntry{Stage: "asr", Error: fmt.Sprintf("降噪失败（降级原始音频）: %v", err)})
		return "", false
	}
	appendTrace(j, repo.TraceEntry{Stage: "asr:denoise", Model: "DeepFilterNet3", MS: msSince(begin)})
	return dst, true
}
