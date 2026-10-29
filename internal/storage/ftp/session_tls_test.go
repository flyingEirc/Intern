package ftp

import (
	"context"
	"crypto/tls"
	"net"
	"testing"
	"time"
)

// TestDataChannelHandshakeWaitsForTheTransfer pins the ordering FTPS servers
// such as Pure-FTPd rely on: the client opens the data connection before it
// sends STOR/RETR/LIST, and the server starts TLS on that connection only after
// the command arrives. A data channel therefore must be handed to the SDK as an
// encrypted-but-not-yet-handshaken connection; handshaking eagerly in the dial
// would wait for a ServerHello the server never sends at that point.
func TestDataChannelHandshakeWaitsForTheTransfer(t *testing.T) {
	for _, mode := range []string{TLSModeExplicit, TLSModeImplicit} {
		t.Run(mode, func(t *testing.T) {
			server, client := net.Pipe()
			t.Cleanup(func() {
				_ = server.Close() //nolint:errcheck // test cleanup of an in-memory pipe
				_ = client.Close() //nolint:errcheck // test cleanup of an in-memory pipe
			})
			repo := &Repository{
				cfg:       Config{TLSMode: mode, DialTimeout: 2 * time.Second},
				tlsConfig: &tls.Config{MinVersion: tls.VersionTLS12, ServerName: "ftp.example"},
				dialContext: func(context.Context, string, string) (net.Conn, error) {
					return client, nil
				},
			}
			ctx, cancel := context.WithCancel(t.Context())
			t.Cleanup(cancel)
			s := &session{repo: repo, ctx: ctx, sockets: map[*deadlineConn]struct{}{}, controlDialed: true}
			done := make(chan error, 1)
			var conn net.Conn
			go func() {
				var err error
				conn, err = s.dial("tcp", "127.0.0.1:1")
				done <- err
			}()
			select {
			case err := <-done:
				if err != nil {
					t.Fatalf("data channel dial failed: %v", err)
				}
			case <-time.After(time.Second):
				t.Fatal("the data channel dial blocked waiting for a TLS handshake the server has not started")
			}
			channel, ok := conn.(*tls.Conn)
			if !ok {
				t.Fatalf("data channel is %T, want a TLS connection", conn)
			}
			if channel.ConnectionState().HandshakeComplete {
				t.Fatal("the data channel handshook before the transfer command")
			}
		})
	}
}
