package mcptools

import (
	"bufio"
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"net"
	"strings"
	"testing"
	"time"

	maildomain "github.com/ThomasCrouzet/icloud-mcp/internal/mail"
	"github.com/mark3labs/mcp-go/client"
	"github.com/mark3labs/mcp-go/mcp"
	"github.com/mark3labs/mcp-go/server"
)

type blankPlainPart struct {
	subtype, charset, encoding, body string
	attachment                       bool
	rejection                        string
}

func TestProtocolMailBlankPlainFallback79(t *testing.T) {
	plain := func(body string) blankPlainPart {
		return blankPlainPart{subtype: "PLAIN", charset: "UTF-8", encoding: "7BIT", body: body}
	}
	html := blankPlainPart{subtype: "HTML", charset: "UTF-8", encoding: "QUOTED-PRINTABLE", body: `<head>hidden head</head><p>Meet &amp; greet at 10:00</p><script>hidden script</script>` +
		`<style>hidden style</style><template>hidden template</template><!-- hidden comment -->` +
		`<img src=3D"https://example.invalid/tracker"><a href=3D"https://example.invalid/">Hall A</a>`}
	const htmlText = "Meet & greet at 10:00\nHall A"
	quotedBlank := blankPlainPart{subtype: "PLAIN", charset: "UTF-8", encoding: "QUOTED-PRINTABLE", body: "=20=09=0D=0A"}
	charsetBlank := blankPlainPart{subtype: "PLAIN", charset: "ISO-8859-1", encoding: "BASE64", body: base64.StdEncoding.EncodeToString([]byte{0xa0, 0x20, 0x09})}
	malformed := blankPlainPart{subtype: "PLAIN", charset: "UTF-8", encoding: "BASE64", body: "!!!!"}
	attachedPlain, attachedHTML := plain("Attached text must not win"), html
	attachedPlain.attachment, attachedHTML.attachment = true, true
	rejected := plain("")
	rejected.rejection = "NO [NOPERM] synthetic denial"
	badHTML := html
	badHTML.body = "<script>unterminated"
	largeHTML := html
	largeHTML.body = "<p>" + strings.Repeat("word ", 20) + "</p>"
	tests := []struct {
		name                  string
		parts                 []blankPlainPart
		allow                 bool
		maxBody               int
		body, source, warning string
		omitted               bool
		errorCode             string
		fetched               string
	}{
		{name: "empty", parts: []blankPlainPart{plain(""), html}, allow: true, body: htmlText, source: "html_fallback", fetched: "1,2"},
		{name: "whitespace", parts: []blankPlainPart{plain(" \t\r\n"), html}, allow: true, body: htmlText, source: "html_fallback", fetched: "1,2"},
		{name: "quoted_printable_whitespace", parts: []blankPlainPart{quotedBlank, html}, allow: true, body: htmlText, source: "html_fallback", fetched: "1,2"},
		{name: "base64_charset_whitespace", parts: []blankPlainPart{charsetBlank, html}, allow: true, body: htmlText, source: "html_fallback", fetched: "1,2"},
		{name: "later_meaningful_plain", parts: []blankPlainPart{plain(""), html, plain(" Later plain \r\n")}, allow: true, body: " Later plain \r\n", source: "plain_text", fetched: "1,3"},
		{name: "meaningful_plain_preferred", parts: []blankPlainPart{plain(" Plain text \r\n"), html}, allow: true, body: " Plain text \r\n", source: "plain_text", fetched: "1"},
		{name: "default_unchanged", parts: []blankPlainPart{plain(" \r\n"), html, plain("Later plain")}, body: " \r\n", source: "plain_text", fetched: "1"},
		{name: "no_html_unchanged", parts: []blankPlainPart{plain(" \r\n")}, allow: true, body: " \r\n", source: "plain_text", fetched: "1"},
		{name: "attached_plain_excluded", parts: []blankPlainPart{plain(""), attachedPlain, html}, allow: true, body: htmlText, source: "html_fallback", fetched: "1,3"},
		{name: "attached_html_excluded", parts: []blankPlainPart{plain(" \r\n"), attachedHTML}, allow: true, body: " \r\n", source: "plain_text", fetched: "1"},
		{name: "malformed_plain_stops", parts: []blankPlainPart{malformed, html}, allow: true, warning: "body_decode_failed", omitted: true, fetched: "1"},
		{name: "later_malformed_plain_stops", parts: []blankPlainPart{plain(""), malformed, html}, allow: true, warning: "body_decode_failed", omitted: true, fetched: "1,2"},
		{name: "decoded_limit_exact", parts: []blankPlainPart{plain("    "), html}, allow: true, maxBody: 4, body: "Meet", source: "html_fallback", warning: "html_truncated", fetched: "1,2"},
		{name: "decoded_limit_exceeded", parts: []blankPlainPart{plain("     "), html}, allow: true, maxBody: 4, warning: "body_too_large", omitted: true, fetched: "1"},
		{name: "wire_limit_exceeded", parts: []blankPlainPart{plain(strings.Repeat(" ", maildomain.MaxWireBodyBytes+1)), html}, allow: true, warning: "body_too_large", omitted: true, fetched: "1"},
		{name: "fetch_error_stops", parts: []blankPlainPart{rejected, html}, allow: true, errorCode: "authorization", fetched: "1"},
		{name: "malformed_html", parts: []blankPlainPart{plain(""), badHTML}, allow: true, warning: "html_malformed", omitted: true, fetched: "1,2"},
		{name: "html_output_bounded", parts: []blankPlainPart{plain(""), largeHTML}, allow: true, maxBody: 9, body: "word word", source: "html_fallback", warning: "html_truncated", fetched: "1,2"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
			defer cancel()
			finished := make(chan blankPlainTrace, 1)
			service, err := maildomain.NewService(maildomain.Config{Address: "fixture@icloud.com", Password: "synthetic-mail-password"},
				func(context.Context) (net.Conn, error) {
					local, remote := net.Pipe()
					go func() { finished <- serveBlankPlainIMAP(remote, test.parts) }()
					return local, nil
				}, nil, false, false)
			if err != nil {
				t.Fatal(err)
			}
			s := server.NewMCPServer("mail-blank-plain", "test", server.WithToolCapabilities(false))
			RegisterMail(s, mailToolsDeps(service), true, false, false, false, false)
			c, err := client.NewInProcessClient(s)
			if err != nil {
				t.Fatal(err)
			}
			defer func() { _ = c.Close() }()
			if err := c.Start(ctx); err != nil {
				t.Fatal(err)
			}
			init := mcp.InitializeRequest{}
			init.Params.ProtocolVersion = mcp.LATEST_PROTOCOL_VERSION
			init.Params.ClientInfo = mcp.Implementation{Name: "mail-blank-plain", Version: "test"}
			if _, err := c.Initialize(ctx, init); err != nil {
				t.Fatal(err)
			}
			request := mailToolsRequest(map[string]any{
				"mailbox": "INBOX", "uid_validity": 9, "uid": 2, "allow_html_fallback": test.allow,
			})
			request.Params.Name = "get_message"
			if test.maxBody != 0 {
				request.Params.Arguments.(map[string]any)["max_body_bytes"] = test.maxBody
			}
			result, err := c.CallTool(ctx, request)
			if err != nil {
				t.Fatal(err)
			}
			t.Logf("mcp_result=%s", mailToolsText(t, result))
			select {
			case trace := <-finished:
				t.Logf("imap_fetch_parts=%s", strings.Join(trace.parts, ","))
				if trace.err != nil {
					t.Fatal(trace.err)
				}
				if got := strings.Join(trace.parts, ","); got != test.fetched {
					t.Errorf("fetched parts = %s, want %s", got, test.fetched)
				}
			case <-ctx.Done():
				t.Fatal(ctx.Err())
			}
			if test.errorCode != "" {
				payload := mailToolsJSON(t, result)
				if !result.IsError || payload["code"] != test.errorCode {
					t.Fatalf("error = %+v, want %s", payload, test.errorCode)
				}
				return
			}
			var got maildomain.Message
			if result.IsError {
				t.Fatalf("unexpected tool error: %s", mailToolsText(t, result))
			}
			if err := json.Unmarshal([]byte(mailToolsText(t, result)), &got); err != nil {
				t.Fatal(err)
			}
			if got.Body != test.body || got.BodySource != test.source || got.BodyOmitted != test.omitted {
				t.Errorf("body=%q source=%q omitted=%v, want body=%q source=%q omitted=%v", got.Body, got.BodySource, got.BodyOmitted, test.body, test.source, test.omitted)
			}
			if test.warning == "" && len(got.Warnings) != 0 || test.warning != "" && (len(got.Warnings) != 1 || got.Warnings[0].Code != test.warning) {
				t.Errorf("warnings = %+v, want %q", got.Warnings, test.warning)
			}
			if len(mailToolsText(t, result)) > maildomain.MaxResultBytes {
				t.Error("result exceeded byte limit")
			}
		})
	}
}

type blankPlainTrace struct {
	parts []string
	err   error
}

func serveBlankPlainIMAP(conn net.Conn, parts []blankPlainPart) (trace blankPlainTrace) {
	defer func() { _ = conn.Close() }()
	if err := conn.SetDeadline(time.Now().Add(5 * time.Second)); err != nil {
		trace.err = err
		return
	}
	write := func(response string) bool {
		_, trace.err = fmt.Fprint(conn, response)
		return trace.err == nil
	}
	if !write("* OK [CAPABILITY IMAP4rev1] ready\r\n") {
		return
	}
	reader := bufio.NewReader(conn)
	for {
		line, err := reader.ReadString('\n')
		if err != nil {
			trace.err = err
			return
		}
		fields := strings.Fields(line)
		if len(fields) < 2 {
			trace.err = fmt.Errorf("invalid fixture command")
			return
		}
		tag := fields[0]
		var response string
		switch {
		case fields[1] == "LOGIN":
			response = tag + " OK [CAPABILITY IMAP4rev1] authenticated\r\n"
		case fields[1] == "EXAMINE":
			response = "* 1 EXISTS\r\n* OK [UIDVALIDITY 9] valid\r\n* OK [UIDNEXT 3] next\r\n" + tag + " OK [READ-ONLY] examined\r\n"
		case strings.Contains(line, "BODYSTRUCTURE"):
			if !strings.Contains(line, "UID FETCH 2 ") || !strings.Contains(line, "BODY.PEEK[HEADER.FIELDS") {
				trace.err = fmt.Errorf("unsafe metadata command: %s", line)
				return
			}
			var structure strings.Builder
			structure.WriteByte('(')
			for _, part := range parts {
				disposition := "NIL"
				if part.attachment {
					disposition = `("ATTACHMENT" ("FILENAME" "fixture.txt"))`
				}
				fmt.Fprintf(&structure, `("TEXT" "%s" ("CHARSET" "%s") NIL NIL "%s" %d 1 NIL %s NIL NIL)`, part.subtype, part.charset, part.encoding, len(part.body), disposition)
			}
			structure.WriteString(` "ALTERNATIVE" NIL NIL NIL NIL)`)
			response = fmt.Sprintf("* 1 FETCH (UID 2 FLAGS () ENVELOPE (NIL \"Fixture\" NIL NIL NIL NIL NIL NIL NIL NIL) BODYSTRUCTURE %s BODY[HEADER.FIELDS (MESSAGE-ID IN-REPLY-TO REFERENCES REPLY-TO)]<0> {2}\r\n\r\n)\r\n%s OK metadata\r\n", structure.String(), tag)
		case strings.Contains(line, "UID FETCH 2 "):
			found := false
			for i, part := range parts {
				id := fmt.Sprint(i + 1)
				if !strings.Contains(line, "BODY.PEEK["+id+".MIME]<0.65537>") || !strings.Contains(line, "BODY.PEEK["+id+"]<0.524289>") {
					continue
				}
				found = true
				trace.parts = append(trace.parts, id)
				if part.rejection != "" {
					response = tag + " " + part.rejection + "\r\n"
					break
				}
				header := fmt.Sprintf("Content-Type: text/%s; charset=%s\r\nContent-Transfer-Encoding: %s\r\n\r\n", strings.ToLower(part.subtype), part.charset, part.encoding)
				response = fmt.Sprintf("* 1 FETCH (UID 2 BODY[%s.MIME]<0> {%d}\r\n%s BODY[%s]<0> {%d}\r\n%s)\r\n%s OK body\r\n", id, len(header), header, id, len(part.body), part.body, tag)
				break
			}
			if !found {
				trace.err = fmt.Errorf("unsafe or unexpected body command: %s", line)
				return
			}
		case fields[1] == "LOGOUT":
			write("* BYE closing\r\n" + tag + " OK logout\r\n")
			return
		default:
			trace.err = fmt.Errorf("unexpected fixture command: %s", line)
			return
		}
		if !write(response) {
			return
		}
	}
}
