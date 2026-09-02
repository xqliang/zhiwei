#!/usr/bin/env python3
"""denoise_df.py —— DeepFilterNet3 降噪 wrapper（zhiwei asr stage 子进程调用）。

用法：python denoise_df.py <src.wav> <dst.wav> [atten_lim_db]

设计要点：
- 直接调 df 库（init_df/enhance/load_audio/save_audio）而非 deepFilter CLI：
  CLI 在 macOS 上写完输出文件后进程挂起不退出（torch/torchaudio 清理问题，
  实测 4 分钟+不退），库直调可控；
- 末尾 os._exit(0) 绕过解释器清理（atexit/析构）——文件已 save_audio 落盘关闭，
  这是规避上述挂起的关键，勿改回 sys.exit；
- 模型 DeepFilterNet3（首次运行自动下载权重到 ~/.cache/DeepFilterNet）；
  实测 75s 音频含模型加载 2.7s（RTF≈0.036）；
- 输出与输入同采样率/声道（本仓库调用方保证 16k mono s16le，见 stage_asr
  transcodeToWAV），供 ASR 直接消费；
- atten_lim_db：降噪强度（dB）。DeepFilterNet 语义=增强信号与原始信号的混合上限：
  12dB 只压 12dB、其余噪声保留；越大越强，0 等效不降噪。
"""
import os
import sys
import time


def main() -> int:
    if len(sys.argv) < 3:
        print("用法: denoise_df.py <src.wav> <dst.wav> [atten_lim_db]", file=sys.stderr)
        return 2
    src, dst = sys.argv[1], sys.argv[2]
    atten = float(sys.argv[3]) if len(sys.argv) > 3 else 21.0

    t0 = time.time()
    # import 放 main 内：参数错误时不必等 torch 加载（秒级）才报 usage
    from df.enhance import init_df, enhance
    from df.io import load_audio, save_audio

    model, df_state, _ = init_df(model_base_dir="DeepFilterNet3", log_level="ERROR", log_file=None)
    audio, _ = load_audio(src, df_state.sr())
    enhanced = enhance(model, df_state, audio, atten_lim_db=atten)
    save_audio(dst, enhanced, df_state.sr())
    dur = time.time() - t0
    print(f"denoised atten={atten}dB took={dur:.1f}s", flush=True)
    return 0


if __name__ == "__main__":
    code = main()
    sys.stdout.flush()
    sys.stderr.flush()
    # 关键：绕过解释器退出清理。macOS 实测 df 库加载后正常退出路径会挂起
    # （torch/torchaudio 线程清理问题，写完文件 4 分钟+不退）；输出已落盘关闭，
    # os._exit 直接终止进程，调用方（exec.Command）立即拿到退出码。
    os._exit(code)
