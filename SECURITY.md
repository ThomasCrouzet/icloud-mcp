# Security Policy

## Threat model

`icloud-mcp` is a host-agnostic stdio child process: any MCP-compatible client
that spawns it with an environment and wires stdin/stdout can drive the tools.
Calendar text, contact fields, mailbox metadata, and message content are
untrusted remote data and may contain prompt-injection text. Assume an LLM
driving the host can be manipulated into invoking any registered tool,
regardless of which host or model vendor is used.

The effective blast radius is every enabled domain and capability for one
configured iCloud account. With global read-only disabled, this can include
Calendar and Contacts mutation, Mail flag/move/trash mutation, and SMTP delivery
to configured recipients.

### Shared-process residual risk

The unified binary deliberately trades stronger process isolation for simpler
deployment. A memory-disclosure or arbitrary-code defect in any enabled domain
can expose every credential held by that process. Feature flags remove tools and
prevent optional client construction, but do not remove compiled code. Mail may
use a dedicated app-specific password, and separate process/account
configurations remain the stronger isolation option.

### Security boundaries

- **Per-domain network allowlists:** Calendar can reach only
  `caldav.icloud.com:443` and `p[0-9]{1,3}-caldav.icloud.com:443`; Contacts can
  reach only `contacts.icloud.com:443` and
  `p[0-9]{1,3}-contacts.icloud.com:443`; IMAP can dial only
  `imap.mail.me.com:993`; SMTP can dial only `smtp.mail.me.com:587`.
- **Verified encryption:** DAV uses HTTPS, IMAP uses implicit TLS, and SMTP
  requires STARTTLS before authentication. TLS verification is always enabled
  with TLS 1.2 or later. DAV proxy environment variables are ignored.
- **Credential isolation:** Calendar and Contacts have distinct authenticated
  HTTP clients. IMAP and SMTP use fixed dialers and fresh sessions. There is no
  union authenticated client or response-controlled destination.
- **Global read-only:** `ICLOUD_MCP_READ_ONLY=true` removes every Calendar and
  Contacts write, every Mail mutation, and Mail send from `tools/list`.
- **Independent Mail gates:** Mail read grants neither mutation nor send. SMTP
  send additionally requires an exact-address recipient allowlist; literal `*`
  is an explicit allow-all policy and emits a boot warning. Prefer exact
  addresses in production.
- **Secret redaction:** configured identities, passwords, Basic-auth variants,
  SASL PLAIN variants, and URL-escaped forms are redacted from stderr, tool
  errors, success payloads, and panic responses.
- **Bounded remote content:** DAV XML/vCard, IMAP protocol data, MIME structure,
  decoded bodies, list/search results, and SMTP messages have byte, item, depth,
  or result caps. Mail list/search never returns bodies. Message retrieval never
  returns raw MIME, raw headers, HTML, or attachment bytes. Stdio frames are
  capped at 1 MiB, caller-reflecting error records above 64 KiB are replaced,
  and serialized MCP results are capped at 256 KiB.
- **Optimistic concurrency:** Calendar and Contacts update/delete require
  specific ETags on the wire. Mail references include UIDVALIDITY; conditional
  flag updates fail closed when MODSEQ/MODIFIED safety cannot be established.
- **No mutation replay:** Calendar PUT/DELETE, including series delete, Contacts
  writes, IMAP mutations, and SMTP submission are not automatically replayed.
  Calendar retries apply to reads only. Ambiguous mutation or post-DATA SMTP
  failures return `outcome_unknown` with reconciliation guidance.
- **Mutation audit:** every production mutation emits `domain`, `resourceType`,
  and a process-local opaque HMAC `resourceToken`, never a raw Calendar path or
  UID. Records also exclude contact UIDs, mailbox identities, recipients,
  Calendar title/location/notes, contact fields, message subjects, addresses,
  bodies, Message-IDs, and attachment names.
- **Minimal local surface:** there is no `os/exec`, telemetry, plugin loading,
  runtime code download, or disk write. The only disk access is optional
  boot-time `file://` secret loading from regular files capped at 4 KiB and
  required to be mode 0600 or stricter (not group or world accessible). The
  optional health listener is loopback-only and accepts no arbitrary hostname.
- **Revocable credentials:** use app-specific passwords, never the main Apple
  Account password. They can be revoked independently at appleid.apple.com.

The network and registration boundaries limit what a manipulated MCP caller can
do through the tool surface. They do not make remote content trustworthy and do
not eliminate vulnerabilities in this process or its dependencies.

`delete_event` may include `deletedTitle` in a successful MCP response for target
confirmation. It is never included in the audit trail. Mail `outcome_unknown`
must never be retried without checking Sent and the recipients.

When an IMAP server advertises CONDSTORE, go-imap beta.8 cannot expose the
tagged MODIFIED response required for a safe conditional STORE.
`set_message_flags` therefore returns `protocol_error` before STORE and does not
claim `concurrent_modification` on that path. For SMTP, `to`, `cc`, and `bcc`
are each optional, but their aggregate must contain at least one recipient.

Implementation details: [docs/security.md](docs/security.md).

## Reporting a vulnerability

Report security issues privately through GitHub's
[private vulnerability reporting](https://github.com/ThomasCrouzet/icloud-mcp/security/advisories/new)
rather than opening a public issue. Include the affected domain, whether a
credential or remote-content boundary is involved, and a minimal reproduction
without real account data or secrets. You should receive an acknowledgement
within a few days.
