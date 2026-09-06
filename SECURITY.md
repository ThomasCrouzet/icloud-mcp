# Security Policy

## Threat model

`icloud-mcp` is a host-agnostic stdio child process. Any MCP-compatible client
can start it with an environment and connect stdin and stdout. The client can
then use the tools.

Calendar text, contact fields, mailbox metadata, and message content are
untrusted remote data. This data can contain prompt-injection text. Assume that
an attacker can manipulate the host LLM and invoke any registered tool. This
assumption applies to all hosts and model vendors.

The effective blast radius includes all enabled domains and capabilities for
one configured iCloud account. If you disable global read-only mode, the host
can change Calendar and Contacts data. It can also change Mail flags, move or
trash messages, and send Mail to configured recipients.

### Shared-process residual risk

The unified binary makes deployment simple, but process isolation is lower. A
memory disclosure or arbitrary-code defect in one enabled domain can expose all
credentials in the process. Feature flags remove tools and prevent construction
of optional clients. They do not remove compiled code. Mail can use a dedicated
app-specific password. Use separate process and account configurations for
stronger isolation.

### Security boundaries

- **Per-domain network allowlists:** Calendar can reach only
  `caldav.icloud.com:443` and `p[0-9]{1,3}-caldav.icloud.com:443`. Contacts can
  reach only `contacts.icloud.com:443` and
  `p[0-9]{1,3}-contacts.icloud.com:443`. IMAP can dial only
  `imap.mail.me.com:993`. SMTP can dial only `smtp.mail.me.com:587`.
- **Verified encryption:** DAV uses HTTPS, and IMAP uses implicit TLS. SMTP
  requires STARTTLS before authentication. The server always verifies TLS and
  requires TLS 1.2 or later. DAV ignores proxy environment variables.
- **Credential isolation:** Calendar and Contacts use separate authenticated
  HTTP clients. IMAP and SMTP use fixed dialers and fresh sessions. No client
  authenticates to multiple domains. A response cannot control a destination.
- **Global read-only:** `ICLOUD_MCP_READ_ONLY=true` removes every Calendar and
  Contacts write, every Mail mutation, and Mail send from `tools/list`.
- **Independent Mail gates:** Mail read does not permit mutation or send. SMTP
  send also requires an exact-address recipient allowlist. The literal `*`
  permits all recipients and causes a boot warning. Use exact addresses in
  production.
- **Secret redaction:** the server removes configured identities, passwords,
  Basic-auth variants, SASL PLAIN variants, and URL-escaped forms. It removes
  them from stderr, tool errors, success payloads, and panic responses.
- **Bounded remote content:** the server limits DAV XML, vCard data, IMAP
  protocol data, and MIME structures. It also limits decoded bodies, list and
  search results, and SMTP messages. It applies byte, item, depth, or result
  limits. Mail list and
  search operations never return bodies. Message retrieval never returns raw
  MIME, raw headers, HTML, or attachment bytes. The server limits stdio frames
  to 1 MiB and serialized MCP results to 256 KiB. It replaces reflected error
  records that exceed 64 KiB.
- **Optimistic concurrency:** Calendar and Contacts update and delete operations
  require specific ETags on the wire. Mail references include UIDVALIDITY.
  Conditional flag updates fail closed if the server cannot establish
  MODSEQ/MODIFIED safety.
- **No mutation replay:** the server does not automatically replay Calendar PUT
  or DELETE, Contacts writes, IMAP mutations, or SMTP submission. Calendar
  series delete follows the same rule. The server retries only Calendar reads.
  Ambiguous mutation failures return `outcome_unknown` with reconciliation
  guidance. SMTP failures after DATA starts use the same result.
- **Mutation audit:** each production mutation emits `domain`, `resourceType`,
  and a process-local opaque HMAC `resourceToken`. It never emits a raw Calendar
  path or UID. Audit records also exclude contact UIDs, mailbox identities,
  recipients, Calendar titles, locations, and notes. They exclude contact
  fields, message subjects, addresses, bodies, Message-IDs, and attachment
  names.
- **Minimal local surface:** there is no `os/exec`, telemetry, plugin loading,
  runtime code download, or disk write. Optional boot-time `file://` secret
  loading is the only disk access. It accepts regular files of at most 4 KiB
  with mode 0600 or stricter. The files must not permit group or world access. The
  optional health listener uses loopback only and does not accept arbitrary
  hostnames.
- **Revocable credentials:** use app-specific passwords, never the main Apple
  Account password. You can revoke them independently at appleid.apple.com.

The network and registration boundaries limit the actions of a manipulated MCP
caller. Remote content remains untrusted. These boundaries do not remove
vulnerabilities from the process or its dependencies.

For target confirmation, a successful `delete_event` response can include
`deletedTitle`. The audit trail never includes it. Before you retry a Mail
operation with `outcome_unknown`, check Sent and the recipients.

When an IMAP server advertises CONDSTORE, go-imap beta.8 cannot expose the
tagged MODIFIED response. A safe conditional STORE requires this response.
Thus, `set_message_flags` returns `protocol_error` before STORE. It does not
report `concurrent_modification` on that path. For SMTP, `to`, `cc`, and `bcc`
are optional. Together, they must contain at least one recipient.

Implementation details: [docs/security.md](docs/security.md).

## Reporting a vulnerability

Report security issues privately through GitHub
[private vulnerability reporting](https://github.com/ThomasCrouzet/icloud-mcp/security/advisories/new)
instead of a public issue. Identify the affected domain and the applicable
credential or remote-content boundary. Include a minimal reproduction without
real account data or secrets. You should receive an acknowledgement within a
few days.
