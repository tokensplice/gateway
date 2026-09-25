# TokenSplice Gateway

**One endpoint. Every model.**

TokenSplice Gateway is a unified AI inference gateway that provides a single OpenAI-compatible API endpoint for accessing 100+ LLM models from multiple providers. Built on [new-api](https://github.com/QuantumNous/new-api) (AGPLv3).

[![License: AGPL v3](https://img.shields.io/badge/License-AGPLv3-blue.svg)](https://www.gnu.org/licenses/agpl-3.0)
[![Go](https://img.shields.io/badge/Go-1.25+-00ADD8?logo=go)](https://go.dev/)
[![React](https://img.shields.io/badge/React-19-61DAFB?logo=react)](https://react.dev/)

## Features

- **OpenAI-Compatible API** — Change one line (`base_url`) in your existing code to access all supported models
- **Multi-Provider Routing** — Intelligent load balancing, automatic failover, health-based channel selection
- **BYOK (Bring Your Own Key)** — Connect your own provider API keys and pay only a 5% platform routing fee
- **Token-Based Billing** — Fine-grained per-model pricing with multi-currency display
- **Unified Management** — Users, tokens, channels, quotas, logs, and analytics in one admin console
- **100+ Models** — OpenAI, Anthropic, Google, DeepSeek, Qwen, Llama, Mistral, and more
- **Streaming Support** — Full SSE streaming with usage statistics
- **Docker Ready** — One-command deployment with docker-compose

## Quick Start

### Docker (Recommended)

```bash
# Clone the repository
git clone https://github.com/tokensplice/gateway.git
cd gateway

# Configure environment
cp .env.example .env
# Edit .env with your settings (database, secrets, branding)

# Start services
docker compose up -d

# Access the console
open http://localhost:3000
```

### Build from Source

```bash
# Backend
go build -o gateway .

# Frontend
cd web && bun install && bun run build && cd ..

# Run
./gateway --port 3000
```

## Configuration

All configuration is via environment variables. See [.env.example](.env.example) for the full list.

Key variables:

| Variable | Description | Default |
|---|---|---|
| `SYSTEM_NAME` | Platform display name | `TokenSplice Gateway` |
| `LOGO_URL` | Logo image URL | (empty) |
| `SESSION_SECRET` | Session encryption key | (random per boot — **set this!**) |
| `CRYPTO_SECRET` | Data encryption key | (falls back to SESSION_SECRET) |
| `SQL_DSN` | MySQL connection string | (SQLite fallback) |
| `REDIS_CONN_STRING` | Redis connection | (disabled) |
| `PORT` | Listening port | `3000` |

> **Critical**: Always set `SESSION_SECRET` and `CRYPTO_SECRET` in production. Without them, secrets are regenerated on every restart, invalidating all sessions and encrypted data.

## API Usage

```python
from openai import OpenAI

client = OpenAI(
    api_key="sk-your-tokensplice-token",
    base_url="https://your-instance.com/v1"
)

response = client.chat.completions.create(
    model="gpt-4o",
    messages=[{"role": "user", "content": "Hello!"}]
)
print(response.choices[0].message.content)
```

## Architecture

```
┌─────────────────────────────────────────────┐
│              TokenSplice Gateway             │
│                                             │
│  ┌─────────┐  ┌──────────┐  ┌───────────┐ │
│  │ Router  │→ │ Channels │→ │ Upstream  │ │
│  │ /v1/*   │  │ (N prov.)│  │ Providers │ │
│  └─────────┘  └──────────┘  └───────────┘ │
│       ↕              ↕                      │
│  ┌─────────┐  ┌──────────┐                 │
│  │  MySQL  │  │  Redis   │                 │
│  └─────────┘  └──────────┘                 │
└─────────────────────────────────────────────┘
```

## License

This project is licensed under the **GNU Affero General Public License v3.0** (AGPLv3).

- Full license text: [LICENSE](LICENSE)
- Third-party licenses: [THIRD-PARTY-LICENSES.md](THIRD-PARTY-LICENSES.md)
- Open source notice: [NOTICE](NOTICE)

This project is a derivative work of [new-api](https://github.com/QuantumNous/new-api) by QuantumNous, which is itself derived from [one-api](https://github.com/songquanpeng/one-api) by JustSong. All are licensed under AGPLv3.

Per AGPLv3 §13, if you interact with a modified version of this program over a network, you are entitled to receive the Corresponding Source. The complete source code is available at this repository.

## Contributing

See [CONTRIBUTING.md](CONTRIBUTING.md) for guidelines.

## Security

See [SECURITY.md](SECURITY.md) for vulnerability reporting.

## Links

- **Website**: https://tokensplice.com
- **Documentation**: https://docs.tokensplice.com
- **Issues**: https://github.com/tokensplice/gateway/issues
