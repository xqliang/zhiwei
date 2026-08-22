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
        assert client.post("/remove", json={"speaker_id": 42}).json()["ok"] is True
        assert client.post("/search", json={"vector": v}).json()["matched"] is False


def test_health():
    with TestClient(appmod.app) as client:
        r = client.get("/health")
        assert r.status_code == 200 and r.json()["status"] == "ok"
