package link

import (
	"context"
	"log/slog"
	"net"
	"strings"
	"time"
)

// dnsForwarder is a minimal UDP DNS forwarder. It parses the question name,
// optionally filters against an allowlist (responding with NXDOMAIN), and
// forwards everything else to the host resolver. It is defense in depth on
// top of the proxy's allowlist — name resolution is not the primary
// enforcement point.
type dnsForwarder struct {
	conn    net.PacketConn
	allowed []string // exact and "*.suffix"; nil means allow everything
	log     *slog.Logger
}

func (d *dnsForwarder) run(ctx context.Context) {
	buf := make([]byte, 1500)
	for {
		if ctx.Err() != nil {
			return
		}
		d.conn.SetReadDeadline(time.Now().Add(time.Second))
		n, src, err := d.conn.ReadFrom(buf)
		if err != nil {
			if ne, ok := err.(net.Error); ok && ne.Timeout() {
				continue
			}
			return
		}
		req := append([]byte(nil), buf[:n]...)
		go d.handle(ctx, src, req)
	}
}

func (d *dnsForwarder) handle(ctx context.Context, src net.Addr, req []byte) {
	name, qtype, ok := parseDNSQuestion(req)
	if !ok {
		return
	}

	if !d.permit(name) {
		d.log.Info("dns blocked", "name", name)
		_, _ = d.conn.WriteTo(buildNXDOMAIN(req), src)
		return
	}

	resp, err := d.forward(ctx, req, name, qtype)
	if err != nil {
		_, _ = d.conn.WriteTo(buildServFail(req), src)
		return
	}
	_, _ = d.conn.WriteTo(resp, src)
}

func (d *dnsForwarder) permit(name string) bool {
	if len(d.allowed) == 0 {
		return true
	}
	name = strings.ToLower(strings.TrimSuffix(name, "."))
	for _, a := range d.allowed {
		a = strings.ToLower(a)
		if strings.HasPrefix(a, "*.") {
			suffix := a[1:]
			if strings.HasSuffix(name, suffix) && len(name) > len(suffix) {
				return true
			}
		} else if name == a {
			return true
		}
	}
	return false
}

// forward issues the upstream lookup via net.DefaultResolver and synthesizes
// an answer packet. We don't proxy the DNS protocol bytes — this is enough
// for A/AAAA lookups, which are what the VM does in practice.
func (d *dnsForwarder) forward(ctx context.Context, req []byte, name string, qtype uint16) ([]byte, error) {
	ctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()

	switch qtype {
	case 1: // A
		ips, err := net.DefaultResolver.LookupIP(ctx, "ip4", name)
		if err != nil {
			return nil, err
		}
		return buildAnswer(req, name, qtype, ips), nil
	case 28: // AAAA
		ips, err := net.DefaultResolver.LookupIP(ctx, "ip6", name)
		if err != nil {
			return nil, err
		}
		return buildAnswer(req, name, qtype, ips), nil
	default:
		// Other RR types fall through to NXDOMAIN; fine for our use case.
		return buildNXDOMAIN(req), nil
	}
}

// parseDNSQuestion extracts the QNAME and QTYPE from a DNS query message.
func parseDNSQuestion(msg []byte) (string, uint16, bool) {
	if len(msg) < 12 {
		return "", 0, false
	}
	// Header: ID(2) flags(2) qdcount(2) ancount(2) nscount(2) arcount(2)
	// Then questions; we only parse the first.
	off := 12
	var name strings.Builder
	for off < len(msg) {
		l := int(msg[off])
		off++
		if l == 0 {
			break
		}
		if l&0xC0 != 0 || off+l > len(msg) {
			return "", 0, false
		}
		if name.Len() > 0 {
			name.WriteByte('.')
		}
		name.Write(msg[off : off+l])
		off += l
	}
	if off+4 > len(msg) {
		return "", 0, false
	}
	qtype := uint16(msg[off])<<8 | uint16(msg[off+1])
	return name.String(), qtype, true
}

// buildAnswer constructs a minimal DNS response for an A/AAAA query.
func buildAnswer(req []byte, name string, qtype uint16, ips []net.IP) []byte {
	resp := append([]byte(nil), req...)
	// Set QR=1 (response), keep RD bit; set RA=1.
	resp[2] = (resp[2] | 0x80) // QR
	resp[3] = (resp[3] | 0x80) // RA
	// Count answers.
	var count int
	for _, ip := range ips {
		if qtype == 1 && ip.To4() != nil {
			count++
		} else if qtype == 28 && ip.To4() == nil {
			count++
		}
	}
	resp[6] = byte(count >> 8)
	resp[7] = byte(count)

	// Append answer records: NAME(ptr to question, 0xc00c), TYPE, CLASS=IN,
	// TTL=60, RDLENGTH, RDATA.
	for _, ip := range ips {
		var rdata []byte
		switch qtype {
		case 1:
			ip4 := ip.To4()
			if ip4 == nil {
				continue
			}
			rdata = ip4
		case 28:
			if ip.To4() != nil {
				continue
			}
			rdata = ip.To16()
		}
		resp = append(resp,
			0xc0, 0x0c,
			byte(qtype>>8), byte(qtype),
			0x00, 0x01,
			0x00, 0x00, 0x00, 60,
			byte(len(rdata)>>8), byte(len(rdata)),
		)
		resp = append(resp, rdata...)
	}
	_ = name // suppress unused; included for future expansion (CNAMEs etc.)
	return resp
}

// buildNXDOMAIN flips the response bits and sets RCODE=3.
func buildNXDOMAIN(req []byte) []byte {
	resp := append([]byte(nil), req...)
	if len(resp) < 12 {
		return resp
	}
	resp[2] |= 0x80                // QR
	resp[3] = (resp[3] & 0xf0) | 3 // RCODE = 3 (NXDOMAIN)
	resp[6], resp[7] = 0, 0
	resp[8], resp[9] = 0, 0
	resp[10], resp[11] = 0, 0
	return resp
}

// buildServFail flips the response bits and sets RCODE=2.
func buildServFail(req []byte) []byte {
	resp := append([]byte(nil), req...)
	if len(resp) < 12 {
		return resp
	}
	resp[2] |= 0x80
	resp[3] = (resp[3] & 0xf0) | 2
	return resp
}
