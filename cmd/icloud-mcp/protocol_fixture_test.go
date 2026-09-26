package main

import (
	"bufio"
	"context"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"os"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/ThomasCrouzet/icloud-mcp/internal/config"
	"github.com/ThomasCrouzet/icloud-mcp/internal/contacts"
	"github.com/ThomasCrouzet/icloud-mcp/internal/icloud"
	maildomain "github.com/ThomasCrouzet/icloud-mcp/internal/mail"
	"github.com/ThomasCrouzet/icloud-mcp/internal/security"
)

// TestMain provides a fixture-only executable. No fixture switch is compiled
// into the product. The external runner supplies synthetic credentials only.
var protocolOutput io.Writer

func TestMain(m *testing.M) {
	if os.Getenv("ICLOUD_MCP_PROTOCOL_FIXTURE") != "" {
		protocolOutput = os.Stdout
		// Go test diagnostics must not enter the JSON-RPC stream.
		os.Stdout = os.Stderr
	}
	os.Exit(m.Run())
}

func TestExecutableProtocolFixture(t *testing.T) {
	mode := os.Getenv("ICLOUD_MCP_PROTOCOL_FIXTURE")
	if mode == "" {
		t.Skip("started only by scripts/protocol_evidence.py")
	}
	if err := runProtocolFixture(mode); err != nil {
		t.Fatal(err)
	}
}

func runProtocolFixture(mode string) error {
	cfg, err := config.Load()
	if err != nil {
		return err
	}
	calendar := icloud.NewClient(&protocolDAV{mode: mode}, security.ICloudBaseURL, security.IsICloudHost)
	var addressBooks contacts.Service
	if cfg.EnableContacts {
		addressBooks = contacts.NewClient(&protocolDAV{contacts: true, mode: mode}, security.ContactsBaseURL, security.IsContactsHost)
	}
	var mail maildomain.Service
	if cfg.EnableMail {
		policy, policyErr := maildomain.RecipientPolicyFromExact(cfg.SMTPAllowedRecipients)
		if cfg.EffectiveMailSend() && policyErr != nil {
			return policyErr
		}
		mail, err = maildomain.NewService(maildomain.Config{
			Address: cfg.MailAddress, Password: cfg.MailPassword, RecipientPolicy: policy,
		}, func(context.Context) (net.Conn, error) {
			client, peer := net.Pipe()
			go protocolIMAP(peer, mode)
			return client, nil
		}, func(context.Context) (net.Conn, error) {
			return nil, fmt.Errorf("fixture prohibits SMTP submission")
		}, cfg.EffectiveMailWrite(), cfg.EffectiveMailSend())
		if err != nil {
			return err
		}
	}
	return runServer(cfg, security.AuditFormatJSON, "", calendar, addressBooks, mail, os.Stdin, protocolOutput)
}

// protocolDAV never opens a socket. The real DAV clients parse these responses
// and still validate the fixed production authorities.
type protocolDAV struct {
	mode     string
	contacts bool
	lists    atomic.Int32
}

func (d *protocolDAV) Do(request *http.Request) (*http.Response, error) {
	status, body := http.StatusMultiStatus, ""
	if d.contacts {
		slog.Info("fixture Contacts request")
		if request.URL.Host != "contacts.icloud.com" {
			return nil, fmt.Errorf("unexpected fixture Contacts authority")
		}
		if d.mode == "protocol-failure" {
			body = "<invalid"
		} else {
			status = http.StatusUnauthorized
		}
	} else {
		if request.Method != "PROPFIND" || request.URL.Host != "caldav.icloud.com" {
			return nil, fmt.Errorf("unexpected fixture Calendar request")
		}
		switch request.URL.Path {
		case "/":
			body = protocolMultistatus("/", `<current-user-principal><href>/fixture/principal/</href></current-user-principal>`)
		case "/fixture/principal/":
			body = protocolMultistatus(request.URL.Path, `<C:calendar-home-set><href>/fixture/calendars/</href></C:calendar-home-set>`)
		case "/fixture/calendars/":
			if d.mode == "cancel" && d.lists.Add(1) == 1 {
				slog.Info("fixture Calendar waiting")
				<-request.Context().Done()
				slog.Info("fixture Calendar canceled")
				return nil, request.Context().Err()
			}
			body = protocolMultistatus("/fixture/calendars/home/", `<resourcetype><collection/><C:calendar/></resourcetype><displayname>Fixture Calendar</displayname><C:supported-calendar-component-set><C:comp name="VEVENT"/></C:supported-calendar-component-set>`)
		default:
			return nil, fmt.Errorf("unexpected fixture Calendar path")
		}
	}
	return &http.Response{
		StatusCode: status, Header: http.Header{"Content-Type": {"application/xml"}},
		Body: io.NopCloser(strings.NewReader(body)), Request: request,
	}, nil
}

func protocolMultistatus(path, properties string) string {
	return `<multistatus xmlns="DAV:" xmlns:C="urn:ietf:params:xml:ns:caldav"><response><href>` + path + `</href><propstat><prop>` + properties + `</prop><status>HTTP/1.1 200 OK</status></propstat></response></multistatus>`
}

// protocolIMAP exercises the real adapter through an in-memory connection.
// Neither commands nor credentials are copied into the fixture log.
func protocolIMAP(peer net.Conn, mode string) {
	defer func() { _ = peer.Close() }()
	_ = peer.SetDeadline(time.Now().Add(10 * time.Second))
	slog.Info("fixture IMAP connection")
	if mode == "protocol-failure" {
		_, _ = io.WriteString(peer, "invalid greeting\r\n")
		return
	}
	if _, err := io.WriteString(peer, "* OK [CAPABILITY IMAP4rev1] Fixture ready\r\n"); err != nil {
		return
	}
	scanner := bufio.NewScanner(peer)
	for scanner.Scan() {
		fields := strings.Fields(scanner.Text())
		if len(fields) < 2 {
			return
		}
		var response string
		switch fields[1] {
		case "LOGIN":
			response = fields[0] + " NO [AUTHENTICATIONFAILED] Fixture rejection\r\n"
		case "CAPABILITY":
			response = "* CAPABILITY IMAP4rev1\r\n" + fields[0] + " OK Capability\r\n"
		default:
			return
		}
		if _, err := io.WriteString(peer, response); err != nil {
			return
		}
	}
}
