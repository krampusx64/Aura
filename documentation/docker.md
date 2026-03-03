# AuraGo — Docker Installation

Run AuraGo as a Docker container with a single command. No Go, Python, or ffmpeg installation required on the host.

## Prerequisites

- [Docker Engine](https://docs.docker.com/engine/install/) 20.10+ (or Docker Desktop)
- [Docker Compose](https://docs.docker.com/compose/install/) v2+ (`docker compose` — usually included with Docker Desktop)

---

## Quick Start

```bash
# 1. Clone the repository
git clone https://github.com/antibyte/AuraGo.git
cd AuraGo

# 2. Configure your API keys
#    Edit config.yaml and fill in at least llm.api_key
nano config.yaml

# 3. Build and start
docker compose up -d

# 4. View logs
docker compose logs -f
```

AuraGo is now running at **http://localhost:8088**.

---

## Step-by-Step Guide

### 1. Clone & Enter Directory

```bash
git clone https://github.com/antibyte/AuraGo.git
cd AuraGo
```

### 2. Configure

Edit `config.yaml` in the project root. At minimum you need:

```yaml
server:
  host: "0.0.0.0"          # Important: bind to 0.0.0.0 inside the container
  port: 8088

llm:
  api_key: "sk-your-api-key-here"
```

> **Tip:** For Docker, set `host: "0.0.0.0"` instead of the default `127.0.0.1`, otherwise the Web UI won't be reachable from outside the container.

Optional integrations — enable as needed:

```yaml
telegram:
  bot_token: "123456:ABC..."
  telegram_user_id: 123456789

discord:
  enabled: true
  bot_token: "..."

home_assistant:
  enabled: true
  url: "http://host.docker.internal:8123"  # ← Access host services from inside Docker
  access_token: "..."

docker:
  enabled: true
  host: "unix:///var/run/docker.sock"       # ← Manage Docker from inside the container
```

### 3. Build & Start

```bash
docker compose up -d
```

This will:
1. Build the Go binary in a multi-stage build (no Go needed on host)
2. Create a slim Python 3.12 + ffmpeg runtime image
3. Start the container with automatic restart

### 4. Verify

```bash
# Check status
docker compose ps

# View live logs
docker compose logs -f

# Open Web UI
# http://localhost:8088
```

---

## Volumes & Persistence

The `docker-compose.yml` defines two named volumes that survive container restarts and rebuilds:

| Volume | Container Path | Purpose |
|---|---|---|
| `aurago_data` | `/app/data` | Memory, chat history, state, vector DB, SQLite databases |
| `aurago_workdir` | `/app/agent_workspace/workdir` | Python venv, generated tools, scratch files |

Your `config.yaml` is bind-mounted as read-only.

### Backup Volumes

```bash
# Backup data volume
docker run --rm -v aurago_data:/data -v $(pwd):/backup alpine tar czf /backup/aurago_data_backup.tar.gz -C /data .

# Restore data volume
docker run --rm -v aurago_data:/data -v $(pwd):/backup alpine tar xzf /backup/aurago_data_backup.tar.gz -C /data
```

---

## Docker-in-Docker (Agent manages Docker)

If you want the AuraGo agent to manage Docker containers on the host, mount the Docker socket:

Add this to `docker-compose.yml` under `volumes:`:

```yaml
volumes:
  - ./config.yaml:/app/config.yaml:ro
  - aurago_data:/app/data
  - aurago_workdir:/app/agent_workspace/workdir
  - /var/run/docker.sock:/var/run/docker.sock   # ← Add this line
```

And enable it in `config.yaml`:

```yaml
docker:
  enabled: true
  host: "unix:///var/run/docker.sock"
```

> **Security Note:** Mounting the Docker socket gives the container full control over the Docker daemon. Only do this in trusted environments.

---

## Environment Variables

| Variable | Description |
|---|---|
| `AURAGO_MASTER_KEY` | Encryption key for the secrets vault. Set a stable value (`openssl rand -hex 32`) so secrets persist across rebuilds. |

Set it in a `.env` file next to `docker-compose.yml`:

```bash
echo "AURAGO_MASTER_KEY=$(openssl rand -hex 32)" > .env
```

Or export it directly:

```bash
export AURAGO_MASTER_KEY="your-64-char-hex-key"
docker compose up -d
```

---

## Common Operations

### Rebuild after code changes

```bash
docker compose up -d --build
```

### Stop

```bash
docker compose down
```

### Stop and delete all data

```bash
docker compose down -v
```

### View resource usage

```bash
docker stats aurago
```

### Enter the container (debugging)

```bash
docker compose exec aurago /bin/bash
```

---

## Port Configuration

The default port is **8088**. To change it, edit both files:

**docker-compose.yml:**
```yaml
ports:
  - "3000:8088"   # Host port 3000 → Container port 8088
```

Or match both sides if you also change `config.yaml`:

**config.yaml:**
```yaml
server:
  port: 3000
```

**docker-compose.yml:**
```yaml
ports:
  - "3000:3000"
```

---

## Accessing Host Services

To reach services running on the Docker host (e.g. Home Assistant, Ollama, local databases):

| Platform | Host Address |
|---|---|
| Docker Desktop (Mac/Windows) | `host.docker.internal` |
| Linux (Docker 20.10+) | Add `extra_hosts: ["host.docker.internal:host-gateway"]` to compose |

Example for Linux — add to `docker-compose.yml`:

```yaml
services:
  aurago:
    extra_hosts:
      - "host.docker.internal:host-gateway"
```

Then use `http://host.docker.internal:8123` for Home Assistant, `http://host.docker.internal:11434` for Ollama, etc.

---

## Troubleshooting

### Container starts but Web UI is not reachable

Make sure `config.yaml` has `host: "0.0.0.0"`:
```yaml
server:
  host: "0.0.0.0"
```
`127.0.0.1` only listens inside the container.

### Permission denied on Docker socket

```bash
# Add your user to the docker group
sudo usermod -aG docker $USER
# Then log out and back in

# Or temporarily fix permissions
sudo chmod 666 /var/run/docker.sock
```

### Python skills fail / pip packages missing

The Python venv is stored in the `aurago_workdir` volume. If corrupted:

```bash
# Delete the workdir volume and restart (venv will be recreated)
docker compose down
docker volume rm aurago_aurago_workdir
docker compose up -d
```

### Out of disk space

```bash
# Clean unused Docker resources
docker system prune -a --volumes
```

### View full startup logs

```bash
docker compose logs --no-log-prefix aurago 2>&1 | head -100
```
