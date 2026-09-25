# Changelog

All notable changes to TokenSplice Gateway will be documented in this file.

Format follows [Keep a Changelog](https://keepachangelog.com/en/1.1.0/).
Versioning follows [Semantic Versioning](https://semver.org/spec/v2.0.0.html).

## [Unreleased]

### Added
- Brand configuration via environment variables (`SYSTEM_NAME`, `LOGO_URL`, `FOOTER`, `TOP_UP_LINK`)
- Project documentation: CONTRIBUTING.md, SECURITY.md, .env.example
- GitHub Actions CI workflow for lint, test, and build verification

### Changed
- Updated `--help` output to reflect TokenSplice Gateway identity
- README.md rewritten for TokenSplice project

## [1.0.0-rc.1] - 2026-09-25

Initial fork from [QuantumNous/new-api](https://github.com/QuantumNous/new-api) at commit `d04c118`.

### Base Features (inherited from new-api v1.0.0-rc.40)
- OpenAI-compatible API gateway supporting 100+ models
- Multi-channel routing with priority, weight, and automatic failover
- Token-based quota billing system
- User management with OAuth (GitHub, Google, LinuxDO, Telegram, WeChat)
- Admin console (React + Semi Design)
- Docker deployment support
- MySQL / PostgreSQL / SQLite database support
- Redis caching and distributed session support
- Streaming SSE responses with usage statistics
- Model management and pricing configuration
- Log system with consumption tracking
- Email notification support
- Rate limiting (global, per-user, per-IP)
- Channel health monitoring and auto-disable/enable

---

[Unreleased]: https://github.com/tokensplice/gateway/compare/v1.0.0-rc.1...HEAD
[1.0.0-rc.1]: https://github.com/tokensplice/gateway/releases/tag/v1.0.0-rc.1
