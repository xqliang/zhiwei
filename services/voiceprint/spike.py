"""spike: 验证 WeSpeaker resnet34-LM 加载 + 提取 256 维声纹。

用法:
    pip install -r services/voiceprint/requirements.txt
    python services/voiceprint/spike.py testdata/speech.wav

目的: 拿到可用的「模型加载 + embedding 提取」调用，确认输出维度=256，
      把验证过的加载逻辑固化到 services/voiceprint/embedder.py 的 _WeSpeakerEmbedder。

WeSpeaker 仓库: https://github.com/wenet-org/wespeaker
参考其 README 的 Python / ONNX 推理段落。下面 load_embedder 是占位骨架——
真实加载方式未定（待下载 resnet34-LM 87MB 权重 + 确认是 ONNX 还是 wespeaker python 包），
填好后跑通即可。未就绪前会抛 NotImplementedError，提示先做手动验证。

约定: embed(wav_path) -> np.ndarray(shape=(256,), dtype=float32)，已 L2 归一。
"""
import sys

import numpy as np


def load_embedder():
    # TODO(spike→固化): 按 WeSpeaker 实际 API 填充。候选两条路：
    #   1) ONNX Runtime：下载 resnet34-LM 的 ONNX 权重，ort.InferenceSession，
    #      feed 音频特征（fbank/CMVN，参考 wespeaker 的预处理），取最后隐层 256 维。
    #   2) wespeaker python 包：若有高层 API（如 wespeaker.Speaker），直接加载预训练模型调 extract。
    # 跑通后把实现搬到 services/voiceprint/embedder.py 的 _WeSpeakerEmbedder，删掉这里的占位。
    raise NotImplementedError("spike: 先在此填充 WeSpeaker 加载逻辑并跑通（见文件顶部注释）")


def main():
    if len(sys.argv) < 2:
        print("用法: python services/voiceprint/spike.py <wav>")
        sys.exit(1)
    emb = load_embedder()
    vec = emb.embed(sys.argv[1])
    print("dim:", vec.shape)
    assert vec.shape == (256,), f"期望 256 维，实际 {vec.shape}"
    vec = vec / (np.linalg.norm(vec) + 1e-12)
    print("norm:", float(np.linalg.norm(vec)))
    print("ok")


if __name__ == "__main__":
    main()
