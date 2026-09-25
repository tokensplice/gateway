# Security Policy

## Supported Versions

| Version | Supported |
|---|---|
| 1.0.x   | Yes       |
| < 1.0   | No        |

## Reporting a Vulnerability

**Please do NOT report security vulnerabilities through public GitHub issues.**

Instead, use one of these channels:

1. **GitHub Private Vulnerability Reporting** — Go to the [Security Advisories](https://github.com/tokensplice/gateway/security/advisories/new) page and click "Report a vulnerability"
2. **Email** — Send a report to `security@tokensplice.com`

### What to Include

- Description of the vulnerability
- Step-by-step reproduction instructions
- Potential impact assessment
- Suggested fix (if any)

### Response Timeline

- **Acknowledgement**: Within 48 hours
- **Initial Assessment**: Within 5 business days
- **Fix & Disclosure**: Coordinated with the reporter; target within 30 days for critical issues

### Scope

The following are in scope:
- Authentication/authorization bypasses
- Data leaks (user data, API keys, tokens)
- Remote code execution
- SQL injection
- SSRF vulnerabilities
- Cryptographic weaknesses (session, BYOK key storage)
- Privilege escalation

The following are out of scope:
- Denial of service (rate limiting is configurable)
- Social engineering
- Physical access attacks
- Vulnerabilities in upstream dependencies (report to the respective project)

## Security Best Practices for Deployers

If you self-host TokenSplice Gateway:

1. **Always set `SESSION_SECRET` and `CRYPTO_SECRET`** to strong random values (32+ characters). Without them, secrets regenerate on restart, invalidating all sessions and encrypted BYOK keys.
2. **Use HTTPS** — terminate TLS at a reverse proxy (Nginx, Caddy, Cloudflare).
3. **Restrict admin access** — use IP allowlists or VPN for the admin console.
4. **Keep backups** — encrypt MySQL dumps at rest.
5. **Update regularly** — subscribe to release notifications.
6. **Isolate BYOK keys** — the encryption key (`CRYPTO_SECRET`) should never be logged, committed, or shared.

## Past Security Advisories

None published yet.
