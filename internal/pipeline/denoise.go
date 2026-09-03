// denoise.go：降噪能力抽象与 DeepFilterNet3 实现。
// 两个使用域：①ASR 前降噪（denoiseForASR，用户开关 denoise_enabled）；②声纹域降噪
// （voiceprintWAVForStage，用户开关 denoise_voiceprint——录入/添加/对比的切片提向
// 全部换降噪源）。域间独立开关、共用产物 {sid}.denoised.wav 与强度设置；默认均关，
// 行为与历史一致。
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

// DenoisedWAVPath 同 denoisedWAVPath（导出：api 包的播放预览/段录入需要与 pipeline
// 完全一致的产物命名——同一文件、同一幂等语义）。
func DenoisedWAVPath(dataDir string, sessionID ids.ID) string {
	return denoisedWAVPath(dataDir, sessionID)
}

// EnsureDenoisedWAV 确保 dst 存在：不存在则用 denoiser 从 src 生成（attenLimDB 强度）。
// 返回是否本次新生成（已存在=false，调用方据此只在真生成时记耗时 trace）。
// 导出供 api 包共用（手动录入/试匹配的声纹域降噪也要「有则复用、无则生成」的同一语义）。
func EnsureDenoisedWAV(ctx context.Context, dn Denoiser, src, dst string, attenLimDB float64) (generated bool, err error) {
	if _, serr := os.Stat(dst); serr == nil {
		return false, nil
	}
	return true, dn.Denoise(ctx, src, dst, attenLimDB)
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
	begin := time.Now()
	gen, err := EnsureDenoisedWAV(ctx, d.Denoise, wavPath, dst, st.DenoiseAttenLim)
	if err != nil {
		log.Printf("[asr] session=%s 降噪失败（降级用原始音频）: %v", sessionID, err)
		appendTrace(j, repo.TraceEntry{Stage: "asr", Error: fmt.Sprintf("降噪失败（降级原始音频）: %v", err)})
		return "", false
	}
	if gen { // 幂等复用不重复记耗时
		appendTrace(j, repo.TraceEntry{Stage: "asr:denoise", Model: "DeepFilterNet3", MS: msSince(begin)})
	}
	return dst, true
}

// voiceprintWAVForStage 声纹域基准音频：用户开启「声纹降噪」（asr_settings.denoise_
// voiceprint）→ 切片的源 wav 换成降噪版（不存在则现场生成，强度用同一 atten 设置）。
// speaker stage 的切片提向/检索基准/自动登记全部经此取源——同 session 同域；与库内
// 向量的跨域可比性由用户开关统一决定（见 AsrSettings.DenoiseVoiceprint 注释）。
// 任何失败降级原始 wav（尽力而为，log 记录——runSpeakerStage 无 job 句柄可写 trace）。
// 依赖未装配（AsrSettings/Sessions/Denoise 任一为 nil，兼容旧测试装配）→ 原始 wav。
func voiceprintWAVForStage(ctx context.Context, d StageDeps, sessionID ids.ID, wavPath string) string {
	if d.AsrSettings == nil || d.Denoise == nil || d.Sessions == nil {
		return wavPath
	}
	s, err := d.Sessions.Get(ctx, 1, sessionID) // 后台流水线无请求上下文，暂 user-1（同 stageASR）
	if err != nil {
		log.Printf("[speaker] session=%s 读 session 失败（声纹域判定降级原始音频）: %v", sessionID, err)
		return wavPath
	}
	st, err := d.AsrSettings.Get(ctx, s.UserID)
	if err != nil || !st.DenoiseVoiceprint {
		return wavPath // 未开启/读失败 → 原始音频（best-effort 不挡归属）
	}
	dst := denoisedWAVPath(d.DataDir, sessionID)
	if _, err := EnsureDenoisedWAV(ctx, d.Denoise, wavPath, dst, st.DenoiseAttenLim); err != nil {
		log.Printf("[speaker] session=%s 声纹域降噪失败（降级原始音频）: %v", sessionID, err)
		return wavPath
	}
	return dst
}
