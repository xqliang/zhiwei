import os
import tempfile

os.environ["ZW_VOICEPRINT_INDEX"] = tempfile.mktemp(suffix=".npz")

from services.voiceprint import app as appmod  # noqa: E402

# 强制 numpy 索引（撤掉 faiss），用 StubEmbedder，保证测试不依赖真模型/faiss
appmod.faiss = None
appmod._embedder = appmod.StubEmbedder()

from fastapi.testclient import TestClient  # noqa: E402

# 注意：Starlette 1.6 的 TestClient 只有作为上下文管理器（with ...）时才会触发
# FastAPI 的 startup 事件；startup 里才会把 _index 初始化成 _NumpyIndex。
# 故每个用例都用 `with TestClient(appmod.app) as client:` 确保索引已就绪。


def test_embed_dim():
    with TestClient(appmod.app) as client:
        with tempfile.NamedTemporaryFile(suffix=".wav") as f:
            r = client.post("/embed", json={"audio_path": f.name})
            assert r.status_code == 200
            assert len(r.json()["vector"]) == 256


def test_add_search_remove():
    with TestClient(appmod.app) as client:
        with tempfile.NamedTemporaryFile(suffix=".wav") as f:
            name = f.name
        v = client.post("/embed", json={"audio_path": name}).json()["vector"]
        assert client.post("/search", json={"vector": v}).json()["matched"] is False  # 空索引
        assert client.post("/add", json={"vector": v, "speaker_id": 42}).json()["ok"] is True
        r = client.post("/search", json={"vector": v}).json()
        assert r["matched"] is True and r["speaker_id"] == 42
        # top-2 契约：单说话人库 second_distance 为 0（不足 2 个人）
        assert r["second_distance"] == 0.0
        # 加第二个向量后 second_distance 反映**另一个人**的最高分且 ≤ top-1
        v2 = [x * 0.5 for x in v]
        n2 = sum(x * x for x in v2) ** 0.5
        v2 = [x / n2 for x in v2]
        assert client.post("/add", json={"vector": v2, "speaker_id": 43}).json()["ok"] is True
        r2 = client.post("/search", json={"vector": v}).json()
        assert r2["matched"] is True and r2["speaker_id"] == 42
        assert 0.0 < r2["second_distance"] <= r2["distance"]  # 另一人的最高分存在且不超过 top-1
        assert client.post("/remove", json={"speaker_id": 42}).json()["ok"] is True
        assert client.post("/remove", json={"speaker_id": 43}).json()["ok"] is True
        assert client.post("/search", json={"vector": v}).json()["matched"] is False


def test_multi_vector_per_speaker():
    """多向量模型（2026-08-26）：同一 speaker 多条向量（感冒/正常变体）——
    ① /add 纯追加（不再先删后加），一人可多条；
    ② /search 的 top-1 = 与该人任意一条的最高分；second = **另一个人**的最高分
      （同一人的第二条不算 top-2——否则自己跟自己比会把 gap 规则误杀）；
    ③ /remove 删该人全部向量。"""
    with TestClient(appmod.app) as client:
        e = [0.0] * 256
        e[0] = 1.0          # 「正常」声纹
        e2 = [0.0] * 256
        e2[2] = 1.0         # 「感冒」声纹（与 e 正交的变体）
        other = [0.0] * 256
        other[0] = 0.6      # 另一个人：与 e 余弦 0.6、与 e2 正交（0）
        other[1] = 0.8      # (0.6,0.8) 已归一

        def _norm(v):
            n = sum(x * x for x in v) ** 0.5
            return [x / n for x in v]

        e, e2, other = _norm(e), _norm(e2), _norm(other)
        # 42 号人两条变体；43 号人一条
        assert client.post("/add", json={"vector": e, "speaker_id": 42}).json()["ok"] is True
        assert client.post("/add", json={"vector": e2, "speaker_id": 42}).json()["ok"] is True
        assert client.post("/add", json={"vector": other, "speaker_id": 43}).json()["ok"] is True
        health = client.get("/health").json()
        assert health["n_vectors"] == 3  # 追加语义：三条都在（不再被同 id 覆盖）

        # 查询「感冒」向量：top-1 应命中 42 的感冒条目（1.0）而非聚合（0.707）
        r = client.post("/search", json={"vector": e2}).json()
        assert r["matched"] is True and r["speaker_id"] == 42
        assert r["distance"] > 0.999  # max over 42 的两条 = e2 自相似 1.0
        # second = 另一个人（43）与 e2 的最高分 = 0（正交），而不是 42 自己的另一条
        assert r["second_distance"] < 1e-6

        # 查询「正常」向量：top-1 = 42/e（1.0）；second = 43 的 0.6（不是 42 的感冒条目 0）
        r2 = client.post("/search", json={"vector": e}).json()
        assert r2["speaker_id"] == 42 and r2["distance"] > 0.999
        assert abs(r2["second_distance"] - 0.6) < 1e-5

        # remove 删 42 的全部（两条）
        assert client.post("/remove", json={"speaker_id": 42}).json()["ok"] is True
        assert client.get("/health").json()["n_vectors"] == 1
        assert client.post("/remove", json={"speaker_id": 43}).json()["ok"] is True


def test_health():
    with TestClient(appmod.app) as client:
        r = client.get("/health")
        assert r.status_code == 200 and r.json()["status"] == "ok"
