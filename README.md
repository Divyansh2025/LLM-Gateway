# LLM Gateway

One OpenAI-compatible endpoint in front of several LLM providers. Apps send every
chat request to the gateway; the gateway picks the provider, and (as the roadmap
lands) fails over when one is down, caches repeated prompts, enforces spend
limits, and records latency and cost for every call.

Written in Go with only the standard library so far.

## Status

Step 1 of the roadmap: a working proxy for OpenAI, Groq and Gemini, with streaming.

## Run it

```sh
export GROQ_API_KEY=...        # any of OPENAI_API_KEY, GROQ_API_KEY, GEMINI_API_KEY
go run ./cmd/gateway           # listens on :8080 (override with GATEWAY_ADDR)
```

```sh
curl localhost:8080/v1/chat/completions \
  -d '{"model":"groq/llama-3.1-8b-instant","messages":[{"role":"user","content":"Say hi"}]}'
```

The `provider/` prefix on `model` picks the upstream; the rest is sent to that
provider as the model name. Every other field (messages, tools, `stream`, ...) is
passed through unchanged, so existing OpenAI SDKs work by changing the base URL.

## Test

```sh
go test -race ./...
```

Tests run against fake upstream servers, so no API keys are needed.

## Layout

| Path | What it does |
| --- | --- |
| `cmd/gateway` | Server startup, timeouts, graceful shutdown |
| `internal/config` | Reads provider keys and base URLs from the environment |
| `internal/proxy` | Routes by model prefix, forwards the request, streams the response back |

## Roadmap

Each step is one pull request with tests. Steps 5 and 7 produce the numbers for the README and resume.

1. **Proxy** — route by `provider/model`, pass streaming through, OpenAI-style errors.
2. **Reliability** — retries with backoff, per-provider timeouts, circuit breaker, and fallback chains (`groq` fails → `gemini`).
3. **Anthropic** — translate OpenAI format to and from the Anthropic Messages API, including streaming.
4. **Usage and cost** — API keys per client, token counts and cost per request stored in Postgres, per-key rate and budget limits in Redis.
5. **Observability** — Prometheus metrics (latency histograms, errors, tokens), OpenTelemetry traces, a Grafana dashboard.
6. **Caching** — exact-match cache, then semantic cache with embeddings, with a measured false-hit rate.
7. **Benchmarks** — k6 load test (gateway overhead p50/p95, max RPS), chaos test killing a provider mid-run, cache savings on a replayed prompt set. Results published here.
8. **Packaging** — Docker Compose for the whole stack, and a short demo video.
