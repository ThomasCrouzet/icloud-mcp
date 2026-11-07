package main

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/ThomasCrouzet/icloud-mcp/internal/config"
	"github.com/ThomasCrouzet/icloud-mcp/internal/contacts"
	maildomain "github.com/ThomasCrouzet/icloud-mcp/internal/mail"
	"github.com/ThomasCrouzet/icloud-mcp/internal/security"
)

func TestIssue77DoctorDiagnosticProtocol(t *testing.T) {
	for _, mode := range []string{"dns", "tls-eof", "rejected", "success", "unknown"} {
		for _, format := range []string{"json", "text"} {
			t.Run(mode+"/"+format, func(t *testing.T) {
				var dials, logins, lists, smtp atomic.Int32
				policy, _ := maildomain.ParseRecipientPolicy("to@example.invalid")
				dial := security.NewIMAPDialer(func(context.Context, string, string) (net.Conn, error) {
					dials.Add(1)
					if mode == "dns" {
						return nil, &net.DNSError{Name: "private-doctor.invalid", Err: "private-doctor-marker", IsNotFound: true}
					}
					local, remote := net.Pipe()
					go func() {
						defer func() { _ = remote.Close() }()
						_ = remote.SetDeadline(time.Now().Add(time.Second))
						header := make([]byte, 5)
						if _, err := io.ReadFull(remote, header); err == nil {
							_, _ = io.CopyN(io.Discard, remote, int64(header[3])<<8|int64(header[4]))
						}
					}()
					return local, nil
				})
				svc, err := maildomain.NewService(maildomain.Config{Address: "private-doctor@example.invalid", Password: "private-doctor-password", RecipientPolicy: policy}, func(ctx context.Context) (net.Conn, error) {
					if mode == "dns" || mode == "tls-eof" {
						return dial(ctx, "tcp", security.IMAPAddress)
					}
					dials.Add(1)
					if mode == "unknown" {
						return nil, errors.New("stage=tls reason=private-doctor-marker")
					}
					local, remote := net.Pipe()
					go serveDoctorIMAPFixture(remote, mode, &logins, &lists)
					return local, nil
				}, func(context.Context) (net.Conn, error) {
					smtp.Add(1)
					return nil, errors.New("SMTP must not be probed")
				}, false, true)
				if err != nil {
					t.Fatal(err)
				}
				permissions, _ := config.ParsePermissions("mail.read,mail.send")
				cfg := &config.Config{EnableMail: true, EnableMailSend: true, Permissions: permissions}
				factory := func(*config.Config) (contacts.Service, maildomain.Service, error) { return nil, svc, nil }
				var out bytes.Buffer
				status := runDoctorWithProbe(cfg, format, &out, func(cfg *config.Config, name string) doctorDomain {
					return doctorProbeWithServices(cfg, name, factory)
				})
				wantStage, wantReason, wantCode, wantStatus := "", "", "unavailable", "unavailable"
				switch mode {
				case "dns":
					wantStage, wantReason = "connect", "dns"
				case "tls-eof":
					wantStage, wantReason = "tls", "eof"
				case "rejected":
					wantStage, wantReason, wantCode, wantStatus = "authentication", "rejected", "authentication", "authentication_failed"
				case "success":
					wantCode, wantStatus = "", "ready"
				}
				if (status == 0) != (mode == "success") || status > 1 {
					t.Errorf("doctor exit=%d", status)
				}
				if format == "json" {
					var report map[string]any
					if err := json.Unmarshal(out.Bytes(), &report); err != nil {
						t.Fatal(err)
					}
					domains := report["domains"].([]any)
					mail := domains[2].(map[string]any)
					for key, want := range map[string]string{"status": wantStatus, "errorCode": wantCode, "stage": wantStage, "reason": wantReason} {
						got, _ := mail[key].(string)
						if got != want {
							t.Errorf("doctor %s=%q want %q", key, got, want)
						}
						if want == "" && mail[key] != nil {
							t.Errorf("doctor must omit absent %s", key)
						}
					}
					if domains[0].(map[string]any)["status"] != "disabled" || domains[1].(map[string]any)["status"] != "disabled" {
						t.Error("disabled domains were probed")
					}
				} else {
					if !strings.Contains(out.String(), "mail: "+wantStatus) {
						t.Error("doctor text lost Mail status")
					}
					if wantStage != "" && !strings.Contains(out.String(), "stage="+wantStage+" reason="+wantReason) {
						t.Error("doctor text lost diagnostic fields")
					}
					if wantStage == "" && (strings.Contains(out.String(), "stage=") || strings.Contains(out.String(), "reason=")) {
						t.Error("doctor text fabricated a diagnostic boundary")
					}
				}
				if strings.Contains(out.String(), "private-doctor") || smtp.Load() != 0 {
					t.Error("doctor disclosed private data or accessed SMTP")
				}
				wantDials, wantLogins, wantLists := int32(1), int32(0), int32(0)
				switch mode {
				case "dns", "tls-eof":
					wantDials = 2
				case "rejected":
					wantLogins = 2
				case "success":
					wantLogins, wantLists = 1, 1
				}
				if dials.Load() != wantDials || logins.Load() != wantLogins || lists.Load() != wantLists {
					t.Errorf("dial/login/list=%d/%d/%d want %d/%d/%d", dials.Load(), logins.Load(), lists.Load(), wantDials, wantLogins, wantLists)
				}
				t.Logf("exit=%d dial/login/list/smtp=%d/%d/%d/%d output=%s", status, dials.Load(), logins.Load(), lists.Load(), smtp.Load(), out.String())
			})
		}
	}
}

func serveDoctorIMAPFixture(conn net.Conn, mode string, logins, lists *atomic.Int32) {
	defer func() { _ = conn.Close() }()
	_ = conn.SetDeadline(time.Now().Add(time.Second))
	if _, err := io.WriteString(conn, "* OK [CAPABILITY IMAP4rev1] fixture\r\n"); err != nil {
		return
	}
	r := bufio.NewReader(conn)
	for {
		line, err := r.ReadString('\n')
		if err != nil {
			return
		}
		fields := strings.Fields(line)
		if len(fields) < 2 {
			return
		}
		tag := fields[0]
		var reply string
		switch fields[1] {
		case "LOGIN":
			logins.Add(1)
			reply = tag + " OK [CAPABILITY IMAP4rev1] authenticated\r\n"
			if mode == "rejected" {
				reply = tag + " NO [AUTHENTICATIONFAILED] private-doctor-marker\r\n"
			}
		case "LIST":
			lists.Add(1)
			reply = tag + " OK empty list\r\n"
		case "LOGOUT":
			_, _ = io.WriteString(conn, "* BYE fixture\r\n"+tag+" OK logout\r\n")
			return
		default:
			reply = tag + " BAD unexpected\r\n"
		}
		if _, err := io.WriteString(conn, reply); err != nil {
			return
		}
	}
}
