# Secrets broker: keys the agent can use but never read

Status: **proposal**. Extends DESIGN.md §9 (Credentials) and §14 (Security model).

## 1. Problem

People need to hand an agent API keys: a Stripe test key, a vendor's REST token, an
OAuth sign-in for their own Google or Jira account. Today there are two ways, and both
are bad:

- **Paste it in chat.** The key sits in the chat platform's history forever, visible to
  everyone in the channel and to anyone who later exports it.
- **An operator copies it onto the runner box.** Slow, needs shell access, and the key
  ends up in a file the agent can read.

The second point is the deeper one. **A key the agent can read is a key the agent can
leak.** The agent reads untrusted input by design (§14.1): web pages, issue bodies,
files in a repository. One prompt injection and the key is in an outbound request to
someone else's server. Careful handling on the way in changes nothing if the key is
readable once it arrives.

So the goal is not "a safer place to paste". It is: **the agent uses a key without the
key ever being in its process, filesystem or context.**

## 2. Goals and non-goals

### Goals

1. A person hands over a key without it passing through chat or the agent.
2. The key is stored only on the gateway. Runner hosts never hold it (one opt-in
   exception, §7.3).
3. The agent uses the key through the gateway, which attaches it to outbound requests
   for that key's allowed hosts only.
4. Every secret has an owner, a policy (who may use it, from which runners, against which
   hosts) and an expiry, and every use is audited with thread, turn and requesting user.
5. Per-user credentials: a key or OAuth sign-in that only works for turns its owner
   started. This is §9.3 mode 2, finally built.
6. Works unchanged when gateway and runners are on separate machines.

### Non-goals (v1)

- Interception of HTTPS to arbitrary hosts with a private CA. Tools must accept a base
  URL or token from the environment (§7).
- Non-HTTP protocols (database wire protocols, SSH). Possible later via the same tunnel.
- Replacing a real secrets manager. Backends stay pluggable (§5.2); the broker adds
  intake, policy and brokering on top.
- Protection against a malicious operator with root on the gateway host.

## 3. How it fits

```
 person ──(one-time link, private)──▶ gateway intake page
                                          │ encrypt, store, audit
                                          ▼
                                     gateway vault
                                          ▲
 agent ──▶ runner egress proxy ──(existing runner↔gateway connection)──┘
  (http://127.0.0.1:N/s/stripe/v1/…)      gateway attaches the key
                                          ──▶ https://api.stripe.com/v1/…
```

Nothing here is a new trust boundary. It is §9.2's rule ("credential → proxied")
applied to arbitrary HTTP APIs instead of only MCP servers, and §9.1's minting model
applied to OAuth providers other than the forge.

## 4. Intake: how a key gets in

### 4.1 The flow

1. Someone asks in a thread: "I need to give you the vendor key."
2. The agent calls a surface tool, `request_secret(name, hosts, purpose)`. It does not
   receive a link.
3. The gateway creates an intake record and sends a **one-time link** to the person who
   asked, as an **ephemeral message** (`chat.postEphemeral`). Only they see it. Nothing is
   stored in channel history.
4. The link opens a small page served by the gateway: a password field, the requested
   name and hosts (editable within limits), who may use it, and an expiry.
5. On submit the gateway encrypts and stores the value, records the owner, and posts in
   the thread: "Alice added secret `vendor-api` (api.vendor.com, owner only)." The
   agent's tool call returns the name.

A person can also start intake without the agent, with a slash command
(`/splitscreen secret add vendor-api api.vendor.com`) that replies with the same
ephemeral link.

### 4.2 Why this link is safe to send

- **It is a capability, bound to one chat user.** The link carries 128 random bits. It
  expires after 15 minutes or one use, whichever comes first. The intake record stores
  the chat user it was minted for, so the stored secret's owner is that person, not
  whoever opened the page.
- **The agent never sees it.** The tool returns "link sent to @alice". An agent that
  could read the link could submit a key of its choosing. That is low harm, but it is
  pointless to allow.
- **Optional network gating.** The intake page is served on its own listener,
  `gateway.intake.listen`, separate from the runner endpoint. Deployments choose how
  people reach it:

| Mode | Reachability | Identity check on submit |
|---|---|---|
| `tailnet` | Tailscale `serve` in front of a loopback listener | Link, plus the `Tailscale-User-Login` header must map to the same person (§4.3) |
| `access-proxy` | Behind Cloudflare Access, an oauth2-proxy, or similar | Link, plus the proxy's signed identity assertion, verified (JWT) |
| `link-only` | Any HTTPS ingress | Link only |

`link-only` is acceptable because the link is single-use, short-lived and delivered
privately. The other two modes add a second factor and keep the page off the public
internet.

### 4.3 Identity mapping

`people` maps chat users to the identities the other systems use, so a submission from
the tailnet or an access proxy can be matched to the chat user the link was minted for:

```yaml
people:
  U0123ABCD: { email: alice@corp.example, tailnet: alice@corp.example }
```

If the mapping is missing in a mode that requires it, the page refuses rather than
guessing.

## 5. Storage

### 5.1 The vault

A new store table holds the vault. The gateway's existing SQLite store already holds
the audit trail and is backed up with it.

```
vault_secrets(
  name TEXT PRIMARY KEY, owner TEXT, kind TEXT,        -- static | oauth
  ciphertext BLOB, nonce BLOB, key_id TEXT,
  policy TEXT,                                          -- JSON, §6
  created_at, updated_at, expires_at, last_used_at, revoked_at)
```

Values are encrypted with AES-256-GCM. The data key is a named secret in the existing
backend (`vault.key_secret`), so it lives wherever the other gateway secrets already do:
a file in `secrets_dir`, or the cloud parameter store. A copied database file without
that key is useless. Key rotation re-encrypts rows in place, and `key_id` allows a
gradual rollover.

### 5.2 Why not an external manager

The existing `secrets.Backend` interface (env, directory, cloud parameter store) is
read-only and operator-managed. It stays that way: config secrets are infrastructure.
Vault secrets are user data with owners and policies. Mixing the two would mean either
giving chat users write access to the infrastructure store, or teaching every backend
about owners and policies.

A cloud secret manager is a poor fit for another reason. The agent often holds a cloud
identity of its own, frequently a broad one. If vault secrets lived in the same cloud
account, the agent could read them with its own credentials. The vault must be outside
anything the agent's own credentials reach.

## 6. Policy

Each secret carries:

```yaml
name: vendor-api
kind: static
inject:                       # how it goes on the wire, §7.1
  header: Authorization
  format: "Bearer {value}"
hosts: [api.vendor.com]       # exact hosts; optional path prefixes
users: owner                  # owner | any | [U0123ABCD, …]
runners: [review]             # default: all runners
methods: [GET, POST]          # default: all
expose: false                 # §7.3
expires: 2027-03-01
```

`users: owner` is the default. Widening to `any` is an explicit choice at intake, shown
on the page as "anyone who talks to the agent can use this key".

Policy is checked at the gateway on every request. A refusal comes back as an HTTP 403
whose body says why, so the agent can relay it: "`vendor-api` may only be used in turns
started by its owner."

## 7. Use: how the agent uses a key it cannot read

### 7.1 Egress proxy (default)

Each runner opens a loopback HTTP listener, the **egress proxy**. Its address is in
every harness's environment as `SPLITSCREEN_EGRESS`, for example
`http://127.0.0.1:41871`.

```
curl $SPLITSCREEN_EGRESS/s/vendor-api/v2/orders?limit=5
  └─▶ runner proxy
        └─▶ frame egress.request {secret, method, path, headers, body…, thread, turn}
              └─▶ gateway: policy check; attach key; https://api.vendor.com/v2/orders?limit=5
              ◀── egress.response (status, headers, body), streamed in chunks
```

- **The base URL is the integration point.** Most SDKs and CLIs accept one
  (`STRIPE_API_BASE`, `OPENAI_BASE_URL`, `--endpoint`). The agent is told the URL per
  secret: `splitscreen secrets url vendor-api` prints `$SPLITSCREEN_EGRESS/s/vendor-api`.
- **The gateway owns TLS to the upstream**, so the request leaves from the gateway's
  address. That gives one stable IP for vendor allowlists. The cost is one extra hop.
- **Response hygiene.** The gateway strips `Set-Cookie`, and replaces any literal
  occurrence of the secret value in the response with `[secret:vendor-api]`, because some
  APIs echo keys back. Redirects are not followed across hosts: the 3xx goes back as is,
  and following it through the proxy re-applies policy.
- **Request hygiene.** Agent-supplied `Authorization`, `Cookie` and `Proxy-*` headers
  are dropped, so the agent cannot smuggle a different credential through a trusted
  host. Bodies are size-capped (`egress.max_body`, default 32 MiB), and the frames
  reuse the blob chunking from §11.
- **Placeholders for keys in bodies or query strings.** Some APIs take the key as a body
  field or query parameter. `inject: {query: api_key}` or
  `inject: {placeholder: true}` lets the agent write `{{secret:vendor-api}}` in the URL
  or body, and the gateway substitutes it, for that secret's hosts only.

### 7.2 Minted tokens (OAuth and forge-like providers)

For `kind: oauth`, the vault holds the refresh token and the provider's client
reference. It is never handed out. Two ways to use it:

- **Through the egress proxy**, as in §7.1, with the gateway attaching a fresh access
  token.
- **As a minted access token** for tools that insist on holding one, such as CLIs that
  talk to many Google hosts: `splitscreen secrets token google` makes a
  `credential.request {kind: oauth, secret: google}` and receives a provider access token
  (about 1 hour, memory only), exactly like the forge flow in §9.1. A leaked access token
  expires. The refresh token never leaves the gateway.

OAuth sign-in reuses intake: the one-time link starts the provider's consent flow
instead of showing a password field. The callback lands on the intake listener, which
needs a reachable redirect URI. That is the cost §9.3 already names, now confined to the
intake listener rather than the runner endpoint.

### 7.3 Exposed secrets (opt-in, discouraged)

Some tools must hold a raw key and cannot be pointed at a base URL. For those, a secret
marked `expose: true` can be fetched into one child process:

```
splitscreen secrets exec vendor-api --env VENDOR_KEY -- vendorctl deploy
```

The value crosses to the runner host for the life of that process. The agent can read
it there (§14.3), so intake shows a warning and audit marks every exposure. This is the
honest fallback, not the default.

### 7.4 Discovery

`splitscreen secrets list` shows the secrets this turn may use: name, hosts, how to use
it, owner and expiry. It never shows values. Bundles get one line of guidance: "API
keys: `splitscreen secrets list`; never ask anyone to paste a key in chat."

## 8. Binding a request to a turn

Per-user policy (`users: owner`) is only as strong as the gateway's knowledge of which
turn a request belongs to. This is the subtle part.

### 8.1 What the gateway knows

The gateway knows authoritatively who started every turn (`turns.surface_user`). The
question is how a proxy request from the runner host proves which thread it came from.

### 8.2 Why the obvious answers fail

- **A thread id in the request, or in `SPLITSCREEN_THREAD`.** The agent sets its own
  environment and can talk to the runner's socket directly with any thread id. This is
  how the existing shims identify their thread today. That is fine for routing, but it is
  not proof.
- **A per-session token in the harness's environment.** All sessions on a runner run as
  one Unix user, and a process can read the environment of any process with the same
  UID through `/proc/<pid>/environ`. Yama's `ptrace_scope=1` blocks attaching to another
  process, not reading its environment. One session can steal another's token.

### 8.3 Attribution by the runner, from the kernel

The runner attributes each proxy connection itself, using information the agent cannot
forge:

1. It accepts the connection, finds the client's PID (`SO_PEERCRED` on a unix socket,
   or the socket inode in `/proc/net/tcp` mapped to a PID for the loopback TCP
   listener), and walks the parent chain.
2. The walk must reach a **harness process the runner itself spawned**. The runner
   records the PID and start time of every harness at spawn, so PID reuse cannot fool
   it. The thread is the one the runner launched that harness for, from the runner's
   memory, not from any environment.
3. No such ancestor (a double-forked daemon, or a process re-parented to init) means
   the request is unattributed. Unattributed requests may only use `users: any`
   secrets, and the refusal says so.

The runner then sends `thread` and `turn` in `egress.request`. The gateway checks that
the turn is running and belongs to that thread, and applies `users` against that turn's
`surface_user`.

The same attribution should replace the thread claims of the existing helpers: the
credential helper, `send-file` and the MCP shim. That closes a cross-thread confusion
that exists today.

### 8.4 The runner has to be trustworthy for this

§8.3 is only as good as the runner process. In a common install, the agent's Unix user
can tamper with its own runner:

- A user-level systemd unit for the runner can be overridden by a drop-in in that user's
  home directory.
- A harness binary installed in that user's home directory can be replaced.
- Same-UID processes can be traced if `ptrace_scope` is 0.

So this proposal adds a documented **hardened runner mode**, required for per-user
policy to mean anything:

| Requirement | Why |
|---|---|
| Runner is a **system** unit with `User=<agent user>` | Only root can change its command, environment or binary |
| Runner binary and harness binary are root-owned, not writable by the agent user | The agent cannot swap the process that attributes requests |
| Harness auto-update off; updates ship as a deploy step | Same reason |
| `kernel.yama.ptrace_scope >= 1` | The agent cannot attach to its ancestors |
| Runner refuses to start per-user brokering if any check fails, and says which | A misconfiguration fails closed and visibly |

Outside hardened mode, the runner sends `attributed: false` on every request, and the
gateway treats every secret as if its policy were `users: any`, *only if* the secret's
owner allowed that at intake. Owner-only secrets simply do not work there, and the error
says why. Requests are never silently downgraded.

This is the honest framing for §14.3: on a shared-user runner, "which person's
credential" is an instruction to the agent, not a control. Hardened mode makes it a
control.

## 9. Split deployments

Nothing assumes the gateway and runners share a machine:

- The vault, policy, intake page and upstream TLS are all on the gateway. A runner host
  holds no vault secret at rest or in memory, except `expose: true` values during one
  `secrets exec`.
- Egress traffic rides the runner's existing authenticated WebSocket (mutual TLS plus the
  enrollment token, §5.1). No new ports between runner and gateway.
- Upstream calls leave from the gateway's network. Document this. It changes what vendor
  IP allowlists and egress firewalls need to permit.
- Intake reachability (§4.2) is a gateway-side choice: tailnet, an access proxy, or
  ingress. Runner hosts are never involved.

## 10. Protocol

Minor version bump to **1.7**. Every new frame is optional, and a 1.6 runner simply
lacks the feature.

| Direction | Frame | Purpose |
|---|---|---|
| ↑ | `egress.request` | `{id, secret, method, path, headers, thread, turn, attributed}`, body as `blob.*` |
| ↓ | `egress.response` | `{id, status, headers}`, body as `blob.*`, or `{id, error}` |
| ↑ | `credential.request` | Adds `kind: oauth`, `secret`, for minted tokens (§7.2) |
| ↓ | `credential.grant` | Unchanged shape |
| ↑ | `secret.list` | What this turn may use |
| ↑ | `secret.expose` | For `secrets exec`, refused unless `expose: true` |

The runner advertises the capability `egress`. The gateway sends `SPLITSCREEN_EGRESS`
only to runners that advertise it.

## 11. Configuration

```yaml
gateway:
  vault:
    key_secret: vault-key            # 32 random bytes, base64, in the secrets backend
  intake:
    listen: 127.0.0.1:8444
    public_url: https://gw.corp.example/intake
    mode: tailnet                     # tailnet | access-proxy | link-only
    access_proxy: { jwks: https://…/certs, audience: … }   # access-proxy only
  egress:
    max_body: 32MiB
    timeout: 120s
    deny_hosts: [169.254.169.254, metadata.google.internal]  # always refused

people:
  U0123ABCD: { email: alice@corp.example, tailnet: alice@corp.example }

runners:
  review:
    egress: true                      # default false until hardened mode, §8.4
    hardened: true                    # runner must prove it at hello, or per-user secrets refuse
```

Secrets themselves are **not** in this file. They are created by intake and managed
from chat:
- `/splitscreen secret list|show|revoke|policy`
- the web view's new read-only `/vault` page, which lists names, owners, hosts, last
  use and expiry, never values

## 12. Audit and lifecycle

- Events: `secret.intake.created`, `secret.created`, `secret.used` (thread, turn, user,
  host, method, path, status, bytes, but never bodies), `secret.refused` (why),
  `secret.exposed`, `secret.revoked`, `secret.expiring`.
- Expiry warnings reuse `secret_expiry_warning` and are posted to the owner by DM.
- Revocation is immediate. In-flight requests finish, and new ones are refused.
- Owners may revoke their own secrets. Operators (gateway config `admins`) may revoke
  any.
- `last_used_at` makes stale keys visible. A key unused for 90 days gets a DM to its
  owner asking whether to keep it.

## 13. Security analysis

### 13.1 What this achieves

- A key typed into intake reaches only the gateway's memory and its encrypted store. It
  never touches chat history, the runner host or the agent's context.
- Prompt injection can **use** a key only against that key's hosts and methods, only
  within a running turn the policy admits, and visibly in the audit log. It cannot
  **take** the key elsewhere.
- OAuth refresh tokens never leave the gateway. The worst exposure is a one-hour
  access token.
- With a hardened runner, a person's credentials work only in turns that person started.

### 13.2 What it does not achieve

- **Misuse within policy.** An injected instruction in Alice's turn can still make
  `POST /v2/refunds` with Alice's key, if the policy allows POST to that host. Narrow
  `methods` and paths, and the audit trail, are the mitigations. Nothing short of a
  human approving each call stops it. Per-secret `approve: [POST]`, reusing the
  permission prompt, is an open question (§15).
- **Exfiltration through the API itself.** If the allowed host can store data (an
  object store, a messaging API), the agent can write data there. That is the API's
  permission model, not the broker's.
- **`expose: true` secrets** are readable by the agent while exposed, by definition.
- **Gateway compromise** discloses everything. Same as today, plus the vault. The vault
  key in a separate backend helps against a stolen database file, not against root on
  the gateway.
- **Non-hardened runners** get no per-user binding (§8.4). That is stated in the UI, not
  hidden.

### 13.3 Threats considered

| Threat | Mitigation |
|---|---|
| Agent opens the intake link and submits its own value | Link goes only to the requester, ephemerally; the agent never sees it |
| Someone forwards the link | Single use, 15 minutes, owner fixed at mint; `tailnet` and `access-proxy` modes also check the submitter |
| Agent sends a key to a lookalike host | Exact host allowlist; cross-host redirects not followed |
| Agent abuses the proxy to reach internal services | Upstream must match the secret's hosts; `deny_hosts` always applies; no raw forwarding without a secret |
| Agent forges which thread it is in | Kernel-level attribution by a hardened runner (§8.3) |
| API echoes the key in a response | The literal value is redacted from responses |
| Stolen gateway database | AES-GCM with a key held in a separate backend |

## 14. Rollout

Each phase is one PR, and each is usable on its own.

1. **Vault and intake.** Store, encryption, the `request_secret` surface tool,
   ephemeral link, intake page (`link-only` and `tailnet`), `secret list|revoke`, audit.
   No use path yet. Operators can verify storage end to end.
2. **Egress proxy.** Runner listener, `egress.*` frames, gateway injection, hygiene
   rules, `secrets url|list`, policy limited to `users: any`.
3. **Attribution and hardened mode.** Runner-side peer attribution (§8.3), hardened-mode
   checks and documentation, `users: owner`. Move the existing helpers onto the same
   attribution.
4. **OAuth.** `kind: oauth`, consent via intake, minted tokens (§7.2). First provider:
   Google. Second: whatever §9.3's Jira case needs.
5. **`expose`** and the `access-proxy` intake mode, if anyone needs them.

## 15. Open questions

1. **Per-call approval** for dangerous methods: should `approve: [POST, DELETE]` route
   through the existing permission prompt to the secret's owner?
2. **Rate limits per secret** to cap the damage of a runaway loop: requests per minute,
   and a daily cap.
3. **Slack modal intake** as an alternative to the link: smoother on mobile, but the
   value transits the chat platform's servers. Offer it, or keep intake strictly
   off-platform?
4. **Should the vault ever be readable by operators?** Default no: there is no "show
   value" command. Rotation means re-entering the key. Confirm that this is acceptable.
5. **HTTPS interception** for tools with hard-coded hosts (§2 non-goal). Worth it in a
   v2, given a runner-local CA, or do wrappers that accept a base URL always suffice?
