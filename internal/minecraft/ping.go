package minecraft

import (
	"bufio"
	"bytes"
	"context"
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"time"
)

// DefaultPingTimeout bounds a whole Server List Ping (dial + handshake + status
// read) when a caller passes a non-positive timeout.
const DefaultPingTimeout = 5 * time.Second

// statusProtocolVersion is the protocol number sent in the handshake. -1 is the
// conventional "just querying status" value: the server replies with its own
// version regardless, so the exact number doesn't matter for a status ping.
const statusProtocolVersion = -1

// maxStatusPacket caps the status response we'll read, so a hostile or broken
// server can't make us allocate unboundedly. Real status JSON (favicon included)
// is well under this.
const maxStatusPacket = 1 << 20 // 1 MiB

// Status is the decoded Server List Ping response: the subset of the server's
// status JSON we surface as deep health. Description (the MOTD) is intentionally
// dropped — it can be a string or a nested chat component, and player count plus
// version is all the health probe needs.
type Status struct {
	VersionName     string
	ProtocolVersion int
	PlayersOnline   int
	PlayersMax      int
}

// statusJSON mirrors the wire shape of the status response we care about.
type statusJSON struct {
	Version struct {
		Name     string `json:"name"`
		Protocol int    `json:"protocol"`
	} `json:"version"`
	Players struct {
		Max    int `json:"max"`
		Online int `json:"online"`
	} `json:"players"`
}

// Ping performs a Server List Ping handshake against a Minecraft server and
// returns its status. It is the unauthenticated liveness + player-count probe: a
// successful read means the game process — not just the VM — is up and accepting
// connections. A non-positive timeout falls back to DefaultPingTimeout.
func Ping(ctx context.Context, addr string, timeout time.Duration) (*Status, error) {
	if timeout <= 0 {
		timeout = DefaultPingTimeout
	}
	d := net.Dialer{Timeout: timeout}
	conn, err := d.DialContext(ctx, "tcp", addr)
	if err != nil {
		return nil, fmt.Errorf("ping dial %s: %w", addr, err)
	}
	defer func() { _ = conn.Close() }()
	_ = conn.SetDeadline(time.Now().Add(timeout))

	host, port, err := splitHostPort(addr)
	if err != nil {
		return nil, err
	}
	if err := writeHandshake(conn, host, port); err != nil {
		return nil, fmt.Errorf("ping handshake: %w", err)
	}
	if err := writePacket(conn, []byte{0x00}); err != nil { // empty Status Request
		return nil, fmt.Errorf("ping status request: %w", err)
	}

	raw, err := readStatusJSON(bufio.NewReader(conn))
	if err != nil {
		return nil, fmt.Errorf("ping status response: %w", err)
	}
	var s statusJSON
	if err := json.Unmarshal(raw, &s); err != nil {
		return nil, fmt.Errorf("ping decode status: %w", err)
	}
	return &Status{
		VersionName:     s.Version.Name,
		ProtocolVersion: s.Version.Protocol,
		PlayersOnline:   s.Players.Online,
		PlayersMax:      s.Players.Max,
	}, nil
}

// writeHandshake sends the C→S Handshake (packet id 0x00) requesting the status
// (next-state 1) followed by the server address/port the client dialed.
func writeHandshake(w io.Writer, host string, port uint16) error {
	var b bytes.Buffer
	b.WriteByte(0x00) // packet id: handshake
	writeVarInt(&b, statusProtocolVersion)
	writeString(&b, host)
	_ = binary.Write(&b, binary.BigEndian, port)
	writeVarInt(&b, 1) // next state: status
	return writePacket(w, b.Bytes())
}

// readStatusJSON reads the S→C Status Response (packet id 0x00) and returns the
// JSON payload string within it.
func readStatusJSON(r *bufio.Reader) ([]byte, error) {
	length, err := readVarInt(r)
	if err != nil {
		return nil, err
	}
	if length <= 0 || length > maxStatusPacket {
		return nil, fmt.Errorf("implausible packet length %d", length)
	}
	body := make([]byte, length)
	if _, err := io.ReadFull(r, body); err != nil {
		return nil, err
	}
	br := bytes.NewReader(body)
	id, err := readVarInt(br)
	if err != nil {
		return nil, err
	}
	if id != 0x00 {
		return nil, fmt.Errorf("unexpected packet id 0x%02x", id)
	}
	strLen, err := readVarInt(br)
	if err != nil {
		return nil, err
	}
	if strLen < 0 || strLen > maxStatusPacket {
		return nil, fmt.Errorf("implausible status length %d", strLen)
	}
	out := make([]byte, strLen)
	if _, err := io.ReadFull(br, out); err != nil {
		return nil, err
	}
	return out, nil
}

// writePacket frames a packet body with its VarInt length prefix.
func writePacket(w io.Writer, body []byte) error {
	var b bytes.Buffer
	writeVarInt(&b, len(body))
	b.Write(body)
	_, err := w.Write(b.Bytes())
	return err
}

// writeString writes a VarInt length-prefixed UTF-8 string.
func writeString(b *bytes.Buffer, s string) {
	writeVarInt(b, len(s))
	b.WriteString(s)
}

// writeVarInt encodes n as a Minecraft VarInt (LEB128, 7 bits per byte).
func writeVarInt(b *bytes.Buffer, n int) {
	u := uint32(n)
	for {
		if u&^0x7f == 0 {
			b.WriteByte(byte(u))
			return
		}
		b.WriteByte(byte(u&0x7f | 0x80))
		u >>= 7
	}
}

// readVarInt decodes a Minecraft VarInt, rejecting an over-long encoding (>5
// bytes) so a desynced stream can't spin forever.
func readVarInt(r io.ByteReader) (int, error) {
	var result uint32
	for i := 0; i < 5; i++ {
		bt, err := r.ReadByte()
		if err != nil {
			return 0, err
		}
		result |= uint32(bt&0x7f) << (7 * i)
		if bt&0x80 == 0 {
			return int(int32(result)), nil
		}
	}
	return 0, errors.New("varint too long")
}

// splitHostPort splits an "host:port" address into its parts, defaulting to the
// standard Minecraft port when none is given.
func splitHostPort(addr string) (string, uint16, error) {
	host, portStr, err := net.SplitHostPort(addr)
	if err != nil {
		return "", 0, fmt.Errorf("ping address %q: %w", addr, err)
	}
	p, err := net.LookupPort("tcp", portStr)
	if err != nil {
		return "", 0, fmt.Errorf("ping port %q: %w", portStr, err)
	}
	return host, uint16(p), nil
}
