package ciartifacts

import (
	"bytes"
	"context"
	"encoding/binary"
)

// preflightZIP accepts only single-disk, non-ZIP64 ZIPs with an exact central
// directory, bounded metadata and matching local headers. It allocates no
// entry-sized structures. Each loop is bounded by 1000 entries or 65558 EOCD
// candidate bytes; cancellation is checked on every iteration.
func preflightZIP(ctx context.Context, b []byte) bool {
	if len(b) < 22 || len(b) > 64<<20 {
		return false
	}
	u16 := func(p int) uint64 { return uint64(binary.LittleEndian.Uint16(b[p : p+2])) }
	u32 := func(p int) uint64 { return uint64(binary.LittleEndian.Uint32(b[p : p+4])) }
	end := -1
	for p := len(b) - 22; p >= max(0, len(b)-22-65535); p-- {
		if ctx.Err() != nil {
			return false
		}
		commentBytes := int(binary.LittleEndian.Uint16(b[p+20 : p+22]))
		if bytes.Equal(b[p:p+4], []byte{'P', 'K', 5, 6}) && p+22+commentBytes <= len(b) {
			// Match archive/zip's last plausible EOCD, refusing trailing junk.
			if p+22+commentBytes != len(b) {
				return false
			}
			end = p
			break
		}
	}
	if end < 0 || u16(end+4) != 0 || u16(end+6) != 0 {
		return false
	}
	count := u16(end + 10)
	if count > 1000 || u16(end+8) != count {
		return false
	}
	size, start := u32(end+12), u32(end+16)
	if size == 0xffffffff || start == 0xffffffff || start+size != uint64(end) {
		return false
	}
	// ZIP64 locator immediately before EOCD is forbidden, even with small fields.
	if end >= 20 && u32(end-20) == 0x07064b50 {
		return false
	}
	pos := start
	aggregate := uint64(0)
	var spans [1000][2]uint64
	for i := uint64(0); i < count; i++ {
		if ctx.Err() != nil || pos > 64<<20 || pos+46 > uint64(end) {
			return false
		}
		p := int(pos)
		if u32(p) != 0x02014b50 || u16(p+8)&(1|64|8192) != 0 || u16(p+34) != 0 {
			return false
		}
		compressed, plain, local := u32(p+20), u32(p+24), u32(p+42)
		if compressed == 0xffffffff || plain == 0xffffffff || local == 0xffffffff {
			return false
		}
		name, extra, comment := u16(p+28), u16(p+30), u16(p+32)
		aggregate += name + extra + comment
		next := pos + 46 + name + extra + comment
		if name > 4096 || aggregate > 1<<20 || next > uint64(end) {
			return false
		}
		extraEnd := pos + 46 + name + extra
		if extraEnd > 64<<20 {
			return false
		}
		if !safeExtra(b[p+46+int(name) : int(extraEnd)]) {
			return false
		}
		if local > 64<<20 || local+30 > start {
			return false
		}
		l := int(local)
		if u32(l) != 0x04034b50 || u16(l+6) != u16(p+8) || u16(l+8) != u16(p+10) {
			return false
		}
		ln, le := u16(l+26), u16(l+28)
		data := local + 30 + ln + le
		if ln > 4096 || ln != name || le > 1<<20 || data > 64<<20 || data > start || compressed > start-data {
			return false
		}
		if !bytes.Equal(b[l+30:l+30+int(ln)], b[p+46:p+46+int(name)]) || !safeExtra(b[l+30+int(ln):int(data)]) {
			return false
		}
		// Size consistency protects entry boundaries. CRC disagreements are
		// entry-local integrity failures checked by archive/zip during inflation.
		if u16(p+8)&8 == 0 && (u32(l+18) != compressed || u32(l+22) != plain) {
			return false
		}
		localEnd := data + compressed
		if u16(p+8)&8 != 0 {
			if localEnd > 64<<20 || localEnd+12 > start {
				return false
			}
			d := int(localEnd)
			if u32(d) == 0x08074b50 {
				d += 4
				localEnd += 4
			}
			if localEnd+12 > start || u32(d+4) != compressed || u32(d+8) != plain {
				return false
			}
			localEnd += 12
		}
		// Central entries may be out of local order but may never alias or overlap
		// local headers, compressed streams, or descriptors. At most 499500 pairs.
		for j := uint64(0); j < i; j++ {
			if local < spans[j][1] && spans[j][0] < localEnd {
				return false
			}
		}
		spans[i] = [2]uint64{local, localEnd}
		pos = next
	}
	return pos == uint64(end)
}

func safeExtra(b []byte) bool {
	for len(b) > 0 {
		if len(b) < 4 {
			return false
		}
		tag := binary.LittleEndian.Uint16(b)
		n := int(binary.LittleEndian.Uint16(b[2:]))
		if tag == 1 || n > len(b)-4 {
			return false
		}
		b = b[4+n:]
	}
	return true
}
