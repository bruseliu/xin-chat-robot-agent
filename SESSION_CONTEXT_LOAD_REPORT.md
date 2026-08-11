# Session Context Load Verification

Run date: 2026-08-11

## Command

```bash
set -a
. ./.env
set +a
go run ./test/session_context_load.go \
  --seed-turns 500 \
  --measure-turns 50 \
  --max-events 48 \
  --max-turns 8 \
  --recent-turns 12 \
  --text-units 16000
```

The verifier creates a timestamped synthetic conversation under the isolated
`session-context-load` app name. It does not read or mutate application user
conversations and does not call LLM, mem0, ASR, or TTS providers.

## Result

Environment: local PostgreSQL configured by `ADK_SESSION_POSTGRES_DSN`.

| Measurement | p50 | p95 | Max |
|---|---:|---:|---:|
| `conversation_begin_turn` | 1.43 ms | 2.55 ms | 2.78 ms |
| `conversation_load_context` | 0.51 ms | 1.03 ms | 1.78 ms |
| `adk_session_get` | 3.01 ms | 4.37 ms | 5.11 ms |

The run inserted 550 completed semantic turns and 1,100 ADK events.

| Bound | Observed | Configured limit |
|---|---:|---:|
| Semantic context turns returned | 12 | 12 |
| Model-visible ADK events | 16 | 48 |
| Events requested from inner ADK session store | 48 | 48 |

Heap allocation after a forced GC was 479,216 bytes lower than before the
measurement loop. This validates that the measured reads do not retain the
500-turn history in the process. The context-load p95 is below the plan's
20 ms target on this local database.

## Scope

This is a sequential local database verification. It validates bounded SQL
requests and process-visible context, but does not measure concurrent database
contention or LLM first-token latency. Production voice logs continue to report
`asr_final_to_llm_first` for provider-inclusive first-token latency.
