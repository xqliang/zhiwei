"""声纹 sidecar：WeSpeaker 提向量 + FAISS 1:N。
契约见 spec §6.1:
  POST /embed   {audio_path}          -> {vector:[256 float]}
  POST /search  {vector}              -> {speaker_id, distance, second_distance, matched:true} | {matched:false}
      多向量模型（2026-08-26）：一个 speaker 可有多条声纹向量（感冒/哑嗓/变声等变体），
      top-1 = 与该人**任意一条**向量的最高相似度；second_distance = **另一个人**的最高分
      （按说话人去重——同一人的第二条样本不算 top-2，否则自己跟自己比会把 Go 侧
      「区分性弱命中」的 top1−top2 gap 规则误杀）。库中向量 <2 个人时 second 为 0。
  POST /add     {vector, speaker_id}  -> {ok:true}     追加一条向量（同一 speaker 可多次 add）
      注意：不再「先删后加」——多向量语义下幂等由 Go 侧管理（改样本后 Remove+逐条 Add 重建）。
  POST /remove  {speaker_id}          -> {ok:true}     删该说话人的**全部**向量
  GET  /health                          -> {status, model, n_vectors}
faiss 不可用时用 numpy 暴力余弦索引（CI/本地无 faiss 也能跑测试）。
索引落盘 data/voiceprint.index（faiss）或 .npz（numpy）。
"""
from __future__ import annotations
import os
import threading
import numpy as np
from fastapi import FastAPI, HTTPException
from pydantic import BaseModel
from .embedder import load_embedder, StubEmbedder

try:
    import faiss
except ImportError:
    faiss = None

INDEX_PATH = os.environ.get("ZW_VOICEPRINT_INDEX", "data/voiceprint.index")
DIM = 256

app = FastAPI()
_embedder = load_embedder()
_lock = threading.Lock()
_index = None


class _NumpyIndex:
    """faiss 不可用时的纯 numpy 暴力余弦索引。

    向量在入库前已 L2 归一，故内积即余弦相似度。
    落盘用 np.savez 归档 (vecs, ids) 两个数组到 INDEX_PATH + '.npz'。
    """
    def __init__(self) -> None:
        self.vecs: list[np.ndarray] = []
        self.ids: list[int] = []

    def search(self, q: np.ndarray):
        """按说话人去重的 top-2 检索：返回 [(id, sim), ...]（1 或 2 项，id 互异）；
        空索引返回 None。同一 speaker 的多条向量先取 max 再参与排序（多向量模型：
        一个人感冒/正常各一条，任一命中即算命中）。
        """
        if not self.vecs:
            return None
        sims = np.stack(self.vecs) @ q
        best: dict[int, float] = {}
        for i, sid in enumerate(self.ids):
            s = float(sims[i])
            if sid not in best or s > best[sid]:
                best[sid] = s
        ranked = sorted(best.items(), key=lambda kv: -kv[1])[:2]
        return [(sid, s) for sid, s in ranked]

    def remove(self, sid: int) -> None:
        keep = [k for k, _id in enumerate(self.ids) if _id != sid]
        self.vecs = [self.vecs[k] for k in keep]
        self.ids = [self.ids[k] for k in keep]

    def add(self, v: np.ndarray, sid: int) -> None:
        # 多向量语义：纯追加（同一 speaker 可多条——感冒/哑嗓/变声变体各自一条）。
        # 不再做「先删后加」的幂等；向量生命周期由 Go 侧按样本行管理
        # （改样本 = /remove 全删 + 逐条 /add 重建）。
        self.vecs.append(v)
        self.ids.append(sid)

    def count(self) -> int:
        return len(self.ids)

    def all_ids(self) -> list:
        """返回索引中所有 speaker_id（不去重，去重由调用方负责）。"""
        return list(self.ids)

    def _npz_path(self) -> str:
        # np.savez 会在无 .npz 后缀时自动补 .npz；这里显式拼好，读写用同一路径。
        return INDEX_PATH + ".npz"

    def save(self) -> None:
        path = self._npz_path()
        parent = os.path.dirname(path)
        if parent:
            os.makedirs(parent, exist_ok=True)
        if self.vecs:
            # np.savez 归档：vecs=(N,256) float32，ids=(N,) int64；load 时按键取回。
            np.savez(path,
                     vecs=np.stack(self.vecs).astype(np.float32),
                     ids=np.array(self.ids, dtype=np.int64))
        elif os.path.exists(path):
            # 删空后清理旧文件，避免重启时“复活”已删说话人。
            os.remove(path)

    def load(self) -> None:
        path = self._npz_path()
        if os.path.exists(path):
            d = np.load(path, allow_pickle=False)
            self.vecs = [v.astype(np.float32) for v in d["vecs"]]
            self.ids = [int(x) for x in d["ids"].tolist()]


@app.on_event("startup")
def _startup() -> None:
    global _index
    if faiss is not None:
        if os.path.exists(INDEX_PATH):
            _index = faiss.read_index(INDEX_PATH)
        else:
            _index = faiss.IndexIDMap2(faiss.IndexFlatIP(DIM))
    else:
        _index = _NumpyIndex()
        _index.load()


def _to_vec(arr) -> np.ndarray:
    v = np.asarray(arr, dtype=np.float32)
    if v.shape != (DIM,):
        raise ValueError(f"期望 {DIM} 维，实际 {v.shape}")
    n = float(np.linalg.norm(v))
    if n > 0:
        v = v / n
    return v


def _search_faiss(q: np.ndarray):
    """按说话人去重的 top-2 检索（faiss 路径）：返回 [(id, sim), ...]（1 或 2 项，id 互异）；
    空索引返回 None。同一 speaker 多条向量取 max（多向量模型，见 _NumpyIndex.search）。
    实现：从小 k 起逐轮放大检索窗口，直到凑齐 2 个不同 speaker 或窗口覆盖全库。
    """
    ntotal = _index.ntotal
    if ntotal == 0:
        return None
    k = 8
    while True:
        k = min(k, ntotal)
        D, I = _index.search(q.reshape(1, -1), k)
        best: dict[int, float] = {}
        for idx, sid in enumerate(I[0]):
            if sid == -1:
                continue
            s = float(D[0][idx])
            sid = int(sid)
            if sid not in best or s > best[sid]:
                best[sid] = s
        ranked = sorted(best.items(), key=lambda kv: -kv[1])[:2]
        # 窗口内已凑齐 2 个不同说话人（或窗口已覆盖全库）→ 定论
        if len(ranked) >= 2 or k >= ntotal:
            return ranked
        k *= 2  # top-k 全是同一人（多条变体向量）→ 放大窗口再看


class EmbedReq(BaseModel):
    audio_path: str

class VecReq(BaseModel):
    vector: list[float]

class AddReq(BaseModel):
    vector: list[float]
    speaker_id: int

class RemoveReq(BaseModel):
    speaker_id: int


@app.get("/health")
def health() -> dict:
    n = _index.count() if isinstance(_index, _NumpyIndex) else _index.ntotal
    return {"status": "ok", "model": type(_embedder).__name__, "n_vectors": n}


@app.get("/ids")
def list_ids() -> dict:
    """返回索引中所有 speaker_id（去重）——resync 时清理幽灵 ID（DB 已删但索引残留）。"""
    ids = _index.all_ids() if isinstance(_index, _NumpyIndex) else []
    if not isinstance(_index, _NumpyIndex) and hasattr(_index, "ntotal") and _index.ntotal > 0:
        # FAISS IndexIDMap：从 id_map 提取所有 id
        try:
            ids = [int(_index.id_map.at(i)) for i in range(_index.ntotal)]
        except Exception:
            ids = []
    return {"ids": sorted(set(ids))}


@app.post("/embed")
def embed(req: EmbedReq) -> dict:
    try:
        v = _embedder.embed(req.audio_path)
        v = v / (np.linalg.norm(v) + 1e-12)
        return {"vector": v.tolist()}
    except Exception as e:
        raise HTTPException(status_code=500, detail=f"embed failed: {e}")


@app.post("/search")
def search(req: VecReq) -> dict:
    q = _to_vec(req.vector)
    if isinstance(_index, _NumpyIndex):
        res = _index.search(q)
    else:
        res = _search_faiss(q)
    if res is None:
        return {"matched": False}
    sid, dist = res[0]
    # 次高分 = **另一个人**的最高分（按说话人去重，见模块 docstring）；
    # 库中不足 2 人时为 0。Go 侧区分性弱命中规则（top1−top2 gap）用。
    second = res[1][1] if len(res) > 1 else 0.0
    return {"speaker_id": sid, "distance": dist, "second_distance": second, "matched": True}


@app.post("/add")
def add(req: AddReq) -> dict:
    v = _to_vec(req.vector)
    with _lock:
        # 多向量语义：纯追加（同一 speaker 可多条变体向量）。Go 侧改样本后走
        # /remove（全删该人）+ 逐条 /add 重建，索引与 DB 样本行保持一致。
        if isinstance(_index, _NumpyIndex):
            _index.add(v, req.speaker_id)
            _index.save()
        else:
            _index.add_with_ids(v.reshape(1, -1), np.array([req.speaker_id], dtype=np.int64))
            faiss.write_index(_index, INDEX_PATH)
    return {"ok": True}


@app.post("/remove")
def remove(req: RemoveReq) -> dict:
    with _lock:
        if isinstance(_index, _NumpyIndex):
            _index.remove(req.speaker_id)
            _index.save()
        else:
            _index.remove_ids(np.array([req.speaker_id], dtype=np.int64))
            faiss.write_index(_index, INDEX_PATH)
    return {"ok": True}
