"""声纹 sidecar：WeSpeaker 提向量 + FAISS 1:N。
契约见 spec §6.1:
  POST /embed   {audio_path}          -> {vector:[256 float]}
  POST /search  {vector}              -> {speaker_id, distance, matched:true} | {matched:false}
  POST /add     {vector, speaker_id}  -> {ok:true}     幂等：先 remove 再 add
  POST /remove  {speaker_id}          -> {ok:true}
  GET  /health                          -> {status, model, n_vectors}
faiss 不可用时用 numpy 暴力余弦索引（CI/本地无 faiss 也能跑测试）。
索引落盘 data/voiceprint.index（faiss）或 .npz（numpy）。
"""
from __future__ import annotations
import os
import threading
import numpy as np
from fastapi import FastAPI
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

    向量在入库前已 L2 归一，故内积即余弦相似度；搜索取 top-1。
    落盘用 np.savez 归档 (vecs, ids) 两个数组到 INDEX_PATH + '.npz'。
    """
    def __init__(self) -> None:
        self.vecs: list[np.ndarray] = []
        self.ids: list[int] = []

    def search(self, q: np.ndarray):
        if not self.vecs:
            return None
        sims = np.stack(self.vecs) @ q
        i = int(np.argmax(sims))
        return self.ids[i], float(sims[i])

    def remove(self, sid: int) -> None:
        keep = [k for k, _id in enumerate(self.ids) if _id != sid]
        self.vecs = [self.vecs[k] for k in keep]
        self.ids = [self.ids[k] for k in keep]

    def add(self, v: np.ndarray, sid: int) -> None:
        # 幂等：同一 speaker_id 先删后加，避免重复向量。
        self.remove(sid)
        self.vecs.append(v)
        self.ids.append(sid)

    def count(self) -> int:
        return len(self.ids)

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
    D, I = _index.search(q.reshape(1, -1), 1)
    if I[0][0] == -1:
        return None
    return int(I[0][0]), float(D[0][0])


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


@app.post("/embed")
def embed(req: EmbedReq) -> dict:
    v = _embedder.embed(req.audio_path)
    v = v / (np.linalg.norm(v) + 1e-12)
    return {"vector": v.tolist()}


@app.post("/search")
def search(req: VecReq) -> dict:
    q = _to_vec(req.vector)
    if isinstance(_index, _NumpyIndex):
        res = _index.search(q)
    else:
        res = _search_faiss(q)
    if res is None:
        return {"matched": False}
    sid, dist = res
    return {"speaker_id": sid, "distance": dist, "matched": True}


@app.post("/add")
def add(req: AddReq) -> dict:
    v = _to_vec(req.vector)
    with _lock:
        if isinstance(_index, _NumpyIndex):
            _index.add(v, req.speaker_id)
            _index.save()
        else:
            _index.remove_ids(np.array([req.speaker_id], dtype=np.int64))
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
