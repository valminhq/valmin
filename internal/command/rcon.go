// Package command sends authenticated commands to supported game-server command channels.
package command

import (
	"context"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"net"
	"time"
)

const (
	packetLogin    int32 = 3
	packetCommand  int32 = 2
	maxPacketSize        = 64 << 10
	requestTimeout       = 5 * time.Second
)

var ErrAuthentication = errors.New("RCON authentication failed")

type packet struct {
	id   int32
	typ  int32
	body string
}

func executeRCON(ctx context.Context, conn net.Conn, password, command string) (string, error) {
	deadline := time.Now().Add(requestTimeout)
	if d, ok := ctx.Deadline(); ok && d.Before(deadline) {
		deadline = d
	}
	if err := conn.SetDeadline(deadline); err != nil {
		return "", fmt.Errorf("set RCON deadline: %w", err)
	}

	if err := writePacket(conn, packet{id: 1, typ: packetLogin, body: password}); err != nil {
		return "", fmt.Errorf("send RCON login: %w", err)
	}
	login, err := readPacket(conn)
	if err != nil {
		return "", fmt.Errorf("read RCON login: %w", err)
	}
	if login.id == -1 {
		return "", ErrAuthentication
	}
	if login.id != 1 || login.typ != packetCommand {
		return "", fmt.Errorf("unexpected RCON login response id=%d type=%d", login.id, login.typ)
	}

	if err := writePacket(conn, packet{id: 2, typ: packetCommand, body: command}); err != nil {
		return "", fmt.Errorf("send RCON command: %w", err)
	}
	response, err := readPacket(conn)
	if err != nil {
		return "", fmt.Errorf("read RCON command: %w", err)
	}
	if response.id != 2 || response.typ != packetCommand {
		return "", fmt.Errorf("unexpected RCON command response id=%d type=%d", response.id, response.typ)
	}
	return response.body, nil
}

func writePacket(w io.Writer, p packet) error {
	body := []byte(p.body)
	length := 4 + 4 + len(body) + 2
	if length > maxPacketSize {
		return fmt.Errorf("RCON packet is %d bytes, limit is %d", length, maxPacketSize)
	}
	raw := make([]byte, 4+length)
	binary.LittleEndian.PutUint32(raw[0:4], uint32(length))
	binary.LittleEndian.PutUint32(raw[4:8], uint32(p.id))   //nolint:gosec // RCON encodes signed int32 bits.
	binary.LittleEndian.PutUint32(raw[8:12], uint32(p.typ)) //nolint:gosec // RCON encodes signed int32 bits.
	copy(raw[12:], body)
	if _, err := w.Write(raw); err != nil {
		return fmt.Errorf("write RCON packet: %w", err)
	}
	return nil
}

func readPacket(r io.Reader) (packet, error) {
	var sizeRaw [4]byte
	if _, err := io.ReadFull(r, sizeRaw[:]); err != nil {
		return packet{}, fmt.Errorf("read RCON packet size: %w", err)
	}
	size := int(binary.LittleEndian.Uint32(sizeRaw[:]))
	if size < 10 || size > maxPacketSize {
		return packet{}, fmt.Errorf("invalid RCON packet size %d", size)
	}
	raw := make([]byte, size)
	if _, err := io.ReadFull(r, raw); err != nil {
		return packet{}, fmt.Errorf("read RCON packet body: %w", err)
	}
	if raw[len(raw)-2] != 0 || raw[len(raw)-1] != 0 {
		return packet{}, errors.New("RCON packet has no terminator")
	}
	return packet{
		id:   int32(binary.LittleEndian.Uint32(raw[0:4])), //nolint:gosec // RCON encodes signed int32 bits.
		typ:  int32(binary.LittleEndian.Uint32(raw[4:8])), //nolint:gosec // RCON encodes signed int32 bits.
		body: string(raw[8 : len(raw)-2]),
	}, nil
}
