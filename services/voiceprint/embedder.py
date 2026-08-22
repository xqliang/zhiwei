"""声纹提取抽象 + WeSpeaker 实现。

Embedder.embed(wav_path) -> np.ndarray(256,) （app.py 再做 L2 归一）。

真实加载用 wespeaker python 包：resnet34-LM(CnCeleb 预训练)，256 维。
关键：用 soundfile 读 wav→PCM 喂 extract_embedding_from_pcm，**不调 torchaudio.load**——
torchaudio 2.11 的 load 改走 torchcodec（需 ffmpeg 系统库 libavutil，过重且常缺），
而 sidecar 收到的本就是 16k mono wav，soundfile(libsndfile) 直读即可。

依赖装配（见 services/voiceprint/requirements.txt + scripts/sidecar.sh）：
  - pip 装 torch/torchaudio/wespeaker(--no-deps)/soundfile/scipy/kaldiio/pyyaml/tqdm/
    silero-vad/onnxruntime/packaging/requests/numpy。
  - wespeaker 的 SSL/whisper/diarization 前端(s3prl/whisper/transformers/peft/umap/hdbscan)
    在 import 期被 eager 引用但 resnet34 用不到——用 _stubs/ 轻量 stub 遮蔽(靠 PYTHONPATH 前置)，
    不动 site-packages。详见 _stubs/ 各 __init__.py 注释 + README。
  - 环境变量 ZW_WESPEAKER_MODEL 默认 'chinese'(CnCeleb resnet34_LM)。

未就绪(wespeaker 未装/import 失败)时 load_embedder 回退 StubEmbedder(仅自测/联调)。
"""
from __future__ import annotations
import hashlib
import os
import numpy as np


class Embedder:
    def embed(self, wav_path: str) -> np.ndarray:
        raise NotImplementedError


class StubEmbedder(Embedder):
    """测试用：对同一路径返回稳定 256 维向量，便于 /add 后 /search 命中。
    用 hashlib（非内建 hash()）保证跨进程稳定——hash() 受 PYTHONHASHSEED 随机化。"""
    def __init__(self) -> None:
        self._cache: dict[str, np.ndarray] = {}

    def embed(self, wav_path: str) -> np.ndarray:
        if wav_path not in self._cache:
            # sha1(路径) 取前 8 字节作种子 → 稳定可复现
            seed = int.from_bytes(hashlib.sha1(wav_path.encode()).digest()[:8], "big") % (2**32)
            rng = np.random.default_rng(seed)
            v = rng.standard_normal(256).astype(np.float32)
            v /= (np.linalg.norm(v) + 1e-12)
            self._cache[wav_path] = v
        return self._cache[wav_path]


def load_embedder() -> Embedder:
    """生产返回 WeSpeaker resnet34-LM embedder；未实现前/依赖缺失回退 StubEmbedder。"""
    try:
        return _WeSpeakerEmbedder()
    except Exception:
        # 回退：sidecar 仍可起、/health 显 StubEmbedder，便于发现未装 wespeaker
        return StubEmbedder()


class _WeSpeakerEmbedder(Embedder):
    """真实 WeSpeaker resnet34-LM 256 维 embedder。

    模型由 wespeaker.load_model(name) 加载（首次从 ModelScope 下载到 ~/.wespeaker/<name>，
    后续走缓存）。name='chinese' = CnCeleb resnet34_LM；可经 ZW_WESPEAKER_MODEL 切换。
    embed 用 soundfile 读 wav→(1,samples) float32 tensor，喂 extract_embedding_from_pcm
    （内部 fbank+CMVN+resnet34），返回 256 维 float32（未归一，app.py 的 /embed 统一 L2 归一）。
    """
    def __init__(self) -> None:
        import wespeaker  # 顶部 import 失败 → load_embedder 回退 StubEmbedder
        name = os.environ.get("ZW_WESPEAKER_MODEL", "chinese")
        self._model = wespeaker.load_model(name)

    def embed(self, wav_path: str) -> np.ndarray:
        import soundfile as sf
        import torch
        pcm, sr = sf.read(wav_path)  # float64，[-1,1]，与 torchaudio.load(normalize=True) 一致
        t = torch.from_numpy(pcm).float()
        if t.dim() == 2:  # (samples, channels) → 取首道(本就 16k mono)
            t = t[:, 0]
        if t.dim() == 1:
            t = t.unsqueeze(0)  # (1, samples) — 对齐 extract_embedding_from_pcm 期望
        emb = self._model.extract_embedding_from_pcm(t, int(sr))
        if emb is None:
            # apply_vad 路径下整段静音会返回 None；默认 chinese 模型 apply_vad=False 不会，兜底
            raise RuntimeError("WeSpeaker 返回空 embedding（可能整段静音被 VAD 过滤）")
        return np.asarray(emb).ravel().astype(np.float32)
