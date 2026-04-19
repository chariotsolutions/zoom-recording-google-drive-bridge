# Security Posture and Network Options

This document captures the bridge's current security posture, the gap
in network-layer controls, the options we evaluated for closing that
gap, and the decision to stay with application-layer security for now.

Written as a reference for future security reviews, client conversations,
and the eventual point where a compliance checkbox forces the change.

---

## Motivation

The bridge's `/webhook` endpoint is publicly reachable on the internet.
This is required — Zoom needs to POST to it from their infrastructure,
and Zoom is not a Google Cloud IAM principal, so we can't use
`--no-allow-unauthenticated`. The question is: what protects the
endpoint from abuse?

---

## Current security posture

The bridge has the following layers of defense today, all at the
application or platform level:

| Layer | What it does | Protects against |
|---|---|---|
| **TLS** (automatic from Cloud Run) | Encrypts traffic in transit between Zoom and the endpoint | Network eavesdropping, man-in-the-middle, packet sniffing |
| **HMAC-SHA256 signature verification** (`/webhook`) | Zoom signs each event body with a shared secret; the bridge computes the same HMAC and compares with `hmac.Equal` (constant-time) | Forged webhook events from anyone who doesn't know the secret |
| **OIDC bearer token verification** (`/process-event`) | Cloud Tasks attaches a Google-signed JWT with `aud = PROCESS_EVENT_URL`; the handler verifies signature, audience, and expiry using `google.golang.org/api/idtoken` | Forged task dispatches from anyone who can't impersonate our service account |
| **5-minute replay window** (`/webhook`) | Rejects events with timestamps older than 5 minutes | Replaying a captured valid signed request after the fact |
| **Request body size limit** | `io.LimitReader` caps the body at 1 MB on both endpoints (see [issue #4](https://github.com/chariotsolutions/zoom-recording-google-drive-bridge/issues/4)) | Oversized payloads burning memory before signature/OIDC checks run |
| **Global rate limiting** (`/webhook`) | Token-bucket limiter in front of `/webhook`: 10 rps sustained / burst 20; returns 429 before body read or HMAC (see [issue #6](https://github.com/chariotsolutions/zoom-recording-google-drive-bridge/issues/6)) | Burning CPU on forged traffic from a single source below the DDoS threshold |
| **`max-instances=1` on Cloud Run** | Caps horizontal scaling at one container; the in-process per-meeting mutex is sufficient at Chariot's webhook volume | Runaway autoscaling cost if the endpoint is hammered |
| **Distroless container** | No shell, no package manager, no system tools in the runtime image | Post-exploitation lateral movement — if an attacker somehow gets code execution, there's nothing to use |
| **Scoped service account** | Cloud Run runs as a service account that only has Contributor access to one specific Drive folder | Blast radius containment — even if compromised, the service can only write to that one folder |
| **Secret in Secret Manager** | The webhook secret is never in code, env vars, shell history, or git | Secret leakage via repo, deploy logs, or process listing |

### Both endpoints are publicly reachable at the network layer

The Cloud Run service exposes two endpoints: `/webhook` (called by Zoom
from the public internet) and `/process-event` (called by Cloud Tasks
with an OIDC bearer token). The service is deployed with
`--allow-unauthenticated`, which grants `roles/run.invoker` to
`allUsers` across the **whole service** — there is no way in Cloud Run
to allow unauthenticated traffic to `/webhook` while requiring IAM on
`/process-event`. Both paths accept any inbound request at the network
layer; each handler then enforces its own application-layer auth.

Why this is acceptable:

- `/webhook` **has to be public** for Zoom to reach it. Zoom is not a
  Google Cloud IAM principal.
- `/process-event` **could in theory be locked down** with a second
  Cloud Run service (one `--allow-unauthenticated` for `/webhook`,
  one `--no-allow-unauthenticated` for `/process-event`). We don't do
  this because the split adds material complexity for minimal gain:
  the OIDC validator in the `/process-event` handler is strong
  enough that "network-reachable but OIDC-protected" is equivalent to
  "network-unreachable without IAM" for any realistic attacker.
- Forging a request that passes OIDC verification requires
  `iam.serviceAccounts.actAs` on a service account that itself has
  `roles/run.invoker` on the service — a capability no one outside
  the project's IAM has.

So "publicly reachable" reads as a surprise if you're thinking about
it in network terms, but the trust model that actually matters lives
at the application layer on both paths, and that's by design.

### What this posture gets right

The HMAC signature + replay window is exactly the security model Zoom
designed for webhook integrations. An attacker who doesn't know the
webhook secret cannot produce a valid signature, regardless of how they
reach the endpoint. The OIDC-based protection on `/process-event` gives
equivalent application-layer strength for the Cloud Tasks callback
path. The distroless container and scoped service account are
defense-in-depth that most webhook services don't bother with.

### What this posture is missing

**No network-layer controls on inbound traffic.** Anyone on the internet
can send HTTP requests to either endpoint. Forged requests are rejected
at the application layer (HMAC on `/webhook`, OIDC on `/process-event`)
before any Drive work happens, but the requests still reach the
application — consuming a cold start and a small amount of Cloud Run
billing.

**No per-IP rate limiting.** The token bucket in front of `/webhook` is
global: a single attacker at the threshold still consumes all the
available capacity, meaning legitimate Zoom traffic could be rejected
during abuse. Per-IP enforcement is Cloud Armor's job (Option 2); the
application-layer limiter is not the right place to track IPs given
Cloud Run's ephemeral-instance model.

---

## Options evaluated

### Option 1: Stay with application-layer security only (current)

Keep the existing application-layer posture. No network-level filtering.

- **Monthly cost:** ~$0 (Cloud Run scales to zero)
- **IP allowlisting:** ❌
- **Rate limiting:** ✅ global (application-layer token bucket); ❌ per-IP
- **Operational overhead:** Minimal
- **When to choose:** The threat model doesn't require network-layer
  controls (no regulated-client mandate, no compliance checkbox, no
  observed abuse)

### Option 2: Cloud Run + Load Balancer + Cloud Armor

Add a Global External Application Load Balancer in front of Cloud Run,
with Cloud Armor security policies on the load balancer.

```
Internet
  │
  ▼
Global External Application Load Balancer (public IP, TLS)
  │
  ├── Cloud Armor policy (IP allowlisting, rate limiting, WAF)
  │
  ▼
Cloud Run (--ingress=internal-and-cloud-load-balancing)
```

- **Monthly cost:** ~$23 ($18 forwarding rule + $5 Cloud Armor policy)
- **IP allowlisting:** ✅ (Zoom's published IP ranges)
- **Rate limiting:** ✅ (per-IP, configurable)
- **WAF rules:** ✅ (OWASP top 10, SQL injection, XSS — overkill for
  a webhook but available)
- **Geographic blocking:** ✅
- **Adaptive DDoS:** ✅ (ML-based)
- **Operational overhead:** One-time setup (load balancer + NEG +
  Cloud Armor policy). Code changes: zero — just flip Cloud Run's
  ingress flag.
- **Scale to zero:** ✅ preserved
- **Auto-scaling:** ✅ preserved
- **Pay per request:** ✅ preserved (for compute; LB cost is fixed)

**Important:** Cloud Armor is not a standalone service. It's a feature
that runs inside the load balancer's request pipeline. You cannot use
Cloud Armor without a load balancer — the load balancer is where Cloud
Armor's rules execute. Without it, there's nowhere for the IP filtering
to happen.

The `--ingress=internal-and-cloud-load-balancing` flag on Cloud Run
means the service **rejects direct traffic** — only requests forwarded
by the load balancer (after passing Cloud Armor) reach the container.
The public Cloud Run URL stops working for direct access.

**When to choose:** A security audit or regulated client requires
network-layer defense-in-depth beyond application-layer HMAC. Or
observed abuse at a volume that warrants per-IP rate limiting.

### Option 3: Compute Engine VM in a VPC

Deploy the same Go binary on a VM inside a VPC. Apply VPC firewall
rules directly.

```bash
gcloud compute firewall-rules create allow-zoom-webhooks \
  --direction=INGRESS \
  --action=ALLOW \
  --rules=tcp:8080 \
  --source-ranges=<zoom-ip-ranges> \
  --target-tags=webhook-receiver
```

- **Monthly cost:** ~$5–25 (VM running 24/7, size-dependent)
- **IP allowlisting:** ✅ (native VPC firewall rules, no Cloud Armor)
- **Rate limiting:** ❌ (not at the network layer; would need
  application-level middleware)
- **Scale to zero:** ❌ (VM runs 24/7)
- **Auto-scaling:** ❌ (single instance by default; needs Managed
  Instance Group + load balancer for auto-scaling)
- **Operational overhead:** High — you manage the OS, patching,
  monitoring, restarts, TLS certificates, logging pipeline

**When to choose:** Almost never for this use case. Costs more, does
less, and requires ongoing maintenance. The only advantage over
Option 2 is that VPC firewall rules are free (no $18/month forwarding
rule), but the VM itself costs more than that.

### Option 4: GKE Autopilot in a VPC

Deploy the bridge as a Kubernetes pod on GKE Autopilot with VPC-native
networking.

- **Monthly cost:** ~$75+ (cluster management fee + per-pod costs)
- **IP allowlisting:** ✅ (VPC firewall rules or Kubernetes
  NetworkPolicy)
- **Scale to zero:** ❌ (not natively; needs Knative Serving or KEDA)
- **Auto-scaling:** ✅ (Horizontal Pod Autoscaler)
- **Operational overhead:** Moderate — Google manages the nodes but
  you manage the Kubernetes layer

**When to choose:** If the bridge is one of many services and you're
already running GKE for other workloads. Not worth spinning up a
cluster for a single webhook receiver.

### Option 5: Cloud Functions (2nd gen)

Cloud Functions 2nd gen is Cloud Run under the hood — same
infrastructure, same scaling, same networking limitations. It offers a
simpler deployment model (no Dockerfile) but the same security posture
gap: no network-layer inbound filtering without Cloud Armor + a load
balancer.

**When to choose:** Never for this bridge. We already have a Dockerfile
and Cloud Run deployment. Cloud Functions would add nothing and remove
the Dockerfile control we use for distroless.

---

## Comparison table

| Option | Monthly cost | Scale to zero | IP allowlisting | Rate limiting | Ops overhead |
|---|---|---|---|---|---|
| **Cloud Run alone** (current) | ~$0 | ✅ | ❌ | ✅ global / ❌ per-IP | Minimal |
| **Cloud Run + LB + Cloud Armor** | ~$23 | ✅ | ✅ | ✅ per-IP | Low |
| **Compute Engine VM** | ~$5–25 | ❌ | ✅ | ❌ | High |
| **GKE Autopilot** | ~$75+ | ❌ | ✅ | ✅ | Moderate |
| **Cloud Functions** | ~$0 | ✅ | ❌ | ❌ | Minimal |

---

## VPC firewall rules vs Cloud Armor

These two mechanisms overlap on IP allowlisting but differ in scope:

| Capability | VPC firewall rules | Cloud Armor |
|---|---|---|
| IP allowlisting / denylisting | ✅ | ✅ |
| Port / protocol filtering | ✅ | ✅ |
| Rate limiting per IP | ❌ | ✅ |
| Geographic (country) blocking | ❌ | ✅ |
| WAF rules (OWASP top 10) | ❌ | ✅ |
| Custom match expressions | ❌ | ✅ |
| Adaptive DDoS (ML-based) | ❌ | ✅ |

VPC firewall rules operate at **Layer 3/4** (IP, port, protocol). They
answer: "can this IP reach this port?"

Cloud Armor operates at **Layer 7** (HTTP). It can inspect headers,
paths, request rates, geographic origin, and body patterns.

**Key constraint:** VPC firewall rules only apply to resources inside a
VPC. Cloud Run doesn't live in a VPC, so VPC firewall rules can't
protect it. To get network-layer filtering on Cloud Run, you need Cloud
Armor — which requires a load balancer because Cloud Armor is
implemented as a processing step inside the load balancer's request
pipeline.

---

## Decision

**Stay with Option 1 (application-layer security only) at ~$0/month.**

The HMAC signature verification is strong enough for our current threat
model. An attacker who doesn't know the webhook secret cannot produce a
valid signature regardless of how they reach the endpoint. The
distroless container and scoped service account limit blast radius even
in the unlikely event of a code-level vulnerability.

**Upgrade to Option 2 (Cloud Run + LB + Cloud Armor) when:**

- A security audit or regulated client mandates network-layer
  defense-in-depth
- Observed abuse at a volume that warrants IP-level blocking or rate
  limiting
- Chariot's security posture evolves to require all public endpoints
  to have network-layer controls

**Migration path from Option 1 to Option 2:**

The bridge code doesn't change at all. The migration is purely
infrastructure:

1. Create a Serverless NEG pointing at the Cloud Run service
2. Create a Global External Application Load Balancer with the NEG as
   a backend
3. Create a Cloud Armor security policy with Zoom's IP ranges as an
   allow rule
4. Attach the policy to the load balancer's backend service
5. Update Cloud Run: `--ingress=internal-and-cloud-load-balancing`
6. Update the Zoom Marketplace app with the load balancer's IP/domain
   (replacing the Cloud Run URL)
7. Test the validation handshake through the new path

Estimated effort: a few hours of one-time setup. No code changes, no
container rebuild, no test updates.
