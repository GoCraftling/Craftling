// Package minecraft holds small, dependency-free clients for talking to a
// Minecraft server: Source RCON (an authenticated command channel) and the
// Server List Ping handshake (the unauthenticated status query a client runs
// before joining). Both are pure net/encoding so they unit-test on any host and
// are reused by the in-guest init agent (cmd/init), which proxies them over the
// loopback for the host's deep-health probe (P7).
package minecraft

import (
	"bufio"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"net"
	"regexp"
	"strconv"
	"time"
)

// Source RCON packet types. We only need AUTH and EXECCOMMAND; the server
// answers AUTH with an AUTH_RESPONSE (which reuses the EXECCOMMAND type id) and
// EXECCOMMAND with one or more RESPONSE_VALUE packets.
const (
	rconTypeAuth     int32 = 3 // SERVERDATA_AUTH
	rconTypeExec     int32 = 2 // SERVERDATA_EXECCOMMAND (also AUTH_RESPONSE)
	rconTypeRespVal  int32 = 0 // SERVERDATA_RESPONSE_VALUE
	rconAuthFailedID int32 = -1
	rconAuthID       int32 = 1
	rconExecID       int32 = 2
	rconMaxBody            = 4096 // generous cap on a single packet body
)

// DefaultRCONTimeout bounds a whole RCON exchange (dial + auth + commands) when
// a caller passes a non-positive timeout.
const DefaultRCONTimeout = 5 * time.Second

// ErrRCONAuthFailed means the server rejected the password.
var ErrRCONAuthFailed = errors.New("rcon auth failed (bad password)")

// RCONExec dials a Source RCON endpoint, authenticates, runs each command in
// order, and returns each command's response body (positionally matching cmds).
// A non-positive timeout falls back to DefaultRCONTimeout. It is used both to
// quiesce a server before a snapshot ("save-off"/"save-all flush"/"save-on",
// whose replies are ignored) and to probe player counts ("list").
func RCONExec(addr, password string, timeout time.Duration, cmds ...string) ([]string, error) {
	if timeout <= 0 {
		timeout = DefaultRCONTimeout
	}
	conn, err := net.DialTimeout("tcp", addr, timeout)
	if err != nil {
		return nil, fmt.Errorf("rcon dial %s: %w", addr, err)
	}
	defer func() { _ = conn.Close() }()
	_ = conn.SetDeadline(time.Now().Add(timeout))
	br := bufio.NewReader(conn)

	if err := writeRCON(conn, rconAuthID, rconTypeAuth, password); err != nil {
		return nil, fmt.Errorf("rcon auth send: %w", err)
	}
	id, _, _, err := readRCON(br)
	if err != nil {
		return nil, fmt.Errorf("rcon auth reply: %w", err)
	}
	if id == rconAuthFailedID {
		return nil, ErrRCONAuthFailed
	}

	out := make([]string, 0, len(cmds))
	for _, c := range cmds {
		if err := writeRCON(conn, rconExecID, rconTypeExec, c); err != nil {
			return nil, fmt.Errorf("rcon exec %q send: %w", c, err)
		}
		_, _, body, err := readRCON(br)
		if err != nil {
			return nil, fmt.Errorf("rcon exec %q reply: %w", c, err)
		}
		out = append(out, body)
	}
	return out, nil
}

// listPattern matches the vanilla "/list" reply, e.g.
// "There are 3 of a max of 20 players online: alice, bob". Modded servers can
// dress it up, but the "N of a max of M" core is near-universal, so we anchor on
// that rather than the exact prefix.
var listPattern = regexp.MustCompile(`(\d+)\s*of a max of\s*(\d+)`)

// colorCodes matches Minecraft section-sign formatting codes ("§a", "§7", …),
// which some servers embed in RCON output. Stripping them lets the count parser
// see plain text.
var colorCodes = regexp.MustCompile(`§.`)

// ParsePlayerList extracts the online/max counts from a "/list" RCON reply,
// reporting ok=false when the body doesn't match the expected shape (an empty
// reply, an unrecognized server, a command error) so the caller can fall back to
// another probe rather than trust a zeroed count.
func ParsePlayerList(body string) (online, max int, ok bool) {
	m := listPattern.FindStringSubmatch(colorCodes.ReplaceAllString(body, ""))
	if m == nil {
		return 0, 0, false
	}
	online, err1 := strconv.Atoi(m[1])
	max, err2 := strconv.Atoi(m[2])
	if err1 != nil || err2 != nil {
		return 0, 0, false
	}
	return online, max, true
}

// writeRCON encodes one packet. Length counts everything after the length field
// itself: id(4) + type(4) + body + two null bytes.
func writeRCON(w io.Writer, id, typ int32, body string) error {
	payloadLen := 4 + 4 + len(body) + 2
	buf := make([]byte, 4+payloadLen)
	binary.LittleEndian.PutUint32(buf[0:4], uint32(payloadLen))
	binary.LittleEndian.PutUint32(buf[4:8], uint32(id))
	binary.LittleEndian.PutUint32(buf[8:12], uint32(typ))
	copy(buf[12:], body)
	// last two bytes already zero (body null terminator + packet pad)
	_, err := w.Write(buf)
	return err
}

// readRCON decodes one packet, returning its id, type, and body (without the
// trailing nulls). It rejects absurd lengths so a hostile or desynced peer can't
// make us allocate unboundedly.
func readRCON(r io.Reader) (id, typ int32, body string, err error) {
	var lenField [4]byte
	if _, err = io.ReadFull(r, lenField[:]); err != nil {
		return 0, 0, "", err
	}
	n := int(binary.LittleEndian.Uint32(lenField[:]))
	if n < 10 || n > rconMaxBody+10 {
		return 0, 0, "", fmt.Errorf("rcon: implausible packet length %d", n)
	}
	payload := make([]byte, n)
	if _, err = io.ReadFull(r, payload); err != nil {
		return 0, 0, "", err
	}
	id = int32(binary.LittleEndian.Uint32(payload[0:4]))
	typ = int32(binary.LittleEndian.Uint32(payload[4:8]))
	// body is payload[8:] minus the two trailing null bytes.
	b := payload[8:]
	b = b[:len(b)-2]
	return id, typ, string(b), nil
}
