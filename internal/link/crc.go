package link

// DNP3 uses CRC-16/DNP: polynomial 0x3D65, reflected input and output,
// initial value 0x0000, final XOR 0xFFFF. Reflecting the polynomial for the
// right-shifting form gives 0xA6BC, which is what the table is built from.
//
// The check value for the ASCII string "123456789" is 0xEA82. crc_test.go
// asserts that, and cross-checks the table against a bitwise implementation
// over random input, so a corrupted table cannot pass silently.
const (
	crcPoly    = 0x3D65 // as printed in IEEE 1815
	crcRevPoly = 0xA6BC // crcPoly bit-reversed, for the right-shifting form
	crcXorOut  = 0xFFFF
)

var crcTable = buildCRCTable()

func buildCRCTable() [256]uint16 {
	var t [256]uint16
	for i := range t {
		crc := uint16(i)
		for range 8 {
			if crc&1 != 0 {
				crc = (crc >> 1) ^ crcRevPoly
			} else {
				crc >>= 1
			}
		}
		t[i] = crc
	}
	return t
}

// CRC computes the DNP3 CRC over data.
func CRC(data []byte) uint16 {
	var crc uint16
	for _, b := range data {
		crc = (crc >> 8) ^ crcTable[byte(crc)^b]
	}
	return crc ^ crcXorOut
}

// crcBitwise is the unoptimised reference. It exists so the table can be
// proven rather than trusted; production code calls CRC.
func crcBitwise(data []byte) uint16 {
	var crc uint16
	for _, b := range data {
		crc ^= uint16(b)
		for range 8 {
			if crc&1 != 0 {
				crc = (crc >> 1) ^ crcRevPoly
			} else {
				crc >>= 1
			}
		}
	}
	return crc ^ crcXorOut
}

// CRCValid reports whether the two octets following data hold data's CRC in
// the little-endian order DNP3 transmits it. It returns false rather than
// panicking when the slice is too short, because it is called on partially
// received buffers.
func CRCValid(data []byte, crc []byte) bool {
	if len(crc) < 2 {
		return false
	}
	want := CRC(data)
	return crc[0] == byte(want) && crc[1] == byte(want>>8)
}

// appendCRC appends the CRC of data to dst in transmission order.
func appendCRC(dst, data []byte) []byte {
	c := CRC(data)
	return append(dst, byte(c), byte(c>>8))
}
