package image

import (
	"bytes"
	"encoding/binary"
	"fmt"
	"io"
	"sort"
	"time"
)

// ISOFile is one file in the root directory of an ISO written by WriteISO.
type ISOFile struct {
	Name string
	Data []byte
}

// WriteISO writes a minimal ISO9660 image: one root directory holding files,
// volume label label. Names are stored verbatim (no 8.3 mangling, no ";1"),
// which Linux isofs reads as-is; good enough for NoCloud seeds ("user-data")
// and the CrowdVM config drive ("crowdvm-guest"), with no host tools needed.
func WriteISO(w io.Writer, label string, files []ISOFile) error {
	const sector = 2048
	if len(label) > 32 {
		return fmt.Errorf("iso label %q longer than 32 bytes", label)
	}
	files = append([]ISOFile(nil), files...)
	sort.Slice(files, func(i, j int) bool { return files[i].Name < files[j].Name })
	for _, f := range files {
		if f.Name == "" || len(f.Name) > 200 || bytes.ContainsAny([]byte(f.Name), "/\x00") {
			return fmt.Errorf("bad iso file name %q", f.Name)
		}
		if int64(len(f.Data)) > 1<<32-1 {
			return fmt.Errorf("iso file %q too large", f.Name)
		}
	}
	now := time.Now().UTC()
	const pvdLBA, lPathLBA, mPathLBA, rootLBA = 16, 18, 19, 20

	// Root directory: ".", "..", files; records never straddle sectors.
	layoutDir := func(lbas []uint32, rootSectors uint32) []byte {
		var dir []byte
		add := func(r []byte) {
			if rem := sector - len(dir)%sector; len(r) > rem {
				dir = append(dir, make([]byte, rem)...)
			}
			dir = append(dir, r...)
		}
		add(dirRecord([]byte{0}, rootLBA, rootSectors*sector, true, now))
		add(dirRecord([]byte{1}, rootLBA, rootSectors*sector, true, now))
		for i, f := range files {
			add(dirRecord([]byte(f.Name), lbas[i], uint32(len(f.Data)), false, now))
		}
		return padTo(dir, sector)
	}
	rootSectors := uint32(len(layoutDir(make([]uint32, len(files)), 0)) / sector)
	lbas := make([]uint32, len(files))
	next := uint32(rootLBA) + rootSectors
	for i, f := range files {
		lbas[i] = next
		next += uint32((len(f.Data) + sector - 1) / sector)
	}
	total := next
	dir := layoutDir(lbas, rootSectors)

	// Path tables: just the root.
	pathTable := func(order binary.ByteOrder) []byte {
		p := make([]byte, 10)
		p[0] = 1 // identifier length
		order.PutUint32(p[2:], rootLBA)
		order.PutUint16(p[6:], 1) // parent directory number
		return padTo(p, sector)
	}

	pvd := make([]byte, sector)
	pvd[0] = 1
	copy(pvd[1:], "CD001")
	pvd[6] = 1
	copy(pvd[8:40], spaces(32))
	copy(pvd[40:72], padSpaces(label, 32))
	putBoth32(pvd[80:], total)
	putBoth16(pvd[120:], 1)
	putBoth16(pvd[124:], 1)
	putBoth16(pvd[128:], sector)
	putBoth32(pvd[132:], 10)
	binary.LittleEndian.PutUint32(pvd[140:], lPathLBA)
	binary.BigEndian.PutUint32(pvd[148:], mPathLBA)
	copy(pvd[156:190], dirRecord([]byte{0}, rootLBA, rootSectors*sector, true, now))
	copy(pvd[190:813], spaces(813-190))
	stamp := []byte(now.Format("20060102150405") + "00\x00")
	copy(pvd[813:], stamp)
	copy(pvd[830:], stamp)
	copy(pvd[847:], "0000000000000000\x00")
	copy(pvd[864:], "0000000000000000\x00")
	pvd[881] = 1

	term := make([]byte, sector)
	term[0] = 255
	copy(term[1:], "CD001")
	term[6] = 1

	var out []byte
	out = append(out, make([]byte, pvdLBA*sector)...)
	out = append(out, pvd...)
	out = append(out, term...)
	out = append(out, pathTable(binary.LittleEndian)...)
	out = append(out, pathTable(binary.BigEndian)...)
	out = append(out, dir...)
	if _, err := w.Write(out); err != nil {
		return err
	}
	for _, f := range files {
		if _, err := w.Write(padTo(f.Data, sector)); err != nil {
			return err
		}
	}
	return nil
}

func dirRecord(name []byte, lba, size uint32, dir bool, t time.Time) []byte {
	n := 33 + len(name)
	if n%2 == 1 {
		n++
	}
	r := make([]byte, n)
	r[0] = byte(n)
	putBoth32(r[2:], lba)
	putBoth32(r[10:], size)
	r[18], r[19], r[20] = byte(t.Year()-1900), byte(t.Month()), byte(t.Day())
	r[21], r[22], r[23] = byte(t.Hour()), byte(t.Minute()), byte(t.Second())
	if dir {
		r[25] = 2
	}
	putBoth16(r[28:], 1)
	r[32] = byte(len(name))
	copy(r[33:], name)
	return r
}

func putBoth16(b []byte, v uint16) {
	binary.LittleEndian.PutUint16(b, v)
	binary.BigEndian.PutUint16(b[2:], v)
}

func putBoth32(b []byte, v uint32) {
	binary.LittleEndian.PutUint32(b, v)
	binary.BigEndian.PutUint32(b[4:], v)
}

func padTo(b []byte, n int) []byte {
	if r := len(b) % n; r != 0 {
		b = append(b[:len(b):len(b)], make([]byte, n-r)...)
	}
	return b
}

func spaces(n int) []byte { return bytes.Repeat([]byte{' '}, n) }

func padSpaces(s string, n int) []byte { return append([]byte(s), spaces(n-len(s))...) }
