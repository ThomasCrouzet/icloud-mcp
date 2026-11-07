# Mail network troubleshooting

Use this guide when Mail fails but Calendar or Contacts works. Start with
credential-free network checks, not a password change or a test message.

## Supported connections

The binary uses fixed destinations with separate protocol clients:

| Service | Destination | Required transport |
|---|---|---|
| IMAP reads and mutations | `imap.mail.me.com:993` | Implicit TLS |
| SMTP submission | `smtp.mail.me.com:587` | TCP, EHLO, mandatory STARTTLS, then TLS |
| Calendar and Contacts | Their validated iCloud hosts and shards on port 443 | HTTPS |

All TLS connections require certificate and hostname validation and TLS 1.2
or later. Production endpoints are not configurable. Mail uses direct sockets,
not an HTTP transport. The binary has no HTTP CONNECT or SOCKS proxy support.
`HTTP_PROXY`, `HTTPS_PROXY`, and `ALL_PROXY` do not configure Mail routing.
DAV also ignores proxy environment variables.

Do not replace the destination with an IP address or gateway. Do not disable
certificate checks, change DNS to evade a gateway, or tunnel around hosting
restrictions. Ask the network administrator to approve direct egress instead.

See [Mail compatibility](mail-compatibility.md) and
[network allowlists](security.md#network-allowlists) for the protocol contract.

## Check the installed version

These commands print local information without loading account credentials:

```sh
icloud-mcp -version
icloud-mcp -help
```

| Version | Available diagnostics |
|---|---|
| v0.4.3 | No `-doctor` or `-doctor-format` flags. Use the credential-free checks below. |
| v0.5.0 and v0.5.1 | `-doctor` and `-doctor-format=json` run bounded, authenticated, read-only domain probes. Mail failures report a sanitized error code. |
| Unreleased source | The same probes add optional Mail `stage` and `reason` fields for observed connection, TLS, or LOGIN rejection failures. |

The v0.4.3 statement comes from its
[tagged command source](https://github.com/ThomasCrouzet/icloud-mcp/blob/v0.4.3/cmd/icloud-mcp/main.go).
The released doctor behavior appears in the
[v0.5.1 command source](https://github.com/ThomasCrouzet/icloud-mcp/blob/v0.5.1/cmd/icloud-mcp/doctor.go).
Unreleased source is not a promise about an installed binary.
Check `-version` and `-help` before using these flags.

With a supporting binary and your existing local credential configuration, run
`icloud-mcp -doctor -doctor-format=json`. This is not a credential-free test.
It validates configuration and gives each enabled domain a 10-second probe
timeout. It returns exit status 0 when all enabled probes succeed, or 1 when
a probe fails. Disabled domains remain `disabled`.

The Mail probe authenticates to IMAP, lists mailboxes, and attempts a read-only
selection to inspect capabilities. It never sends or changes mail. It does not
test SMTP, even when Mail send is enabled. A Mail `ready` result therefore does
not establish SMTP reachability or permission to send.

### Unreleased Mail failure fields

Doctor JSON includes optional `stage` and `reason` fields on the Mail domain.
Text output uses the same labels, for example:

```text
mail: unavailable (unavailable) stage=tls reason=eof
```

Only these fixed pairs are supported:

| Stage | Reason | Safe interpretation and next action |
|---|---|---|
| `connect` | `dns` | Name resolution failed during dialing. Ask the administrator to check approved DNS and direct egress. |
| `connect` | `connection_failed` | The socket dial failed. Check approved access to IMAP port 993. |
| `connect` or `tls` | `deadline` | The operation timed out at this boundary. Check the network path without changing credentials. |
| `connect` or `tls` | `canceled` | The operation was canceled at this boundary. This does not identify a network defect. |
| `tls` | `eof` | The peer closed before verified TLS completed. No IMAP authentication occurred on that attempt. |
| `tls` | `certificate_verification` | Certificate or hostname validation failed. Repair the approved trust setup; never disable validation. |
| `connect` or `tls` | `unknown` | This boundary failed, but its cause has no supported classification. Do not infer a gateway or credential fault. |
| `authentication` | `rejected` | IMAP explicitly rejected LOGIN. Check the configured Mail identity and app-specific password locally. |

Connection and TLS stages come from the fixed IMAP dialer. Authentication rejection
comes from the observed LOGIN response, after the existing identity fallback.
Other setup and operation failures have no new fields. Success also omits them.
Absent fields mean no supported stage observation, not successful connectivity.
The fields describe the final failed attempt, not a trace of earlier attempts.

The labels never include DNS answers, socket addresses, certificates, mailbox
names, account identities, secret paths, credentials, or server response text.
Unknown or malformed metadata is omitted rather than echoed.
Existing error codes, deadlines, and retries do not change.
MCP errors and health output do not gain these doctor fields.

The optional loopback health endpoint is for monitoring. Health polling does
not perform these connection probes. See [host integration](agent-hosts.md).

## Credential-free TLS checks

Run these commands on the same approved network as the binary. A test on your
laptop does not establish connectivity inside a hosted container. Ask the
hosting provider to run them if you cannot access that runtime.

These examples require Linux, OpenSSL 3, GNU `timeout`, and normal system trust
roots. Run each command once. Each has a 15-second deadline, including DNS,
and a forced-stop allowance of two more seconds. They read no credential files.
They send no LOGIN, AUTH, MAIL FROM, RCPT TO, or DATA commands.

### IMAP implicit TLS

```sh
timeout --kill-after=2s 15s openssl s_client \
  -connect imap.mail.me.com:993 \
  -servername imap.mail.me.com \
  -verify_hostname imap.mail.me.com -verify_return_error \
  -min_protocol TLSv1.2 -brief </dev/null
```

This checks the implicit TLS handshake without logging in or listing mailboxes.

### SMTP STARTTLS

```sh
timeout --kill-after=2s 15s openssl s_client \
  -connect smtp.mail.me.com:587 -starttls smtp \
  -servername smtp.mail.me.com \
  -verify_hostname smtp.mail.me.com -verify_return_error \
  -min_protocol TLSv1.2 -brief </dev/null
```

This reads the SMTP greeting, sends EHLO and STARTTLS, and checks TLS. It does
not authenticate, supply recipients, or submit a message. Do not use implicit
TLS on port 587 or test submission to diagnose connection failures.

Record the exit status immediately after each command. A successful TLS check
requires `Verification: OK`, TLS 1.2 or later, and exit status 0. An exit status
alone does not establish success. GNU `timeout` normally returns 124 at the
deadline; a forced kill can return 137. Neither result establishes reachability.

OpenSSL and Go can use different trust-store installations. These checks locate
network or TLS failures; success does not prove authentication or connector readiness.
Keep hostname validation enabled and fix missing trust roots through normal
system maintenance. Do not add an untrusted gateway certificate to make a check pass.

## Interpret the results

| Observation | What it establishes |
|---|---|
| TCP connects, then TLS returns EOF | Something accepted TCP and closed before TLS completed. Authentication has not occurred. |
| Certificate validation fails | TLS trust or hostname validation failed. Do not continue with credentials. |
| SMTP greeting or STARTTLS fails | The SMTP path cannot complete its required upgrade. IMAP can still work. |
| Timeout | The check did not complete within its bound. It does not identify the failing network component. |
| Verified TLS succeeds | This network can complete that TLS exchange. Account authentication and mail operations remain untested. |
| Calendar or Contacts works over HTTPS | That domain works on port 443. It does not prove access to ports 993 or 587. |

An EOF before authentication cannot establish an account or app-password
failure. It also does not identify whether the host, gateway, or remote server
closed the connection. Do not infer a credential problem from this result.

In [issue #77](https://github.com/ThomasCrouzet/icloud-mcp/issues/77), the reporter
observed both tested names resolving to gateway address `198.18.0.1`.
They reported IMAP TLS EOF, successful CalDAV TLS, and successful Mail access
through a separate route. These environment-specific observations were not
independently reproduced. They suggest a hosting-network limitation, not a
confirmed connector defect. Ask the provider to confirm its DNS and mail-port policy.

The connector intentionally maps failures to bounded messages such as
`mail service is temporarily unavailable`. It does not offer a supported raw
connector-error or protocol-debug mode. Do not patch redaction or print raw
server replies. Use the connection stage and structured error code instead.
See [error codes](error-codes.md) and [redaction](security.md#credentials-and-redaction).

## Deploy on an approved network

Run the MCP host and `icloud-mcp` together on a trusted machine with approved,
direct egress to the fixed iCloud destinations. Use the supported stdio setup
in [host integration](agent-hosts.md). Keep account setup and credential files
local to that machine. Start with read-only capabilities.

1. Confirm that the MCP host can launch a local stdio process on that machine.
2. Ask the administrator to approve direct IMAP TCP 993 and SMTP TCP 587 egress.
3. Keep the fixed hostnames and system certificate validation. Do not substitute a gateway or IP address.
4. Check the installed binary with `-version` and `-help`.
5. Run the credential-free TLS checks from that same execution environment.
6. Configure the host to launch the local binary using [the stdio configuration](agent-hosts.md).
7. Use existing local Mail credentials for the authenticated read-only doctor only when account access is authorized.

Keep `ICLOUD_MCP_READ_ONLY=true` during initial setup. Enable only the required
read domains. Do not enable Mail send merely to test connectivity.
Working direct egress elsewhere does not verify the inaccessible hosted runtime.
The reported environment behavior in #77 remains unverified.

A hosted-only client needs provider support for this stdio deployment and
network access. Otherwise, use a compatible host on an approved network.
This project does not provide a remote MCP listener or a proxy deployment mode.
Do not expose an unauthenticated MCP service or publish the loopback health
endpoint as a remote connector.

## Information for a follow-up

Share the binary version, operating system, transport, and enabled domains.
Include the network location of each check, destination, exit status, and
whether failure occurred at DNS, TCP, SMTP STARTTLS, or TLS verification.
Ask the provider whether both mail ports permit direct approved egress.

Share a short redacted summary, not full transcripts or environment dumps.
Never include credentials, account addresses, secret paths, mailbox content,
or certificate material. Once TLS works, verify IMAP authentication locally
with a read-only operation or supported `-doctor`. Verify SMTP separately only
when an authorized send is actually required, not as a connectivity test.
