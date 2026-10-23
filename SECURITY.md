# Security policy

## Supported versions

OpenBrain is young. Security fixes go into the latest release only; please
upgrade before reporting.

| Version | Supported |
|---------|-----------|
| 0.1.x   | Yes       |

## Reporting a vulnerability

Please report vulnerabilities **privately**, not in a public issue or pull
request:

1. Open the repository's **Security** tab on GitHub and choose **Report a
   vulnerability** (GitHub's private vulnerability reporting).
2. If that option isn't available, open an issue that asks for a private
   contact, without any details of the problem.

Include the version (`openbrain version`), how OpenBrain is deployed (base
path, reverse proxy) and the steps to reproduce. You'll get an acknowledgement,
and a fix or mitigation will be coordinated with you before anything is
disclosed.

## Never post secrets

Don't put real credentials in issues, pull requests, logs or screenshots:
passwords, session cookies, actor tokens (`obt_…`), `OPENBRAIN_TOKEN`, database
files or backups. If one was exposed, rotate it straight away:

- **Actor token:** revoke it on the Agents page or with `openbrain token revoke`.
- **`OPENBRAIN_TOKEN`:** change it in the environment file and restart.
- **Password:** change it under Account; an administrator can also reset it,
  which signs the person out everywhere.

## Hardening

[docs/DEPLOYMENT.md](docs/DEPLOYMENT.md) covers the recommended setup: HTTPS
in front, OpenBrain on loopback only, a firewall, and the sandboxed systemd
unit.
