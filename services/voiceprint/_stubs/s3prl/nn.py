# s3prl.nn stub：提供 wespeaker/frontend/s3prl.py 顶部 `from s3prl.nn import Featurizer, S3PRLUpstream`
# 所需的两个名字。两者只在 S3prlFrontend.__init__ 内被实例化/调用(resnet34 不走)，故空类即可。


class Featurizer:  # noqa: D101
    pass


class S3PRLUpstream:  # noqa: D101
    @staticmethod
    def available_names(*_a, **_k):
        return []
