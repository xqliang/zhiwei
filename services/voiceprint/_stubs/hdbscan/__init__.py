# hdbscan stub：wespeaker/diar/umap_clusterer.py 顶部 `import hdbscan`，cluster() 内用 hdbscan.HDBSCAN。
# diarization 路径(sidecar 不做——Go 侧聚类)，embedding 不走；空类让 import 通过即可。


class HDBSCAN:  # noqa: D101
    def __init__(self, *_a, **_k):
        self.labels_ = []

    def fit(self, *_a, **_k):
        return self
