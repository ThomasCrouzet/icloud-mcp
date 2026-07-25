package imapadapter

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"strings"
	"testing"
	"time"

	"github.com/emersion/go-imap/v2"
)

func TestLoginFallsBackToFullAddressAfterAuthenticationRejection(t *testing.T) {
	// iCloud often returns a plain LOGIN NO for the local-part username without
	// AUTHENTICATIONFAILED, while accepting the full addr-spec next.
	for _, rejection := range []string{
		" NO [AUTHENTICATIONFAILED] rejected\r\n",
		" NO rejected\r\n",
	} {
		rejection := rejection
		t.Run(strings.TrimSpace(rejection), func(t *testing.T) {
			clientConn, serverConn := net.Pipe()
			commands := make(chan string, 3)
			serverErr := make(chan error, 1)
			go func() {
				defer func() { _ = serverConn.Close() }()
				reader := bufio.NewReader(serverConn)
				if _, err := fmt.Fprint(serverConn, "* OK [CAPABILITY IMAP4rev1] ready\r\n"); err != nil {
					serverErr <- err
					return
				}
				first, err := reader.ReadString('\n')
				if err != nil {
					serverErr <- err
					return
				}
				commands <- first
				if _, err := fmt.Fprint(serverConn, commandTag(first)+rejection); err != nil {
					serverErr <- err
					return
				}
				second, err := reader.ReadString('\n')
				if err != nil {
					serverErr <- err
					return
				}
				commands <- second
				if _, err := fmt.Fprint(serverConn, commandTag(second)+" OK [CAPABILITY IMAP4rev1] authenticated\r\n"); err != nil {
					serverErr <- err
					return
				}
				logout, err := reader.ReadString('\n')
				if err != nil {
					serverErr <- err
					return
				}
				commands <- logout
				_, err = fmt.Fprintf(serverConn, "* BYE closing\r\n%s OK logout\r\n", commandTag(logout))
				serverErr <- err
			}()

			ctx, cancel := context.WithTimeout(context.Background(), time.Second)
			defer cancel()
			session, err := NewSession(ctx, clientConn, "local@example.com", "test-password")
			if err != nil {
				t.Fatal(err)
			}
			if err := session.Close(); err != nil {
				t.Fatal(err)
			}
			first := <-commands
			second := <-commands
			logout := <-commands
			if !strings.Contains(first, `LOGIN "local"`) || !strings.Contains(second, `LOGIN "local@example.com"`) {
				t.Fatalf("unexpected login order:\n%s%s", first, second)
			}
			if !strings.Contains(logout, "LOGOUT") {
				t.Fatalf("missing logout: %s", logout)
			}
			if err := <-serverErr; err != nil {
				t.Fatal(err)
			}
		})
	}
}

func TestLoginDoesNotFallbackAfterNonAuthenticationNO(t *testing.T) {
	for _, test := range []struct {
		name     string
		code     string
		wantKind ErrorKind
	}{
		{name: "NOPERM", code: "NOPERM", wantKind: ErrorAuthorization},
		{name: "UNAVAILABLE", code: "UNAVAILABLE", wantKind: ErrorUnavailable},
	} {
		t.Run(test.name, func(t *testing.T) {
			clientConn, serverConn := net.Pipe()
			commands := make(chan string, 2)
			serverErr := make(chan error, 1)
			go func() {
				defer func() { _ = serverConn.Close() }()
				reader := bufio.NewReader(serverConn)
				if _, err := fmt.Fprint(serverConn, "* OK [CAPABILITY IMAP4rev1] ready\r\n"); err != nil {
					serverErr <- err
					return
				}
				first, err := reader.ReadString('\n')
				if err != nil {
					serverErr <- err
					return
				}
				commands <- first
				if _, err := fmt.Fprintf(serverConn, "%s NO [%s] rejected\r\n", commandTag(first), test.code); err != nil {
					serverErr <- err
					return
				}
				second, err := reader.ReadString('\n')
				if second != "" {
					commands <- second
					serverErr <- fmt.Errorf("unexpected command after %s: %q", test.code, second)
					return
				}
				if !errors.Is(err, io.EOF) {
					serverErr <- fmt.Errorf("connection after %s = %v", test.code, err)
					return
				}
				serverErr <- nil
			}()

			ctx, cancel := context.WithTimeout(context.Background(), time.Second)
			defer cancel()
			session, err := NewSession(ctx, clientConn, "local@example.com", "test-password")
			if session != nil || !isAdapterKind(err, test.wantKind) {
				t.Fatalf("NewSession() = %T, %v, want kind %v", session, err, test.wantKind)
			}
			first := <-commands
			if !strings.Contains(first, `LOGIN "local"`) {
				t.Fatalf("unexpected LOGIN command: %s", first)
			}
			if err := <-serverErr; err != nil {
				t.Fatal(err)
			}
			select {
			case command := <-commands:
				t.Fatalf("full-address fallback was sent: %s", command)
			default:
			}
		})
	}
}

func TestFetchBodyPartUsesPeekAndBoundedPartials(t *testing.T) {
	clientConn, serverConn := net.Pipe()
	fetchLine := make(chan string, 1)
	serverErr := make(chan error, 1)
	go func() {
		defer func() { _ = serverConn.Close() }()
		reader := bufio.NewReader(serverConn)
		if _, err := fmt.Fprint(serverConn, "* OK [CAPABILITY IMAP4rev1] ready\r\n"); err != nil {
			serverErr <- err
			return
		}
		login, err := reader.ReadString('\n')
		if err != nil {
			serverErr <- err
			return
		}
		if _, err := fmt.Fprint(serverConn, commandTag(login)+" OK [CAPABILITY IMAP4rev1] authenticated\r\n"); err != nil {
			serverErr <- err
			return
		}
		fetch, err := reader.ReadString('\n')
		if err != nil {
			serverErr <- err
			return
		}
		fetchLine <- fetch
		header := "Content-Type: text/plain; charset=utf-8\r\n\r\n"
		body := "hello"
		response := fmt.Sprintf("* 1 FETCH (UID 1 BODY[1.MIME]<0> {%d}\r\n%s BODY[1]<0> {%d}\r\n%s)\r\n%s OK fetched\r\n", len(header), header, len(body), body, commandTag(fetch))
		if _, err := fmt.Fprint(serverConn, response); err != nil {
			serverErr <- err
			return
		}
		logout, err := reader.ReadString('\n')
		if err != nil {
			serverErr <- err
			return
		}
		_, err = fmt.Fprintf(serverConn, "* BYE closing\r\n%s OK logout\r\n", commandTag(logout))
		serverErr <- err
	}()

	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	session, err := NewSession(ctx, clientConn, "local@example.com", "test-password")
	if err != nil {
		t.Fatal(err)
	}
	data, err := session.FetchBodyPart(1, []int{1})
	if err != nil {
		t.Fatal(err)
	}
	line := <-fetchLine
	if !strings.Contains(line, "BODY.PEEK[1.MIME]<0.65537>") || !strings.Contains(line, "BODY.PEEK[1]<0.524289>") || strings.Contains(line, "BODY[1]") {
		t.Fatalf("unsafe FETCH command: %s", line)
	}
	if string(data.Body) != "hello" || !strings.Contains(string(data.MIMEHeader), "text/plain") {
		t.Fatalf("unexpected body data: %+v", data)
	}
	_ = session.Close()
	if err := <-serverErr; err != nil {
		t.Fatal(err)
	}
}

func TestProtocolGuardLimitsDepthAndIgnoresLiterals(t *testing.T) {
	guard := &guardedConn{}
	for _, b := range []byte("* 1 FETCH (BODY[1] {30}\r\n") {
		if err := guard.scanProtocolByte(b); err != nil {
			t.Fatal(err)
		}
	}
	for _, b := range []byte("((((literal parentheses))))xx") {
		if guard.literalRemaining == 0 {
			t.Fatal("literal ended too early")
		}
		guard.literalRemaining--
		_ = b
	}
	if guard.depth != 1 {
		t.Fatalf("literal changed protocol depth: %d", guard.depth)
	}
	guard = &guardedConn{}
	var err error
	for i := 0; i < maxProtocolDepth+1; i++ {
		err = guard.scanProtocolByte('(')
	}
	if err != errInboundLimit {
		t.Fatalf("deep protocol nesting was not rejected: %v", err)
	}

	guard = &guardedConn{}
	for i := 0; i <= maxProtocolLists; i++ {
		if err = guard.scanProtocolByte('('); err != nil {
			break
		}
		if err = guard.scanProtocolByte(')'); err != nil {
			break
		}
	}
	if err != errInboundLimit {
		t.Fatalf("excessive protocol list count was not rejected: %v", err)
	}

	if _, err := literalSize([]byte("* 1 FETCH (BODY[] {4194305}")); err != errInboundLimit {
		t.Fatalf("oversized literal declaration was not rejected: %v", err)
	}
}

func TestExpandUIDSetRejectsUnsafeCompactRanges(t *testing.T) {
	t.Parallel()
	maxUID := ^uint32(0)
	tests := []struct {
		name         string
		set          imap.UIDSet
		requestedMin uint32
		requestedMax uint32
		maxCount     uint64
		want         []uint32
		wantError    bool
		wantKind     ErrorKind
	}{
		{
			name: "dynamic",
			set: imap.UIDSet{
				{Start: 1, Stop: 0},
			},
			requestedMin: 1,
			requestedMax: 10,
			maxCount:     10,
			wantError:    true,
			wantKind:     ErrorProtocol,
		},
		{
			name:         "search result marker",
			set:          imap.SearchRes(),
			requestedMin: 1,
			requestedMax: 10,
			maxCount:     10,
			wantError:    true,
			wantKind:     ErrorProtocol,
		},
		{
			name: "maximum UID without wrapping",
			set: imap.UIDSet{
				{Start: imap.UID(maxUID - 1), Stop: imap.UID(maxUID)},
			},
			requestedMin: maxUID - 1,
			requestedMax: maxUID,
			maxCount:     2,
			want:         []uint32{maxUID - 1, maxUID},
		},
		{
			name: "compact range over cap",
			set: imap.UIDSet{
				{Start: 1, Stop: 5001},
			},
			requestedMin: 1,
			requestedMax: 5001,
			maxCount:     5000,
			wantError:    true,
			wantKind:     ErrorTooLarge,
		},
		{
			name: "reversed",
			set: imap.UIDSet{
				{Start: 9, Stop: 2},
			},
			requestedMin: 1,
			requestedMax: 10,
			maxCount:     10,
			wantError:    true,
			wantKind:     ErrorProtocol,
		},
		{
			name: "outside requested window",
			set: imap.UIDSet{
				{Start: 9, Stop: 11},
			},
			requestedMin: 1,
			requestedMax: 10,
			maxCount:     10,
			wantError:    true,
			wantKind:     ErrorProtocol,
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			got, err := expandUIDSet(test.set, test.requestedMin, test.requestedMax, test.maxCount)
			if test.wantError {
				protocolErr, ok := err.(*Error)
				if !ok || protocolErr.Kind != test.wantKind {
					t.Fatalf("expandUIDSet() error = %T %+v, want kind %v", err, err, test.wantKind)
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			if fmt.Sprint(got) != fmt.Sprint(test.want) {
				t.Fatalf("expandUIDSet() = %v, want %v", got, test.want)
			}
		})
	}
}

func TestValidatedCopyDataRequiresScalarUIDMapping(t *testing.T) {
	t.Parallel()
	maxUID := imap.UID(^uint32(0))
	data, err := validatedCopyData(7, 9,
		imap.UIDSet{{Start: 7, Stop: 7}},
		imap.UIDSet{{Start: maxUID, Stop: maxUID}}, true)
	if err != nil || data.UIDValidity != 9 || data.DestinationUID != uint32(maxUID) {
		t.Fatalf("valid maximum COPYUID mapping = %+v, %v", data, err)
	}

	for name, test := range map[string]struct {
		source      imap.NumSet
		destination imap.NumSet
	}{
		"dynamic source": {
			source: imap.UIDSet{{Start: 1, Stop: 0}}, destination: imap.UIDSetNum(8),
		},
		"source range": {
			source: imap.UIDSet{{Start: 7, Stop: 8}}, destination: imap.UIDSetNum(8),
		},
		"reversed destination": {
			source: imap.UIDSetNum(7), destination: imap.UIDSet{{Start: 9, Stop: 8}},
		},
		"multiple destination ranges": {
			source: imap.UIDSetNum(7), destination: imap.UIDSet{{Start: 8, Stop: 8}, {Start: 10, Stop: 10}},
		},
		"wrong source": {
			source: imap.UIDSetNum(6), destination: imap.UIDSetNum(8),
		},
	} {
		t.Run(name, func(t *testing.T) {
			_, err := validatedCopyData(7, 9, test.source, test.destination, true)
			var protocolErr *Error
			if !errors.As(err, &protocolErr) || !protocolErr.Ambiguous {
				t.Fatalf("unsafe COPYUID mapping lost command ambiguity: %v", err)
			}
		})
	}
	_, err = validatedCopyData(7, 0, imap.UIDSetNum(7), imap.UIDSetNum(8), true)
	var protocolErr *Error
	if !errors.As(err, &protocolErr) || !protocolErr.Ambiguous {
		t.Fatalf("zero COPYUID UIDVALIDITY lost command ambiguity: %v", err)
	}
}

func TestExplicitAuthenticationRejectionClassification(t *testing.T) {
	t.Parallel()
	if !explicitAuthRejection(&imap.Error{Type: imap.StatusResponseTypeNo, Code: imap.ResponseCodeAuthenticationFailed}) {
		t.Fatal("explicit authentication rejection was not detected")
	}
	if explicitAuthRejection(&imap.Error{Type: imap.StatusResponseTypeNo, Code: imap.ResponseCodeNoPerm}) {
		t.Fatal("authorization rejection would trigger username fallback")
	}
}

func TestSessionCloseStalledLogoutStopsAtContextDeadline(t *testing.T) {
	clientConn, serverConn := net.Pipe()
	logoutSeen := make(chan struct{})
	serverErr := make(chan error, 1)
	go func() {
		defer func() { _ = serverConn.Close() }()
		reader := bufio.NewReader(serverConn)
		if _, err := fmt.Fprint(serverConn, "* OK [CAPABILITY IMAP4rev1] ready\r\n"); err != nil {
			serverErr <- err
			return
		}
		login, err := reader.ReadString('\n')
		if err != nil {
			serverErr <- err
			return
		}
		if _, err := fmt.Fprint(serverConn, commandTag(login)+" OK [CAPABILITY IMAP4rev1] authenticated\r\n"); err != nil {
			serverErr <- err
			return
		}
		logout, err := reader.ReadString('\n')
		if err != nil {
			serverErr <- err
			return
		}
		if !strings.Contains(logout, "LOGOUT") {
			serverErr <- fmt.Errorf("unexpected cleanup command: %q", logout)
			return
		}
		close(logoutSeen)
		if line, err := reader.ReadString('\n'); err == nil {
			serverErr <- fmt.Errorf("connection remained open after deadline: %q", line)
			return
		}
		serverErr <- nil
	}()

	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()
	session, err := NewSession(ctx, clientConn, "local@example.com", "test-password")
	if err != nil {
		t.Fatal(err)
	}
	closed := make(chan struct{})
	go func() {
		_ = session.Close()
		close(closed)
	}()
	select {
	case <-logoutSeen:
	case <-time.After(time.Second):
		t.Fatal("LOGOUT was not sent while context was live")
	}
	select {
	case <-closed:
	case <-time.After(time.Second):
		t.Fatal("stalled LOGOUT outlived the context")
	}
	secondClose := time.Now()
	if err := session.Close(); err != nil {
		t.Fatal(err)
	}
	if time.Since(secondClose) > 50*time.Millisecond {
		t.Fatal("idempotent Close blocked")
	}
	if err := <-serverErr; err != nil {
		t.Fatal(err)
	}
}

func TestSessionCloseAfterCancellationSkipsLogout(t *testing.T) {
	clientConn, serverConn := net.Pipe()
	serverErr := make(chan error, 1)
	go func() {
		defer func() { _ = serverConn.Close() }()
		reader := bufio.NewReader(serverConn)
		if _, err := fmt.Fprint(serverConn, "* OK [CAPABILITY IMAP4rev1] ready\r\n"); err != nil {
			serverErr <- err
			return
		}
		login, err := reader.ReadString('\n')
		if err != nil {
			serverErr <- err
			return
		}
		if _, err := fmt.Fprint(serverConn, commandTag(login)+" OK [CAPABILITY IMAP4rev1] authenticated\r\n"); err != nil {
			serverErr <- err
			return
		}
		line, err := reader.ReadString('\n')
		if err == nil || strings.Contains(line, "LOGOUT") {
			serverErr <- fmt.Errorf("canceled session sent cleanup command: %q, %v", line, err)
			return
		}
		serverErr <- nil
	}()

	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	session, err := NewSession(ctx, clientConn, "local@example.com", "test-password")
	if err != nil {
		cancel()
		t.Fatal(err)
	}
	cancel()
	if err := session.Close(); err != nil {
		t.Fatal(err)
	}
	if err := <-serverErr; err != nil {
		t.Fatal(err)
	}
}

func TestDeadlineConnClampPreservesAbsoluteLimit(t *testing.T) {
	t.Parallel()
	limit := time.Now().Add(time.Minute)
	earlier := limit.Add(-time.Second)
	later := limit.Add(time.Second)
	clamped := &deadlineConn{deadline: limit}
	for name, test := range map[string]struct {
		requested time.Time
		want      time.Time
	}{
		"clear":   {want: limit},
		"earlier": {requested: earlier, want: earlier},
		"later":   {requested: later, want: limit},
	} {
		t.Run(name, func(t *testing.T) {
			if got := clamped.clamp(test.requested); !got.Equal(test.want) {
				t.Fatalf("clamp() = %v, want %v", got, test.want)
			}
		})
	}
	requested := time.Now().Add(time.Second)
	if got := (&deadlineConn{}).clamp(requested); !got.Equal(requested) {
		t.Fatalf("zero absolute deadline changed requested deadline: %v", got)
	}
}

func commandTag(line string) string {
	if space := strings.IndexByte(line, ' '); space > 0 {
		return line[:space]
	}
	return "T0"
}
