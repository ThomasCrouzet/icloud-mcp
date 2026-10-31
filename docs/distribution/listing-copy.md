# Listing copy

The three versions below are 144, 48, and 444 characters. Each names CalDAV,
CardDAV, and IMAP/SMTP, and each states that the server is headless.

## Under 160 characters

Headless iCloud MCP for Calendar, Contacts, and Mail over CalDAV, CardDAV, and IMAP/SMTP. One Go stdio binary with independent capability gates.

## Under 60 characters

Headless iCloud MCP: CalDAV, CardDAV, IMAP/SMTP.

## Paragraph

icloud-mcp is a headless stdio MCP server for one iCloud account. It provides
Calendar over CalDAV, Contacts over CardDAV, and Mail read, mutation, and send
over IMAP and SMTP. Calendar, Contacts, Mail read, Mail mutation, and Mail send
have independent capability gates, and global read-only mode removes every
mutation. It uses app-specific passwords over fixed iCloud destinations, with no
EventKit, browser automation, or private Apple API.

## Rules

- Keep "headless", "CalDAV", "CardDAV", and "IMAP/SMTP" in every version.
- Use app-specific passwords. Never describe the main Apple Account password.
- Do not claim live iCloud verification for the fixture-only test suite.
- Do not add Reminders, Notes, Photos, Drive, Messages, Find My, Keychain, or
  Home to the scope.
