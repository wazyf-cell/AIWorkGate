# 归档：11 个示例渠道插件（2026-11-06，P3）

本目录保存 P3 退役的 11 个 Go 示例插件源码，**不参与根模块构建、不打进发布物、不被 `scripts/test-plugins.ps1` 扫描**：

- autoglm、cline、codearts、gemini、lobsterai、loomy、minimax、officeace、opencode、raccoon、zcode

保留说明：

- `plugins/aichat` 按用户决定保留为普通插件（A 退役后仍需它接入）。
- 这些插件是独立 Go module；恢复某个插件时把对应目录移回 `plugins/` 即可，历史与测试脚本会自动重新包含它。
- 退役前的整仓备份：`F:\Code\DSH-code\program\aiworkgate-backup-20261106.zip`
  （SHA-256 `95429fdc2cca72c4ebec85ce4f4e230319a5e33c46401f45565021b2a1346f47`）。
