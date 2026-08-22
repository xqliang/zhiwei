"""声纹提取抽象 + WeSpeaker 实现(占位)。
Embedder.embed(wav_path) -> np.ndarray(256,) 已 L2 归一。
WeSpeaker 真实加载由 spike.py 验证后填充 _WeSpeakerEmbedder；未就绪返回 StubEmbedder。
"""
from __future__ import annotations
import numpy as np


class Embedder:
    def embed(self, wav_path: str) -> np.ndarray:
        raise NotImplementedError


class StubEmbedder(Embedder):
    """测试用：对同一路径返回稳定 256 维向量，便于 /add 后 /search 命中。"""
    def __init__(self) -> None:
        self._cache: dict[str, np.ndarray] = {}

    def embed(self, wav_path: str) -> np.ndarray:
        if wav_path not in self._cache:
            seed = abs(hash(wav_path)) % 1000
            rng = np.random.default_rng(seed)
            v = rng.standard_normal(256).astype(np.float32)
            v /= (np.linalg.norm(v) + 1e-12)
            self._cache[wav_path] = v
        return self._cache[wav_path]


def load_embedder() -> Embedder:
    """生产返回 WeSpeaker resnet34-LM embedder；未实现前回退 StubEmbedder（仅自测/联调）。"""
    try:
        return _WeSpeakerEmbedder()
    except Exception:
        return StubEmbedder()


class _WeSpeakerEmbedder(Embedder):
    def __init__(self) -> None:
        raise NotImplementedError("WeSpeaker 加载未实现，见 spike.py")
    def embed(self, wav_path: str) -> np.ndarray:
        raise NotImplementedError
