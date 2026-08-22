# peft stub：wespeaker/frontend/w2vbert.py 顶部 `from peft import LoraConfig, get_peft_model`——
# 仅 W2VBertFrontend(SSL) 内用到，resnet34 不走。空实现即可。


class LoraConfig:  # noqa: D101
    pass


def get_peft_model(*_a, **_k):
    raise NotImplementedError("peft stub: w2vbert 前端未启用")
