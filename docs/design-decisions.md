# Design Decisions and Lessons Learned

This document captures the *why* behind some of the non-obvious choices in
the bridge. The code itself documents *what* it does; this document explains
the tradeoffs we considered and the bugs we hit during development.

If you're touching the code and you find yourself wondering "why is this
here?", check here first.

---

## Lesson 1: LLM training data is not authoritative for API specs

The initial scaffold of `main.go` was written based on LLM training data
plus partial snippets from the Zoom Developer Forum, not against current
Zoom documentation. The result compiled cleanly, looked plausible, and was
wrong in three real ways:

1. The `download_token` was placed *inside* the payload object instead of
   at the top level of the webhook envelope where Zoom actually puts it.
2. We were authenticating recording file downloads with the Server-to-Server
   OAuth access token (the wrong auth model) instead of using the per-event
   `download_token` as a query parameter on the download URL.
3. We had no `x-zm-signature` verification at all on incoming events — a
   real attack surface for a publicly-reachable webhook URL on Cloud Run.

All three were caught by reading the actual Zoom webhook documentation
against our struct definitions and handler logic, *before* writing tests.

**The lesson.** When using AI to scaffold integrations with third-party
APIs, the model produces confident-looking code based on its training data
which may be subtly wrong in ways that compile fine but fail at runtime.
Always verify the schema and auth model against current upstream docs
*before* writing tests, not after — otherwise the tests just ratify the
wrong assumptions.

This applies to any agent-assisted integration work. Schema verification is
a checklist item, not an optional polish step.

---

## Lesson 2: Google Drive API silently 404s on Shared Drive items

Our first synthetic test against the real production Drive folder failed
with this in the bridge logs:

```
processRecording error: create meeting folder:
  googleapi: Error 404: File not found: <folder_id>
```

The folder existed. The service account had Contributor access. The
`<folder_id>` we passed was correct. The 404 came from a quirk: **by
default, the Google Drive API does not search Shared Drive items in
`Files.List` queries**, even if you pass the right parent ID. You have to
explicitly opt in with both `SupportsAllDrives(true)` and
`IncludeItemsFromAllDrives(true)` on every call that touches Shared Drive
content.

The fix is small (a few extra builder method calls), but the bug is
invisible until you try to write to a Workspace Shared Drive folder. Most
enterprise Workspace setups *do* use Shared Drives, so this would have
failed for any real production deployment if we hadn't caught it locally
first.

**How we caught it.** Before deploying to production, we built a synthetic
test driver (`cmd/synthetic-test/`) that runs the bridge against a real
Drive folder using fake recording content. The driver reproduced the 404
on the first run. We fixed `getOrCreateFolder` and the two `Files.Create`
call sites, re-ran the driver, and confirmed.

**The lessons.**

1. Synthetic end-to-end tests against real external services catch a
   category of bugs that unit tests against mocks structurally cannot —
   anything that lives at the boundary between your code and the real
   API's actual behavior.
2. When an API returns "not found" for a resource you know exists, the
   first hypothesis should be "wrong query scope" not "wrong ID."

---

## Decision 1: Per-meeting mutex via `sync.Map` for serializing concurrent events

**The problem.** Zoom delivers `recording.completed` and
`recording.transcript_completed` as separate webhook events for the same
meeting. They can arrive within milliseconds of each other, and Zoom does
not guarantee ordering. The Zoom Dev Forum has at least one report of
`recording.transcript_completed` firing ~7ms *before* `recording.completed`.

If both events arrive nearly simultaneously, the bridge spawns two
background goroutines that both call `getOrCreateFolder("<date>-<topic>")`
for the same target. Both see no existing folder, both call `Files.Create`,
and **two folders with identical names appear in Drive** because Drive does
not enforce name uniqueness within a parent. The recording files end up
split between the two duplicate folders.

**Alternatives considered:**

| Option | Why we rejected it |
|---|---|
| Ignore the race; accept rare duplicate folders | The race window is real (multi-millisecond folder creation against a multi-millisecond event delivery gap), and duplicate folders are visible and confusing |
| Delay transcript processing by N seconds to let `recording.completed` finish first | Hacky; depends on a magic number; doesn't actually fix the fundamental race; transcripts can arrive *before* `recording.completed`, so order is not reliable anyway |
| Use Drive's "create or fail if exists" semantics | Drive doesn't have those semantics; the closest analog still requires a list-then-create pattern with the same TOCTOU race |
| **Per-meeting mutex via `sync.Map` keyed by meeting ID** | **What we shipped** |

**Why this approach.** The lock serializes only events for the *same*
meeting — events for different meetings still process in parallel. Lock
acquisition is cheap (no contention except in the rare race case). The
`sync.Map` lookup is concurrency-safe by design.

**Why we don't clean up map entries.** The map grows by one entry per
unique meeting ID seen during the lifetime of a Cloud Run instance. Cloud
Run instances are short-lived (cold-started on demand, evicted after
periods of inactivity). The per-instance memory ceiling is bounded by the
meeting volume during one warm period — typically a few entries to a few
dozen. Building eviction logic would be premature optimization.

**Verified by:** the synthetic test driver's `--reverse-order` flag, which
sends the transcript event *before* the recording event and confirms both
end up in the same folder via the lock.

---

## Decision 2: Skip metadata write on `recording.transcript_completed` events

The `meeting-metadata.json` file is written exactly once per meeting, on
the initial `recording.completed` event.

**Why not also on transcript events?** If we wrote metadata on both events,
the second write would create a file with misleading counts:
`files_uploaded: 1`, `total_files: 1` — reflecting only the transcript.
The recording event has the more informative counts (typically
`files_uploaded: 3`, including audio, video, and timeline).

**Why not merge across both events?** That would require the transcript
handler to read the existing metadata, parse it, merge new info, and write
back. More code, a TOCTOU race during read-modify-write, and the only thing
it would actually add is "yes, the transcript also arrived." We added a
`transcript_may_arrive_separately` field to the initial metadata instead,
which conveys the same information without the merge complexity.

**Why not separate filenames per event?** Two `meeting-metadata-*.json`
files in the same folder is uglier than one, with no value gained.

---

## Decision 3: Synthetic E2E test driver instead of mock-heavy unit tests

We have ~63 unit tests covering pure functions (signature verification,
URL building, sanitization) and HTTP handler routing. We deliberately
stopped there. We did *not* extend unit tests to cover `processRecording`
and `streamFileToDrive` via interface injection and fake clients. Instead,
we built a synthetic end-to-end driver in `cmd/synthetic-test/` that
exercises the real code paths against real Google Drive.

**The reasoning.** For code that is mostly "call API A, transform, call
API B," unit tests with fake interfaces mostly verify that the fakes are
wired up correctly — they don't catch real-world bugs. The bugs we actually
hit (and care about catching) live at the boundary with real services:
API quirks like the Shared Drive 404, schema mismatches, auth model errors.
Those are exactly what synthetic end-to-end tests catch and what unit tests
structurally cannot.

We *could* extend unit tests to cover the streaming path with fake
interfaces, but the cost (refactoring to add `ZoomClient` and `DriveClient`
interfaces, ~50 lines of abstraction + ~250 lines of fake/test code) would
yield maybe 3 genuinely valuable assertions and a lot of tautological
boilerplate. Bad ratio.

**The strategy:** unit-test the parts where logic lives (pure functions,
routing, security gates); integration-test the parts where bugs live
(external API behavior).

---

## Decision 4: `context.Background()` in the background goroutine

The webhook handler responds 200 OK to Zoom immediately and spawns a
background goroutine to do the actual Drive uploads. Inside the goroutine
we use `context.Background()` rather than `r.Context()`, and this is
critical and easy to get wrong.

```go
go func() {
    ctx := context.Background()  // ← NOT r.Context()
    if err := s.processRecording(ctx, meeting, downloadToken, writeMetadata); err != nil {
        log.Printf("processRecording error: %v", err)
    }
}()
```

**The subtlety.** Go's stdlib HTTP server attaches a context to every
request. That context is canceled the moment the handler function returns.
If we wrote `ctx := r.Context()`, the background goroutine would inherit a
context that gets canceled within milliseconds of being spawned — and the
Drive upload, which uses that context internally to abort the in-flight
HTTP requests, would be aborted before any real work happened.

`context.Background()` produces a fresh, never-canceled root context, which
is what fire-and-forget background work needs.

**Why this matters.** This is a classic Go gotcha with webhook-style
services. The compiler cannot help you — both `r.Context()` and
`context.Background()` have the same type. The code looks identical to
working code; only the runtime behavior differs. We're calling it out
here so future maintainers don't "improve" it by inheriting the request
context.

---

## Decision 5: Per-host folder routing

Recordings are routed to a Drive folder structure of
`<root>/<host_username>/<YYYY-MM-DD>-<topic>/raw/`, where `host_username`
is the lowercased local part of the meeting host's email (e.g., `skapadia`
from `skapadia@chariotsolutions.com`). Each consultant's recordings end up
in their own folder, with no behavior change required from anyone in the
org. The host email is always present in the webhook payload, so this
routing is reliable for every meeting.

**Alternatives considered.**

- **Topic-based routing** (e.g., `Sales:` prefix → `/sales/`). Rejected:
  depends on every host following a naming convention we cannot enforce.
  When the convention is violated — even occasionally — recordings end up
  in the wrong folder and the layout loses its meaning.
- **AI-based classification** (call an LLM to decide the folder per
  meeting). Rejected: per-event API cost and operational complexity for a
  problem that doesn't need it. The bridge's whole point is to be a cheap,
  reliable piece of plumbing; calling out to an LLM for routing would
  invert that.

**Why lowercasing matters.** Drive folder names are case-sensitive, so
`Skapadia@chariotsolutions.com` and `skapadia@chariotsolutions.com` would
otherwise create two distinct folders for the same human. We lowercase at
the routing step to make this impossible. Locking this in now is cheaper
than discovering the problem later and writing a migration.

**Known follow-up.** `getOrCreateFolder` has a latent TOCTOU race: if two
events for *different* meetings hosted by the *same* user arrive
simultaneously, both goroutines can call the search-then-create path
concurrently and produce duplicate host folders. The blast radius is
cosmetic (two folders with the same name; Drive lookups remain
deterministic per-process) and the existing per-meeting mutex prevents
the race for events sharing a meeting ID. Not fixed in this PR.

---

## Reading order for new contributors

If you are picking this codebase up for the first time, the suggested
reading order is:

1. [`README.md`](../README.md) — what the bridge does and how to run it
2. [`main.go`](../main.go) — the implementation, top to bottom (~500 lines)
3. **This document** — the design decisions and lessons learned
4. [`docs/deployment.md`](./deployment.md) — how to deploy and operate it
5. [`cmd/synthetic-test/`](../cmd/synthetic-test/) — the synthetic end-to-end test driver
6. [`docs/synthetic-test-driver.md`](./synthetic-test-driver.md) — the driver's design rationale
