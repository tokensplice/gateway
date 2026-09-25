# Changelog

All notable changes to TokenSplice Gateway will be documented in this file.

Format follows [Keep a Changelog](https://keepachangelog.com/en/1.1.0/).
Versioning follows [Semantic Versioning](https://semver.org/spec/v2.0.0.html).

## [Unreleased]

### Added
- Multi-currency billing display (rc.3): internal billing stays in quota, but prices and balances can be shown in the user's preferred currency
  - `currency_rates` table with unique (base, target) pairs, auto-seeded on first boot with USD→HKD/CNY/EUR/GBP/JPY/KRW/SGD/TWD default rates
  - Automatic exchange rate refresh from `open.er-api.com` every 24 hours via the system task framework; on failure existing rates are kept and flagged `rate_ok=false`; admin-set manual rates are never overwritten by auto-update
  - Admin API: `GET/POST /api/currency/rates`, `DELETE /api/currency/rates/:id`, `POST /api/currency/rates/refresh`; public `GET /api/currency/supported` for the frontend currency dropdown
  - Per-user `preferred_currency` setting (default `USD`), updatable via `PUT /api/user/self` and returned by `GET /api/user/self` along with converted `quota_display` / `used_quota_display` amounts
  - `common.ConvertQuotaToCurrency` / `common.FormatCurrency` display conversion helpers with rate-unavailable fallback (rate 1.0, `rate_ok=false`)
  - `/api/pricing` now includes `display_currency`, `default_display_currency`, and the `currency_rates` table; `/api/status` includes `default_display_currency` and `supported_display_currencies`
  - Response headers `X-Quota-USD`, `X-Quota-Display`, `X-Display-Currency`, and `X-Quota-Display-Reliable` on `GET /api/user/self` and the `/dashboard/billing/*` endpoints
  - Environment variables `CURRENCY_AUTO_UPDATE` (default `true`), `CURRENCY_UPDATE_INTERVAL_HOURS` (default `24`), and `DEFAULT_DISPLAY_CURRENCY` (default `USD`)
- Observability: a Prometheus endpoint at `GET /metrics` on the main HTTP port, outside every authentication group and optionally protected by a `METRICS_TOKEN` bearer credential. It exposes relay request counts, durations, and time to first token by model, provider, and route; token and quota throughput; BYOK attempts, fee revenue, active keys, and key failures; managed channel health, attempts, and latency; and active connections
- Request metrics middleware covering every endpoint, labelled by the route pattern rather than the raw URL so an unauthenticated caller cannot create unbounded series by requesting random paths
- Grafana dashboard template at `deploy/grafana/tokensplice-gateway.json`, importable as-is against a Prometheus data source
- Environment variables `METRICS_ENABLED` (default `true`) and `METRICS_TOKEN` (default empty, meaning the endpoint is open for local scraping)
- BYOK relay integration: a request whose model matches a customer's own provider key is routed through that key and charged only the platform fee (a percentage of the notional managed-channel price), with seamless failover to managed channels when the key fails
- BYOK usage recording: every attempt, successful or not, writes a `byok_usage` row and an upstream 401/403 marks the key invalid
- Brand configuration via environment variables (`SYSTEM_NAME`, `LOGO_URL`, `FOOTER`, `TOP_UP_LINK`)
- Project documentation: CONTRIBUTING.md, SECURITY.md, .env.example
- GitHub Actions CI workflow for lint, test, and build verification

### Changed
- `byok_fee_override` is now part of the cached user record, so resolving the BYOK platform fee costs no extra database read on the relay path
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
