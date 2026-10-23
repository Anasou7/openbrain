# Deployment

This guide installs OpenBrain as a hardened systemd service on a Linux server,
puts it behind a reverse proxy, creates the first account, and sets up
backups. It assumes a systemd-based distribution (Debian, Ubuntu, Fedora, Arch
and so on) and root access. Docker is not required.

The shape is always the same:

```
browser / agent ──HTTPS──▶ reverse proxy (:443) ──▶ OpenBrain (127.0.0.1:8080) ──▶ SQLite file
```

OpenBrain never needs to listen on a public address.

## 1. Build

Build anywhere with Go 1.25 or newer. An older Go, such as Debian 13's 1.24,
fetches the right toolchain by itself (`GOTOOLCHAIN=auto` is the default). The
binary is static, so you can build on a laptop and copy it over.

```sh
git clone https://github.com/Anasou7/openbrain.git && cd openbrain
go test ./...
CGO_ENABLED=0 go build -trimpath -ldflags="-s -w" -o openbrain ./cmd/openbrain
# Cross-compile, e.g. for an ARM server:
# GOOS=linux GOARCH=arm64 CGO_ENABLED=0 go build -trimpath -ldflags="-s -w" -o openbrain ./cmd/openbrain
```

## 2. Install the service

```sh
sudo install -m 0755 openbrain /usr/local/bin/openbrain

# Service user. systemd creates and owns /var/lib/openbrain via StateDirectory=.
sudo useradd --system --home-dir /var/lib/openbrain --no-create-home \
  --shell /usr/sbin/nologin openbrain

# Configuration.
sudo install -d -m 0750 -o root -g openbrain /etc/openbrain
sudo install -m 0640 -o root -g openbrain deploy/openbrain.env.example /etc/openbrain/env
sudoedit /etc/openbrain/env          # set OPENBRAIN_BASE_PATH (step 3)

# Service and nightly backup.
sudo install -m 0644 deploy/openbrain.service deploy/openbrain-backup.service \
  deploy/openbrain-backup.timer /etc/systemd/system/
sudo systemctl daemon-reload
sudo systemctl enable --now openbrain.service openbrain-backup.timer

curl -s http://127.0.0.1:8080/ready              # base path "/"
curl -s http://127.0.0.1:8080/openbrain/ready    # base path "/openbrain"
```

The unit is heavily sandboxed. It can write only to its state directory and
has no capabilities. `systemd-analyze security openbrain` rates it about 1.4,
"OK", where lower is better.

Run administrative commands (`openbrain user …`, `actor …`, `token …`,
`backup`) as the service user, so any files SQLite creates stay owned by it:

```sh
sudo -u openbrain env OPENBRAIN_DB=/var/lib/openbrain/openbrain.db openbrain user list
```

## 3. Choose the URL and configure the proxy

OpenBrain can own a host, or share one with other sites under a path prefix.
**`OPENBRAIN_BASE_PATH` and the proxy must agree**, and the proxy must pass
the prefix through unchanged (don't strip it).

| Layout | URL | `OPENBRAIN_BASE_PATH` |
|--------|-----|-----------------------|
| Own host | `https://brain.example.com/` | `/` |
| Shared host | `https://example.com/openbrain/` | `/openbrain` |

The base path covers every application URL: the dashboard, `/login`,
`/setup`, `/logout`, static files, `/api/...`, `/mcp`, `/health` and `/ready`.
The session cookie is scoped to the base path. Requests outside it get 404,
and the bare prefix (`/openbrain`) redirects to `/openbrain/`.

**HTTPS.** A domain name is the recommended setup: any proxy can get a
standard certificate for it, and it survives a change of server. Without a
domain, a publicly trusted certificate for the server's IP address also works
(see the Caddy section below). Either way, the proxy terminates HTTPS and
talks to OpenBrain over loopback.

### Caddy (automatic HTTPS)

```caddy
# Own host
brain.example.com {
	reverse_proxy 127.0.0.1:8080
}

# Shared host: "handle" keeps the /openbrain prefix, as OpenBrain expects.
# path_regexp is case-sensitive, so only /openbrain/... reaches OpenBrain.
example.com {
	redir /openbrain /openbrain/ permanent
	@openbrain path_regexp ^/openbrain/
	handle @openbrain {
		reverse_proxy 127.0.0.1:8080
	}
	handle {
		# other sites
	}
}
```

No domain? Let's Encrypt also issues publicly trusted certificates for a
public IP address, valid for six days, which Caddy 2.11+ obtains and renews
by itself and swaps in without a restart. Use the IP as the site address and
request the `shortlived` profile; variant C in the example file shows it.
Without that profile, Caddy uses its own CA for IP addresses, and browsers
show a warning. Clients must then use the IP in the URL, e.g.
`https://203.0.113.10/openbrain/`.

### nginx

```nginx
location /openbrain/ {
    proxy_pass http://127.0.0.1:8080;          # no URI here: the prefix is passed through
    proxy_set_header Host $host;
    proxy_set_header X-Forwarded-For $proxy_add_x_forwarded_for;
    proxy_set_header X-Forwarded-Proto $scheme;
}
location = /openbrain { return 301 /openbrain/; }
```

Full examples, including the own-host layout, are in
[deploy/Caddyfile.example](../deploy/Caddyfile.example) and
[deploy/nginx.conf.example](../deploy/nginx.conf.example).

### Trusted proxies

OpenBrain uses the client address for sign-in rate limiting, and the scheme
for `Secure` cookies. It believes `X-Forwarded-For` and `X-Forwarded-Proto`
**only** from `OPENBRAIN_TRUSTED_PROXIES` (default `127.0.0.1,::1`, a proxy on
the same machine). It takes the right-most address in the chain that isn't a
trusted proxy, so a client can't spoof its address by sending its own header.
If your proxy runs elsewhere, add its address or CIDR.

### Other ways in

- **Tailscale:** run `sudo tailscale serve --bg 8080` with
  `OPENBRAIN_BASE_PATH=/`. You get `https://<machine>.<tailnet>.ts.net/`,
  private to your tailnet, with HTTPS.
- **SSH tunnel:** run `ssh -N -L 8080:127.0.0.1:8080 you@server`, then open
  `http://127.0.0.1:8080/` (plus the base path). The traffic is encrypted by
  SSH.
- **WireGuard or a private LAN:** set `OPENBRAIN_ADDR` to the private
  interface address and firewall everything else.

> **Never send account credentials over plain HTTP across the public
> internet.** `http://<server-ip>/openbrain` is acceptable only on a trusted
> LAN, a private overlay network or for a temporary local setup. For anything
> reachable from the internet, use HTTPS (Caddy does it automatically) or
> Tailscale. OpenBrain shows a warning on its sign-in and setup pages when the
> connection is unencrypted and not to a loopback address. Configure agents
> with the `https://` URL: with `http://`, the bearer token crosses the network
> before any redirect happens.

### Firewall

Allow only what the server needs: SSH (22), HTTP (80, for certificate
challenges and the redirect to HTTPS) and HTTPS (443). Never expose
OpenBrain's own port (8080 by default). It already listens on loopback only;
the firewall is the second layer.

With ufw:

```sh
sudo ufw default deny incoming
sudo ufw allow 22/tcp && sudo ufw allow 80/tcp && sudo ufw allow 443/tcp
sudo ufw enable
```

Or with plain nftables, in `/etc/nftables.conf` (then
`sudo systemctl enable --now nftables`):

```
table inet host_firewall
delete table inet host_firewall
table inet host_firewall {
	set tcp_open { type inet_service; elements = { 22, 80, 443 } }
	chain input {
		type filter hook input priority filter; policy drop;
		ct state established,related accept
		ct state invalid drop
		iif lo accept
		meta l4proto { icmp, ipv6-icmp } accept
		tcp dport @tcp_open accept
	}
}
```

Replacing only its own table means rules added by Docker or Tailscale survive
a reload. **Before enabling a firewall on a remote machine, make sure SSH is
allowed**, and keep your current session open until a new SSH login works.

## 4. First-run setup and accounts

Open the URL. With no accounts yet, every page redirects to `/setup`, where
you create the first account. It is always an administrator, and you are
signed in immediately.

Once that account exists, `/setup` redirects to sign-in and anonymous setup
attempts are refused. Creating the first account is a single database
transaction, so two simultaneous setups can't both succeed.

**Complete setup promptly after making the service reachable.** Until then,
whoever opens `/setup` first becomes the administrator. To avoid that window
entirely, create the administrator from the shell before exposing the
service:

```sh
sudo -u openbrain env OPENBRAIN_DB=/var/lib/openbrain/openbrain.db \
  openbrain user create alice --name "Alice" --admin     # prints a generated password once
```

**Roles.** Administrators manage users (System page), actors and tokens
(Agents page). Users have normal dashboard access. Every account writes as its
own human actor, so dashboard notes, handoffs and edits are attributed to the
person who made them.

**Passwords** are hashed with Argon2id. Use at least 8 characters;
passphrases and password managers are welcome. People change their own
password under Account, which also signs out their other sessions.

**Sessions** are server-side and survive restarts:
- They expire after 30 days unused, and after 90 days at most.
- Signing out ends the session.
- An administrator's password reset or disabling an account ends that
  person's sessions immediately.

**Recovery.** If you lose the only administrator password:

```sh
sudo -u openbrain env OPENBRAIN_DB=/var/lib/openbrain/openbrain.db openbrain user reset-password alice
```

**Agents** don't have accounts. Give each agent machine its own token on the
Agents page, or with `openbrain token create`. [MCP.md](MCP.md) explains the
rest.

### Credentials

OpenBrain has three kinds of credential, each for one kind of client:

| Who | Credential | Used for | Writes are attributed to |
|-----|------------|----------|--------------------------|
| People | Username and password, then a browser session | The dashboard | Their own human actor |
| Agents (Claude, Codex, OpenCode, Hermes, …) | Their own actor token, `obt_…` | MCP (and the REST API) | That agent |
| Administrative scripts | `OPENBRAIN_TOKEN` (optional) | The REST API only | Whichever actor the request names, logged as `admin-api` |

They never substitute for each other: a browser session doesn't authenticate
the API or MCP, and `/mcp` refuses `OPENBRAIN_TOKEN`.

**`OPENBRAIN_TOKEN`** is a powerful machine credential. With it, a script can
register actors and write as any actor through the REST API. People using the
browser don't need it, and neither do agents. Set it only if a script needs
it:

- Generate it with `openssl rand -hex 32` (at least 32 characters) and put it
  in `/etc/openbrain/env`, which stays `root:openbrain`, mode `0640`.
- Keep it out of agent configuration, repositories and chat. When attribution
  matters, which it usually does, give the script its own actor token instead.
- To rotate it, change the value and run `systemctl restart openbrain`. To
  disable it, remove the line.

## 5. Backups

`openbrain backup` writes a consistent snapshot using SQLite's `VACUUM INTO`
while the server keeps running, then integrity-checks the copy. **Never `cp` a
live database.** Recent commits sit in the `-wal` file, and a plain copy can be
inconsistent.

The bundled timer runs nightly, keeps 14 days of `openbrain-*.db` files in
`/var/lib/openbrain/backups/`, and runs with idle I/O priority.

```sh
systemctl list-timers openbrain-backup.timer   # next run
sudo systemctl start openbrain-backup.service  # back up now
```

Backups on the same disk don't survive losing the disk. Copy the `backups/`
directory off the machine, for example with restic, rsync or rclone. The
files are self-contained SQLite databases, and they include password and token
hashes, so store them as carefully as the server.

### Restore

```sh
sudo systemctl stop openbrain
sudo -u openbrain env OPENBRAIN_DB=/var/lib/openbrain/openbrain.db \
  openbrain restore /var/lib/openbrain/backups/openbrain-20260102T030000Z.db
sudo systemctl start openbrain
```

`restore` does four things:
1. Verifies the backup (integrity check and schema version).
2. Saves the current database as `openbrain.db.pre-restore-<time>`.
3. Copies the backup in with SQLite's online backup API.
4. Applies any migrations the backup predates.

It refuses backups from a newer OpenBrain version.

## 6. Upgrades

```sh
sudo systemctl start openbrain-backup.service       # back up first
sudo cp -p /usr/local/bin/openbrain /usr/local/bin/openbrain.prev
sudo install -m 0755 openbrain /usr/local/bin/openbrain
sudo systemctl restart openbrain
journalctl -u openbrain -n 5    # "applied migrations" if the schema changed
```

Migrations run at startup, each in its own transaction, and are additive. An
older binary refuses to open a database migrated by a newer one, so roll back
by restoring the pre-upgrade backup with the previous binary.

### Upgrading from Project Brain

OpenBrain is the renamed Project Brain. The upgrade is additive: migration 3
adds the `users` and `user_sessions` tables, and existing data and agent
tokens keep working.

| Project Brain | OpenBrain |
|---------------|-----------|
| `project-brain` binary and `project-brain.service` | `openbrain` and `openbrain.service` |
| `/var/lib/project-brain/brain.db` | `/var/lib/openbrain/openbrain.db` (any path works via `OPENBRAIN_DB`) |
| `/etc/project-brain/env` with `BRAIN_*` variables | `/etc/openbrain/env` with `OPENBRAIN_*`. The old names are still read, with a warning. |
| `BRAIN_TOKEN`, used for sign-in and the admin API | `OPENBRAIN_TOKEN`, optional, admin API only. People sign in with accounts. |
| Actor tokens `pbt_…` | Still valid. New tokens start with `obt_`. |
| Dashboard actor `user` | Claimed by the first account you create, and renamed to its username, so earlier dashboard history stays with you. |

Steps:

```sh
sudo systemctl start project-brain-backup.service              # 1. back up
sudo systemctl disable --now project-brain-backup.timer project-brain.service
sudo useradd --system --home-dir /var/lib/openbrain --no-create-home --shell /usr/sbin/nologin openbrain
sudo mv /var/lib/project-brain /var/lib/openbrain              # 2. move data, rename the file
cd /var/lib/openbrain && for e in "" -wal -shm; do [ -e brain.db$e ] && sudo mv brain.db$e openbrain.db$e; done
sudo chown -R openbrain:openbrain /var/lib/openbrain
sudo install -d -m 0750 -o root -g openbrain /etc/openbrain    # 3. config (rename BRAIN_ → OPENBRAIN_)
sudo sed 's/^BRAIN_/OPENBRAIN_/; s#/var/lib/project-brain/brain.db#/var/lib/openbrain/openbrain.db#' \
  /etc/project-brain/env | sudo install -m 0640 -o root -g openbrain /dev/stdin /etc/openbrain/env
sudo install -m 0755 openbrain /usr/local/bin/openbrain        # 4. new binary and units
sudo install -m 0644 deploy/openbrain*.service deploy/openbrain-backup.timer /etc/systemd/system/
sudo rm /etc/systemd/system/project-brain*.service /etc/systemd/system/project-brain-backup.timer
sudo systemctl daemon-reload && sudo systemctl enable --now openbrain.service openbrain-backup.timer
```

Then open `/setup` (under your base path) to create your account.

**Rollback.** Stop `openbrain` and move the data directory back. Put the old
env file and units back, and restore the pre-upgrade backup with the old
binary, because it refuses schema 3. Older `brain-*.db` backups are kept: the
pruning only touches `openbrain-*.db` files.

## 7. Operations

- **Logs:** `journalctl -u openbrain`.
  - Startup reports the migration status, the base path and whether setup is
    pending.
  - Sign-ins (by internal user ID), failed sign-ins and rate limiting (client
    address and reason only), writes, errors and authentication failures are
    logged. Submitted usernames, passwords, cookies and tokens never are:
    people sometimes paste a password into the username field.
  - Successful reads are not logged unless `OPENBRAIN_LOG_LEVEL=debug`.
- **Health:** `<base>/health` reports liveness. `<base>/ready` returns 503
  if the database is unreachable or not migrated.
- **Sign-in throttling:** after 5 failed attempts for one username from one
  client, or 20 from one client in total, sign-in waits up to 15 minutes. The
  counters are in memory and reset on restart.
- **Rotate an agent's token:** run `openbrain token create` for the same
  actor, update the agent, then `openbrain token revoke <old id>`.
- **Graceful shutdown:** on SIGTERM, in-flight requests get up to 10 seconds
  to finish.

## Resource use

Measured on a 2 vCPU / 4 GB Debian 13 VM, with the release build running
under the unit's limits:

| Situation | RSS | cgroup memory |
|-----------|-----|---------------|
| Idle after start | 20 MB | 8 MB |
| 600 dashboard page views and 1,400 MCP calls, 16 concurrent, 25 projects | 30 MB | 20 MB |
| Burst of 80 sign-ins, 8 concurrent (Argon2id, 12 MiB each) | 47 MB peak | 36 MB peak |
| Settled after load | 28–31 MB | 18–19 MB |

- **CPU:** idle use is zero, because there are no background jobs or polling.
- **The reverse proxy** has its own footprint: Caddy 2.11 used about 52 MB RSS
  on the same VM.
- **Latency (p50):**
  - Dashboard overview: ~4 ms
  - Project page: ~5 ms
  - MCP `get_project_context`: ~3 ms
  - Sign-in: ~55 ms. Argon2id is deliberately slow, and each hash's 12 MiB is
    reclaimed immediately so bursts don't grow the heap.
- **Page weight:**
  - Overview with 25 projects: 43 KB of HTML
  - CSS: 26 KB
  - JavaScript: 2 KB
  - Fonts: 42 KB, cached for a year

| Setting | Value | Why |
|---------|-------|-----|
| `MemoryHigh` | 64M | Above the measured peaks. Past it the kernel reclaims (mostly SQLite page cache) instead of killing anything. |
| `MemoryMax` | 96M | Hard ceiling, well under 100 MB. |
| `GOMEMLIMIT` | 48MiB | Makes Go's garbage collector work harder well before the cgroup limit. |
| `GODEBUG=disablethp=1` | | Stops transparent huge pages from inflating the small heap on THP=`always` hosts (settled RSS 30 MB → 24 MB in testing). |
| `CPUWeight=50`, `CPUQuota=50%`, `Nice=10` | | Yields to other services and never takes more than half a core. |
| `IOSchedulingPriority=7`, `TasksMax=64` | | Low I/O priority. Normal operation uses about 10 threads. |
