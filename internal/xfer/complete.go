package xfer

import (
	"bytes"
	"encoding/binary"
	"io"
	"os"
)

// Renders are written front to back and finished last: a QuickTime/MP4 file
// gets its movie index (moov) at the end, an MXF file its footer and random
// index pack. Until then the file can't be played. That holds even where its
// size and timestamp can't be trusted: on an SMB share, the view of a file
// another machine is writing can stand still for longer than the settle time,
// and the server may keep its mtime until the writer closes it.

type mediaFormat int

const (
	formatUnknown mediaFormat = iota // nothing to check; size and mtime decide
	formatFinished
	formatUnfinished
)

// mediaState checks a QuickTime/MP4 or MXF file of the given size for the
// parts its writer adds last.
func mediaState(path string, size int64) mediaFormat {
	fh, err := os.Open(path)
	if err != nil {
		return formatUnknown
	}
	defer fh.Close()
	var head [16]byte
	if n, _ := fh.ReadAt(head[:], 0); n < len(head) || size < int64(len(head)) {
		return formatUnknown
	}
	switch {
	case quickTimeAtom(head[4:8]):
		return quickTimeState(fh, size)
	case mxfKey(head[:], mxfHeader):
		return mxfState(fh, size)
	}
	return formatUnknown
}

func quickTimeAtom(typ []byte) bool {
	switch string(typ) {
	case "ftyp", "moov", "mdat", "wide", "free", "skip", "pnot":
		return true
	}
	return false
}

// quickTimeState walks the top-level atoms. A finished file has a moov, and
// no atom runs past the end of the file.
func quickTimeState(r io.ReaderAt, size int64) mediaFormat {
	done := func(moov bool) mediaFormat {
		if moov {
			return formatFinished
		}
		return formatUnfinished
	}
	var h [16]byte
	moov := false
	for off := int64(0); off < size; {
		n, _ := r.ReadAt(h[:min(16, size-off)], off)
		if n < 8 {
			return done(moov) // stray bytes after the last atom
		}
		length, hdr := int64(binary.BigEndian.Uint32(h[:4])), int64(8)
		switch length {
		case 0: // runs to the end of the file
			length = size - off
		case 1: // 64-bit length
			if n < 16 {
				return done(moov)
			}
			length, hdr = int64(binary.BigEndian.Uint64(h[8:16])), 16
		}
		if length < hdr {
			return done(moov) // a length the writer hasn't filled in yet, or padding
		}
		if string(h[4:8]) == "moov" {
			moov = true
		}
		if length > size-off {
			return formatUnfinished // the atom isn't all there yet
		}
		off += length
	}
	return done(moov)
}

// MXF keys: 06.0E.2B.34.02.05.01.01.0D.01.02.01.01, then the pack kind.
var mxfPrefix = []byte{0x06, 0x0e, 0x2b, 0x34, 0x02, 0x05, 0x01, 0x01, 0x0d, 0x01, 0x02, 0x01, 0x01}

const (
	mxfHeader = 0x02 // header partition pack
	mxfFooter = 0x04 // footer partition pack
	mxfRIP    = 0x11 // random index pack
)

func mxfKey(k []byte, kind byte) bool { return bytes.Equal(k[:13], mxfPrefix) && k[13] == kind }

// mxfState looks for what an MXF writer adds when it closes the file: a random
// index pack at the very end, or the footer's offset in the header partition.
func mxfState(r io.ReaderAt, size int64) mediaFormat {
	var b [64]byte
	if size >= 20 {
		if _, err := r.ReadAt(b[:4], size-4); err == nil {
			// The pack ends with its own length, key included.
			if n := int64(binary.BigEndian.Uint32(b[:4])); n >= 20 && n <= size {
				if _, err := r.ReadAt(b[:16], size-n); err == nil && mxfKey(b[:16], mxfRIP) {
					return formatFinished
				}
			}
		}
	}
	n, _ := r.ReadAt(b[:], 0)
	val := 17 // the pack follows its key and BER length
	if b[16] >= 0x80 {
		val += int(b[16] & 0x7f)
	}
	if n < val+32 {
		return formatUnfinished
	}
	// Major and minor version, KAG size, this and the previous partition: then the footer.
	footer := int64(binary.BigEndian.Uint64(b[val+24 : val+32]))
	if footer > 0 && footer <= size-16 {
		if _, err := r.ReadAt(b[:16], footer); err == nil && mxfKey(b[:16], mxfFooter) {
			return formatFinished
		}
	}
	return formatUnfinished
}
