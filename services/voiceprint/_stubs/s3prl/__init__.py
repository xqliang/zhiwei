# s3prl stub：wespeaker/frontend/s3prl.py 顶部 `import s3prl` + `from s3prl.nn import ...`。
# s3prl 真包与 torchaudio 2.11 不兼容(set_audio_backend 已移除)，而 resnet34 走 fbank
# 前端、S3prlFrontend(SSL) 永不实例化——用 stub 让 import 通过即可。
# 放在 PYTHONPATH 最前(service:voiceprint)遮蔽，不动 site-packages。
