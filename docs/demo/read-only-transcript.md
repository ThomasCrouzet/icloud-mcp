# Synthetic read-only first-run transcript

**Label: SYNTHETIC.** This transcript comes from the repository protocol
fixture. It is not a live iCloud account, not a desktop host install, and not
proof of Apple compatibility. Credentials, calendar names, and paths are
fabricated.

## Reproduce

The fixture runs the production startup, registration, and bounded stdio code
with an in-memory CalDAV peer. It never opens a network socket.

```bash
go test -c -o /tmp/icloud-mcp-protocol ./cmd/icloud-mcp
ICLOUD_MCP_PROTOCOL_FIXTURE=normal \
ICLOUD_EMAIL=fixture-user@example.com \
ICLOUD_PASSWORD=fixture-password-only \
ICLOUD_MCP_DEFAULT_TZ=Europe/Paris \
ICLOUD_MCP_READ_ONLY=true \
ICLOUD_MCP_PERMISSIONS=calendar.read \
ICLOUD_MCP_ENABLE_CONTACTS=false \
ICLOUD_MCP_ENABLE_MAIL=false \
TZ=UTC \
/tmp/icloud-mcp-protocol -test.run='^TestExecutableProtocolFixture$'
```

Send newline-delimited JSON-RPC frames on stdin. The relevant requests are:

```json
{"jsonrpc":"2.0","id":1,"method":"initialize","params":{"protocolVersion":"2025-11-25","capabilities":{},"clientInfo":{"name":"demo","version":"1"}}}
{"jsonrpc":"2.0","method":"notifications/initialized"}
{"jsonrpc":"2.0","id":2,"method":"tools/list"}
{"jsonrpc":"2.0","id":3,"method":"tools/call","params":{"name":"icloud_capabilities","arguments":{}}}
{"jsonrpc":"2.0","id":4,"method":"tools/call","params":{"name":"list_calendars","arguments":{}}}
```

## Observed result

The server identifies itself and negotiates the legacy protocol:

```json
{"jsonrpc":"2.0","id":1,"result":{"protocolVersion":"2025-11-25","capabilities":{"tools":{}},"serverInfo":{"name":"icloud-mcp","version":"dev"}}}
```

The read-only profile registers seven tools:

```text
calendar_capabilities
find_free_slots
get_event
icloud_capabilities
list_calendars
search_events
validate_event
```

No create, update, delete, mutate, trash, or send tool appears.

`icloud_capabilities` reports the effective gates:

```json
{
  "readOnly": true,
  "toolCount": 7,
  "capabilityGroups": {
    "calendarRead": true,
    "calendarWrite": false,
    "calendarDelete": false,
    "contactsRead": false,
    "contactsWrite": false,
    "contactsDelete": false,
    "mailRead": false,
    "mailMutation": false,
    "mailTrash": false,
    "mailSend": false,
    "mailReply": false
  }
}
```

`list_calendars` returns one fixture calendar:

```json
{
  "count": 1,
  "calendars": [
    {"path": "/fixture/calendars/home/", "name": "Fixture Calendar"}
  ]
}
```

## What this proves

- The production process validates a read-only configuration and starts.
- The tool manifest contains exactly seven read-only tools.
- Structured and text results agree on the effective capability groups.
- No credential from the fixture appears in a result or log.

## What this does not prove

- A real MCPB desktop host install, settings persistence, or secret storage.
- A real iCloud account, calendar list, or Apple server behavior.
- That the host passes only the advertised environment variables to the child.

The live matrix stays `UNVERIFIED` until a recorded run. See
[compatibility](../compatibility-matrix.md) and the
[MCPB status](../distribution/mcpb.md#status).
