# Contributing to TokenSplice Gateway

Thank you for your interest in contributing! This project is licensed under AGPLv3 — by contributing, you agree that your contributions will be licensed under the same license.

## Development Setup

### Prerequisites

- Go 1.25+
- Bun 1.1+ (frontend)
- Docker & Docker Compose (for local services)
- MySQL 8.0+ or SQLite (default)
- Redis 7+ (optional, for caching)

### Getting Started

```bash
# Clone
git clone https://github.com/tokensplice/gateway.git
cd gateway

# Backend dependencies
go mod download

# Frontend dependencies
cd web && bun install && cd ..

# Copy environment config
cp .env.example .env
# Edit .env — at minimum set SESSION_SECRET and CRYPTO_SECRET

# Run backend (development mode)
go run . --port 3000

# Run frontend (separate terminal, hot-reload)
cd web && bun run dev
```

### Running Tests

```bash
# Go unit tests
go test ./...

# Go tests with race detector
go test -race ./...

# Frontend lint
cd web && bun run lint
```

## Making Changes

### Branch Naming

- `feat/short-description` — new features
- `fix/short-description` — bug fixes
- `docs/short-description` — documentation only
- `refactor/short-description` — code restructuring

### Commit Messages

Follow [Conventional Commits](https://www.conventionalcommits.org/):

```
feat(byok): add BYOK key management API
fix(routing): prevent nil pointer on channel failover
docs: update deployment guide for Docker Compose v2
```

### Pull Request Process

1. Fork the repository
2. Create a feature branch from `main`
3. Make your changes with tests
4. Ensure `go test ./...` and `bun run lint` pass
5. Open a PR against `main` with a clear description
6. Wait for review

### Code Style

- **Go**: Follow standard Go conventions. Run `gofmt` before committing.
- **TypeScript/React**: Follow the existing ESLint/Prettier config in `web/`.
- **Database**: New migrations go in the appropriate model files. Never break backward compatibility with existing data.

## Important Notes

### AGPLv3 Obligations

All contributions to this repository are licensed under AGPLv3. This means:
- Your code will be publicly available
- Derivative works must also be AGPLv3
- Network use triggers source disclosure obligations (§13)

### Upstream Syncing

This project is a fork of [QuantumNous/new-api](https://github.com/QuantumNous/new-api). We periodically sync with upstream. To minimize conflicts:
- Keep changes modular and well-documented
- Prefer configuration over hard-coding
- Add comments explaining TokenSplice-specific modifications

### What We Accept

- Bug fixes
- New model/provider adapters
- Performance improvements
- Documentation improvements
- Security fixes
- UI/UX improvements

### What We Don't Accept (without discussion)

- Breaking changes to the public API
- Features that violate upstream provider ToS
- Code that removes AGPLv3 licensing
- Large refactors without prior discussion (open an issue first)

## Getting Help

- Open an [issue](https://github.com/tokensplice/gateway/issues) for bugs or feature requests
- Start a [discussion](https://github.com/tokensplice/gateway/discussions) for questions

## License

By contributing, you agree that your contributions will be licensed under the [GNU Affero General Public License v3.0](LICENSE).
