"""spike: 验证 WeSpeaker resnet34-LM 加载 + 提取 256 维声纹。

用法:
    make spike-voiceprint   # = .venv/bin/python services/voiceprint/spike.py testdata/speech.wav

真实加载逻辑已固化到 services/voiceprint/embedder.py 的 _WeSpeakerEmbedder；
本脚本复用 load_embedder() 跑一遍，确认维度=256 + 确定性。
"""
import sys
from pathlib import Path

import numpy as np

# stubs 前置到 sys.path 最前：遮蔽 wespeaker 的 SSL/whisper/diarization 可选前端
# （resnet34 走 fbank 前端用不到它们，但被 import 期 eager 引用）。sidecar 启动同理设 PYTHONPATH。
sys.path.insert(0, str(Path(__file__).resolve().parent / "_stubs"))
from embedder import load_embedder  # noqa: E402  (spike 与 embedder 同目录)


def main():
    if len(sys.argv) < 2:
        print("用法: python services/voiceprint/spike.py <wav>")
        sys.exit(1)
    wav = sys.argv[1]
    emb = load_embedder()
    print("embedder:", type(emb).__name__)
    if type(emb).__name__ == "StubEmbedder":
        print("!! 回退到 StubEmbedder——wespeaker 未就绪或 stubs/依赖未装，见 embedder.py 顶部说明")
    v = emb.embed(wav)
    print("dim:", v.shape)
    assert v.shape == (256,), f"期望 256 维，实际 {v.shape}"
    v = v / (np.linalg.norm(v) + 1e-12)
    print("norm:", float(np.linalg.norm(v)))
    # 确定性：同一段两次提取应一致
    v2 = emb.embed(wav)
    v2 = v2 / (np.linalg.norm(v2) + 1e-12)
    print("两次 max diff:", float(np.abs(v - v2).max()))
    print("ok")


if __name__ == "__main__":
    main()
