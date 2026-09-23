---
name: release
description: 发布 panel 新版本时使用。
---
# 发版

1. 运行 `workflows/release-audit.md` 工作流（在 workspace 中），确认无严重问题。
2. 更新 CHANGELOG（按新增、变更、修复、安全分类）与兼容矩阵（控制面 × Agent × 协议）。
3. 迁移检查：列出本版本新增的迁移及是否可在线执行。
4. 打 tag 触发 GoReleaser：二进制、容器镜像、cosign 签名、SBOM。
5. 在全新环境按安装文档部署并运行冒烟测试。
