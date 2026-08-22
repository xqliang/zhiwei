# s3prl.util stub：S3prlFrontend.__init__ 内若传 download_dir 会调 s3prl.util.download.set_dir。
# resnet34 不走该前端，永不调用——空包让 `import s3prl` 的子模块路径可解析即可。
