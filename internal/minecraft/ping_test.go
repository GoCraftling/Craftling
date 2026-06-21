package minecraft

import (
	"bufio"
	"bytes"
	"context"
	"net"
	"testing"
	"time"
)

// fakeStatusServer accepts one Server List Ping handshake + status request and
// replies with the given status JSON, exercising the full Ping path over a real
// TCP socket.
func fakeStatusServer(t *testing.T, statusJSON string) string {
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
		// Read and discard the handshake and status-request packets.
		if _, err := readRawPacket(br); err != nil {
			return
		}
		if _, err := readRawPacket(br); err != nil {
			return
		}
		// Reply with a Status Response: packet id 0x00 + length-prefixed JSON.
		var body bytes.Buffer
		body.WriteByte(0x00)
		writeString(&body, statusJSON)
		_ = writePacket(conn, body.Bytes())
	}()
	return ln.Addr().String()
}

// readRawPacket reads one length-prefixed packet and returns its body.
func readRawPacket(r *bufio.Reader) ([]byte, error) {
	n, err := readVarInt(r)
	if err != nil {
		return nil, err
	}
	buf := make([]byte, n)
	if _, err := readFull(r, buf); err != nil {
		return nil, err
	}
	return buf, nil
}

func readFull(r *bufio.Reader, buf []byte) (int, error) {
	got := 0
	for got < len(buf) {
		n, err := r.Read(buf[got:])
		got += n
		if err != nil {
			return got, err
		}
	}
	return got, nil
}

func TestPingDecodesStatus(t *testing.T) {
	addr := fakeStatusServer(t, `{"version":{"name":"1.20.1","protocol":763},"players":{"max":20,"online":4},"description":"Hi"}`)
	st, err := Ping(context.Background(), addr, time.Second)
	if err != nil {
		t.Fatalf("Ping: %v", err)
	}
	if st.VersionName != "1.20.1" || st.ProtocolVersion != 763 {
		t.Errorf("version = %q/%d, want 1.20.1/763", st.VersionName, st.ProtocolVersion)
	}
	if st.PlayersOnline != 4 || st.PlayersMax != 20 {
		t.Errorf("players = %d/%d, want 4/20", st.PlayersOnline, st.PlayersMax)
	}
}

func TestPingErrorsWhenUnreachable(t *testing.T) {
	// Reserve a port, then close it so the dial is refused promptly.
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	addr := ln.Addr().String()
	_ = ln.Close()
	if _, err := Ping(context.Background(), addr, 500*time.Millisecond); err == nil {
		t.Fatal("expected error pinging a closed port")
	}
}

func TestVarIntRoundTrip(t *testing.T) {
	for _, n := range []int{0, 1, 127, 128, 255, 25565, 2097151, 2147483647, -1} {
		var b bytes.Buffer
		writeVarInt(&b, n)
		got, err := readVarInt(bytes.NewReader(b.Bytes()))
		if err != nil {
			t.Fatalf("readVarInt(%d): %v", n, err)
		}
		if got != n {
			t.Errorf("varint round trip %d = %d", n, got)
		}
	}
}
