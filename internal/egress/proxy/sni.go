package proxy

import (
	"encoding/binary"
	"io"

	"github.com/cockroachdb/errors"
)

// peekSNI extracts the SNI server name from the first TLS ClientHello on
// the connection. The bytes consumed are returned to the buffer on
// peekConn so the subsequent real TLS handshake sees them again.
func peekSNI(p *peekConn) (string, error) {
	// TLS record header is 5 bytes: type(1) + version(2) + length(2).
	header, err := p.Peek(5)
	if err != nil {
		return "", errors.Wrap(err, "peek record header")
	}
	if header[0] != 0x16 { // handshake
		return "", errors.New("not a TLS handshake record")
	}
	recordLen := int(binary.BigEndian.Uint16(header[3:5]))
	if recordLen < 4 {
		return "", errors.New("record too short")
	}
	full, err := p.Peek(5 + recordLen)
	if err != nil {
		return "", errors.Wrap(err, "peek full record")
	}
	return parseClientHelloSNI(full[5 : 5+recordLen])
}

func parseClientHelloSNI(buf []byte) (string, error) {
	// Handshake header: type(1) + length(3).
	if len(buf) < 4 {
		return "", io.ErrUnexpectedEOF
	}
	if buf[0] != 0x01 { // client_hello
		return "", errors.New("not a ClientHello")
	}
	body := buf[4:]

	// version(2) + random(32)
	if len(body) < 34 {
		return "", io.ErrUnexpectedEOF
	}
	body = body[34:]

	// session_id<0..32>
	if len(body) < 1 {
		return "", io.ErrUnexpectedEOF
	}
	sidLen := int(body[0])
	if len(body) < 1+sidLen {
		return "", io.ErrUnexpectedEOF
	}
	body = body[1+sidLen:]

	// cipher_suites<2..2^16-2>
	if len(body) < 2 {
		return "", io.ErrUnexpectedEOF
	}
	csLen := int(binary.BigEndian.Uint16(body[:2]))
	if len(body) < 2+csLen {
		return "", io.ErrUnexpectedEOF
	}
	body = body[2+csLen:]

	// compression_methods<1..2^8-1>
	if len(body) < 1 {
		return "", io.ErrUnexpectedEOF
	}
	cmLen := int(body[0])
	if len(body) < 1+cmLen {
		return "", io.ErrUnexpectedEOF
	}
	body = body[1+cmLen:]

	// extensions<0..2^16-1>
	if len(body) < 2 {
		return "", errors.New("no extensions in ClientHello")
	}
	extLen := int(binary.BigEndian.Uint16(body[:2]))
	body = body[2:]
	if len(body) < extLen {
		return "", io.ErrUnexpectedEOF
	}
	exts := body[:extLen]

	for len(exts) >= 4 {
		extType := binary.BigEndian.Uint16(exts[:2])
		extDataLen := int(binary.BigEndian.Uint16(exts[2:4]))
		if len(exts) < 4+extDataLen {
			return "", io.ErrUnexpectedEOF
		}
		extData := exts[4 : 4+extDataLen]
		exts = exts[4+extDataLen:]

		if extType != 0x0000 { // server_name
			continue
		}

		// server_name extension:
		//   list_length(2) entries of: name_type(1) + name<2..2^16-1>
		if len(extData) < 2 {
			return "", io.ErrUnexpectedEOF
		}
		listLen := int(binary.BigEndian.Uint16(extData[:2]))
		if len(extData) < 2+listLen {
			return "", io.ErrUnexpectedEOF
		}
		list := extData[2 : 2+listLen]
		for len(list) >= 3 {
			nameType := list[0]
			nameLen := int(binary.BigEndian.Uint16(list[1:3]))
			if len(list) < 3+nameLen {
				return "", io.ErrUnexpectedEOF
			}
			if nameType == 0x00 { // host_name
				return string(list[3 : 3+nameLen]), nil
			}
			list = list[3+nameLen:]
		}
	}

	return "", errors.New("no SNI in ClientHello")
}
