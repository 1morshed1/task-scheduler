# 09 · The Dashboard — Why Server-Sent Events, not WebSockets

The spec says "Dashboard (Go + WebSocket)". We built it with **Server-Sent
Events (SSE)** instead. This doc explains that choice, because *choosing the
simplest tool that fits* is a skill worth as much as knowing the fancy one.
Code: `internal/dashboard/`.

## What the dashboard actually needs

Look at the data flow:

- **Server → browser:** task counts, worker health, DLQ depth — pushed live,
  every couple of seconds. This is 99% of the dashboard.
- **Browser → server:** exactly one action — clicking "Retry" on a DLQ entry.

So the *streaming* need is entirely one-directional. The browser never needs to
stream anything back up the live channel; the single write action is a normal
request/response.

## SSE vs WebSocket in one table

| | WebSocket | Server-Sent Events |
|---|---|---|
| Direction | full-duplex (both ways) | server → client only |
| Protocol | its own (`ws://`), upgrade handshake | plain HTTP, `text/event-stream` |
| Go library | needs a 3rd-party lib (gorilla/coder) | **standard library only** |
| Auto-reconnect | you implement it | **built into the browser** |
| Proxies/firewalls | sometimes need special config | it's just HTTP; works everywhere |
| Data | text or binary | text (UTF-8) only |

For a one-directional stream of small JSON snapshots, every row favors SSE. The
only WebSocket advantages — full duplex and binary frames — are things we don't
need. Choosing WebSocket here would mean adding a dependency and hand-rolling
reconnection to buy capabilities we'd never use. (Recorded as ADR-005.)

## How our SSE works (it's tiny)

**Server side** (`dashboard.go`):
- A single `Hub` goroutine polls `store.Snapshot()` every 2s and fan-outs the
  JSON to every connected client's channel. One querier, N receivers — the
  classic pub/sub hub.
- The SSE HTTP handler sets `Content-Type: text/event-stream`, then for each
  snapshot writes the frame `data: <json>\n\n` and **flushes**. That `\n\n` and
  the flush are the entire SSE wire format.
- Slow clients get frames dropped (non-blocking send) rather than stalling the
  broadcast — the next tick carries fresh state anyway, so a dropped frame is
  harmless.
- New clients are "primed" with the last snapshot immediately, so a freshly
  opened page fills in instantly instead of waiting up to 2s.

**Browser side** (`static/index.html`):
```js
const es = new EventSource("/api/v1/events");
es.onmessage = (e) => render(JSON.parse(e.data));
```
That's the whole client. `EventSource` is built into every browser; it parses
the `data:` frames and — importantly — **automatically reconnects** if the
connection drops (server restart, network blip). We wrote zero reconnection
logic; you can watch the little status dot go "reconnecting… → live" on its own
when you bounce the server.

## The one browser→server action

The "Retry" button doesn't use the SSE channel (SSE is receive-only). It's an
ordinary `fetch(..., {method:"POST"})` to `/api/v1/dlq/{id}/retry`. This is the
right mental model: **SSE for the live feed, plain HTTP for actions.** Trying to
push the action back up a WebSocket would be more machinery for no benefit.

## What the dashboard shows

- **Status cards** — live counts of pending/running/completed/failed/dead.
- **Workers** — each live worker's host, status (active/draining/dead),
  completed/failed counters, and last heartbeat age. Start and stop workers and
  watch them appear and drain.
- **Dead-letter queue** — the failure inbox, each row with a Retry button.

## When you *would* reach for WebSockets

To be fair to the other tool: pick WebSockets when you genuinely need
**bidirectional, low-latency** streaming — a chat app, a collaborative editor, a
multiplayer game, a terminal. There the full-duplex channel earns its
complexity. A monitoring dashboard is not that. Matching the tool to the shape of
the problem is the whole lesson here.

## Embedded, like the migrations

The HTML is baked into the server binary with `//go:embed` (`embed.go`), so the
server is still a single self-contained executable — no separate static-file
deploy. Same reasoning as the embedded migrations in `docs/03`.
