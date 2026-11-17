package nest

import (
	"bufio"
	"bytes"
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"strconv"
	"strings"
	"time"
)

// A Minecraft server list ping: what a friend's game sees when it looks at the server. Used to
// check from outside that a way in really works.

type PingResult struct {
	Version string
	Online  int
	Max     int
	MOTD    string
}

func writeVarInt(w *bytes.Buffer, v int) {
	u := uint32(v)
	for {
		if u&^0x7f == 0 {
			w.WriteByte(byte(u))
			return
		}
		w.WriteByte(byte(u&0x7f | 0x80))
		u >>= 7
	}
}

func readVarInt(r io.ByteReader) (int, error) {
	var v uint32
	for i := 0; i < 5; i++ {
		b, err := r.ReadByte()
		if err != nil {
			return 0, err
		}
		v |= uint32(b&0x7f) << (7 * i)
		if b&0x80 == 0 {
			return int(int32(v)), nil
		}
	}
	return 0, errors.New("bad varint")
}

var errNotMinecraft = errors.New("not a Minecraft server")

// Ping asks host:port for its status.
func Ping(host string, port int, timeout time.Duration) (*PingResult, error) {
	c, err := net.DialTimeout("tcp", net.JoinHostPort(host, strconv.Itoa(port)), timeout)
	if err != nil {
		return nil, err
	}
	defer c.Close()
	c.SetDeadline(time.Now().Add(timeout))
	var hs bytes.Buffer
	writeVarInt(&hs, 0) // handshake
	writeVarInt(&hs, 767)
	writeVarInt(&hs, len(host))
	hs.WriteString(host)
	binary.Write(&hs, binary.BigEndian, uint16(port))
	writeVarInt(&hs, 1) // status
	var pkt bytes.Buffer
	writeVarInt(&pkt, hs.Len())
	pkt.Write(hs.Bytes())
	pkt.Write([]byte{1, 0}) // status request
	if _, err := c.Write(pkt.Bytes()); err != nil {
		return nil, err
	}
	r := bufio.NewReader(c)
	n, err := readVarInt(r)
	if err != nil {
		if errors.Is(err, io.EOF) {
			return nil, errSilent
		}
		return nil, err
	}
	if n <= 0 || n > 1<<20 {
		return nil, errNotMinecraft
	}
	body := make([]byte, n)
	if _, err := io.ReadFull(r, body); err != nil {
		return nil, err
	}
	br := bytes.NewReader(body)
	if id, err := readVarInt(br); err != nil || id != 0 {
		return nil, errNotMinecraft
	}
	l, err := readVarInt(br)
	if err != nil || l <= 0 || l > br.Len() {
		return nil, errNotMinecraft
	}
	js := make([]byte, l)
	br.Read(js)
	var st struct {
		Version     struct{ Name string }     `json:"version"`
		Players     struct{ Online, Max int } `json:"players"`
		Description json.RawMessage           `json:"description"`
	}
	if err := json.Unmarshal(js, &st); err != nil {
		return nil, errNotMinecraft
	}
	res := &PingResult{Version: st.Version.Name, Online: st.Players.Online, Max: st.Players.Max}
	var text string
	if json.Unmarshal(st.Description, &text) != nil {
		var obj struct{ Text string }
		json.Unmarshal(st.Description, &obj)
		text = obj.Text
	}
	res.MOTD = text
	return res, nil
}

var errSilent = errors.New("connected, but no answer came back")

// pingProblem says in words why a check from outside failed.
func pingProblem(addr string, err error) string {
	s := err.Error()
	switch {
	case strings.Contains(s, "no such host"):
		return addr + " doesn't lead anywhere yet. Try the check again in a few minutes."
	case strings.Contains(s, "refused"):
		return "Nothing is listening at " + addr + "."
	case strings.Contains(s, "timeout") || strings.Contains(s, "i/o timeout"):
		return addr + " didn't answer in time."
	case errors.Is(err, errSilent):
		return "Something took the connection at " + addr + ", but no answer came back from this computer."
	case errors.Is(err, errNotMinecraft):
		return "Something answered at " + addr + ", but not a Minecraft server."
	}
	return fmt.Sprintf("Couldn't reach %s: %v", addr, err)
}

func splitHostPort(addr string, def int) (string, int) {
	h, p, err := net.SplitHostPort(addr)
	if err != nil {
		return addr, def
	}
	n, _ := strconv.Atoi(p)
	if n == 0 {
		n = def
	}
	return h, n
}
