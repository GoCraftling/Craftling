package minecraft

import (
	"bufio"
	"bytes"
	"net"
	"testing"
	"time"
)

func TestRCONCodecRoundTrip(t *testing.T) {
	var buf bytes.Buffer
	if err := writeRCON(&buf, 7, rconTypeExec, "save-all flush"); err != nil {
		t.Fatalf("writeRCON: %v", err)
	}
	id, typ, body, err := readRCON(&buf)
	if err != nil {
		t.Fatalf("readRCON: %v", err)
	}
	if id != 7 || typ != rconTypeExec || body != "save-all flush" {
		t.Errorf("round trip = (%d, %d, %q), want (7, %d, %q)", id, typ, body, rconTypeExec, "save-all flush")
	}
}

func TestRCONReadRejectsImplausibleLength(t *testing.T) {
	// length field claims 4 bytes — below the 10-byte minimum (id+type+2 nulls).
	bad := []byte{0x04, 0x00, 0x00, 0x00, 0, 0, 0, 0}
	if _, _, _, err := readRCON(bytes.NewReader(bad)); err == nil {
		t.Fatal("expected error on implausible packet length")
	}
}

func TestParsePlayerList(t *testing.T) {
	cases := []struct {
		body       string
		online     int
		max        int
		ok         bool
	}{
		{"There are 3 of a max of 20 players online: alice, bob, carol", 3, 20, true},
		{"There are 0 of a max of 20 players online:", 0, 20, true},
		{"§7There are §a5§7 of a max of §a100§7 players online", 5, 100, true},
		{"unexpected output", 0, 0, false},
		{"", 0, 0, false},
	}
	for _, c := range cases {
		online, max, ok := ParsePlayerList(c.body)
		if ok != c.ok || online != c.online || max != c.max {
			t.Errorf("ParsePlayerList(%q) = (%d, %d, %v), want (%d, %d, %v)",
				c.body, online, max, ok, c.online, c.max, c.ok)
		}
	}
}

// fakeRCONServer accepts one connection, authenticates (accepting password
// "pw"), and answers each EXECCOMMAND with reply. It exercises the full RCONExec
// path over a real TCP socket.
func fakeRCONServer(t *testing.T, password, reply string) string {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	t.Cleanup(func() { _ = ln.Close() })
	go func() {
		conn, err := ln.Accept()
		if err != nil {
			return
		}
		defer func() { _ = conn.Close() }()
		br := bufio.NewReader(conn)
		// Auth.
		id, _, body, err := readRCON(br)
		if err != nil {
			return
		}
		if body != password {
			_ = writeRCON(conn, rconAuthFailedID, rconTypeExec, "")
			return
		}
		_ = writeRCON(conn, id, rconTypeExec, "") // auth ok: echo id
		// Serve commands until the client closes.
		for {
			cid, _, _, err := readRCON(br)
			if err != nil {
				return
			}
			_ = writeRCON(conn, cid, rconTypeRespVal, reply)
		}
	}()
	return ln.Addr().String()
}

func TestRCONExecListReply(t *testing.T) {
	// RCONExec authenticates with the agent's fixed password ("1234"), so the
	// server must accept it for the exchange to succeed.
	addr := fakeRCONServer(t, "1234", "There are 2 of a max of 10 players online: a, b")
	bodies, err := RCONExec(addr, time.Second, "list")
	if err != nil {
		t.Fatalf("RCONExec: %v", err)
	}
	if len(bodies) != 1 {
		t.Fatalf("got %d bodies, want 1", len(bodies))
	}
	online, max, ok := ParsePlayerList(bodies[0])
	if !ok || online != 2 || max != 10 {
		t.Errorf("parsed (%d, %d, %v), want (2, 10, true)", online, max, ok)
	}
}

func TestRCONExecBadPassword(t *testing.T) {
	// A server configured with a different password rejects the fixed one.
	addr := fakeRCONServer(t, "some-other-password", "")
	if _, err := RCONExec(addr, time.Second, "list"); err != ErrRCONAuthFailed {
		t.Fatalf("err = %v, want ErrRCONAuthFailed", err)
	}
}
