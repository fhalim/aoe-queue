# aoe-queue

Command-line tool to enqueue and send commands to Agent of Empires (AOE) sessions via HTTP API. Handles Claude throttling gracefully by detecting rate-limit messages and backing off until reset time.

## Features

- **Message queue**: SQLite-backed queue persists commands across restarts
- **Throttle-aware**: Detects "You've hit your limit · resets X:XXam (UTC)" messages
- **Smart backoff**: Waits 50% of time until reset before retrying
- **Polling**: Detects session idle by monitoring output stability
- **Multiple sessions**: Run separate queues for different AOE sessions

## Installation

```bash
go build -o aoe-queue ./
```

## Usage

### Enqueue commands
```bash
./aoe-queue enqueue --session sess_abc123 "command 1" "command 2"
```

### Process queue
```bash
./aoe-queue run --session sess_abc123 --token your_token
```

Options:
- `--url` — AOE API base URL (default: `http://localhost:8080`)
- `--db` — SQLite database path (default: `aoe-queue.db`)

### List pending commands
```bash
./aoe-queue list --session sess_abc123
./aoe-queue list  # all sessions
```

## Environment Variables

- `AOE_SESSION` — Session ID
- `AOE_TOKEN` — API token
- `AOE_URL` — Base URL (overrides `--url` flag)

## API

The tool uses the AOE HTTP API:
- `POST /api/sessions/{id}/send` — Send message
- `GET /api/sessions/{id}/output` — Get session output snapshot

See https://www.agent-of-empires.com/docs/api/ for details.
