# 贡献指南

感谢你改进 Cryp。

## 开始之前

1. 对较大功能先创建 Issue，说明需求、兼容性和数据格式影响。
2. 安全漏洞不要发布公开 Issue，请遵循 [SECURITY.md](SECURITY.md)。
3. 从最新 `master` 创建范围明确的分支，避免在同一个 Pull Request 中混入无关重构。

开发环境、目录说明和验证命令见 [docs/DEVELOPMENT.md](docs/DEVELOPMENT.md)。

## Pull Request 要求

- 解释用户可见行为、失败路径和兼容性影响。
- 为 bug 修复增加能够复现问题的测试。
- 涉及加密格式、文件删除、导入路径或生命周期时，说明安全边界和回滚方案。
- 涉及 FFmpeg、GPU、浏览器或移动端时，列出实际验证环境。
- 更新长期有效的公开文档，不提交一次性修复日志或内部审查报告。
- 确保测试、race、vet、前端 lint 和 build 通过。

提交即表示你同意按本项目的 [MIT License](LICENSE) 发布贡献。
