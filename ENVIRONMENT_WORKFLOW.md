# Environment Workflow

## Shared In Git

- Application source, tests, deployment scripts, `.env.example`, and product-level development progress.
- `DEVELOPMENT_STATUS.md` records feature behavior, design decisions, migrations, tests, and known product risks only.
- Do not put machine paths, LAN addresses, API keys, model caches, systemd state, audio devices, or one workstation-specific command output in shared progress documents.

## Local To Each Workstation

- `.env`: credentials and local endpoint settings.
- `.local/WORKSTATION.md`: deployment notes, service names, paths, current model cache state, and recovery commands.
- `client/.venv/`, `~/.config/systemd/user/`, downloaded models, PostgreSQL data, and external FunASR or mem0 checkouts.

`.env` and `.local/` are ignored by Git. Each workstation creates its own local files from `.env.example` and this workflow.

## Voice Client Endpoint

`client/voice_client.py` connects directly to `ws://127.0.0.1:8080/v1/chat/voice` by default. This is correct when the client and agent service run on the same workstation. The client disables automatic HTTP proxy use because local and LAN WebSocket traffic must not be sent through a proxy.

For a remote agent service, set the endpoint in the local shell before starting the client:

```bash
export VOICE_SERVER_URL=ws://<server-ip>:8080/v1/chat/voice
export VOICE_USER_ID=<local-user-id>
export VOICE_SESSION_ID=<local-session-id>
```

These values are workstation-specific. Keep them in the ignored local environment or shell profile, not in a tracked document.

## Synchronizing Development

1. Commit only portable source, tests, docs, and deployment artifacts.
2. Before changing code, pull the shared branch and read `DEVELOPMENT_STATUS.md` for product progress.
3. After pulling, keep the local `.env` and `.local/WORKSTATION.md`; do not replace either with another machine's values.
4. When a code change introduces a new environment variable, add a blank documented key to `.env.example`; each workstation fills its own `.env`.
5. Record a feature as complete in `DEVELOPMENT_STATUS.md` only with portable verification commands. Put host-specific validation results in `.local/WORKSTATION.md`.
