// Package iso writes small flat ISO9660 images with Joliet names, without
// host tools. The CROWDVM config drive and NoCloud seeds use it.
//
// Primary names are mangled d-characters (CROWDVM_GUEST.;1). The Joliet
// tree holds the real names ("crowdvm-guest"), and Linux isofs and Windows
// both read it, so long lowercase names come out unchanged.
package iso

import (
	"encoding/binary"
	"fmt"
	"io"
	"os"
	"sort"
	"strings"
	"time"
	"unicode/utf16"
)

const sector = 2048

// File is one file in the image's root directory. Data is used unless Path
// (a host file, streamed) is set.
type File struct {
	Name string
	Data []byte
	Path string
}

type entry struct {
	File
	size   int64
	lba    uint32
	pName  []byte // primary (ISO9660) identifier
	jName  []byte // Joliet identifier (UCS-2 BE)
	jOrder string
}

// Write writes an image labelled label (≤16 chars) holding files.
func Write(w io.Writer, label string, files []File) error {
	if label == "" || len(label) > 16 {
		return fmt.Errorf("iso: bad label %q", label)
	}
	es := make([]*entry, len(files))
	seen := map[string]bool{}
	for i, f := range files {
		if f.Name == "" || len(f.Name) > 64 || strings.ContainsAny(f.Name, "/\\\x00") {
			return fmt.Errorf("iso: bad file name %q", f.Name)
		}
		e := &entry{File: f, size: int64(len(f.Data))}
		if f.Path != "" {
			st, err := os.Stat(f.Path)
			if err != nil {
				return err
			}
			e.size = st.Size()
		}
		if e.size >= 1<<32 {
			return fmt.Errorf("iso: %s too large", f.Name)
		}
		e.pName = []byte(primaryName(f.Name))
		if seen[string(e.pName)] {
			return fmt.Errorf("iso: names collide after mangling: %q", f.Name)
		}
		seen[string(e.pName)] = true
		e.jName = ucs2(f.Name)
		e.jOrder = string(e.jName)
		es[i] = e
	}
	now := time.Now().UTC()

	// Layout: 16 system area, PVD, Joliet SVD, terminator, 4 path tables,
	// primary root, Joliet root, file data.
	const pvdLBA, svdLBA, termLBA, lpt, mpt, jlpt, jmpt = 16, 17, 18, 19, 20, 21, 22
	pRoot := uint32(23)
	pOrder := append([]*entry(nil), es...)
	sort.Slice(pOrder, func(i, j int) bool { return string(pOrder[i].pName) < string(pOrder[j].pName) })
	jOrder := append([]*entry(nil), es...)
	sort.Slice(jOrder, func(i, j int) bool { return jOrder[i].jOrder < jOrder[j].jOrder })

	// Directory sizes do not depend on extents, so lay out with zeros first.
	pDirLen := len(dirExtent(pOrder, func(e *entry) []byte { return e.pName }, 0, 0, now))
	jDirLen := len(dirExtent(jOrder, func(e *entry) []byte { return e.jName }, 0, 0, now))
	jRoot := pRoot + uint32(pDirLen/sector)
	next := jRoot + uint32(jDirLen/sector)
	for _, e := range es {
		e.lba = next
		next += uint32((e.size + sector - 1) / sector)
	}
	total := next

	pDir := dirExtent(pOrder, func(e *entry) []byte { return e.pName }, pRoot, uint32(pDirLen), now)
	jDir := dirExtent(jOrder, func(e *entry) []byte { return e.jName }, jRoot, uint32(jDirLen), now)

	img := make([]byte, int(jRoot+uint32(jDirLen/sector))*sector)
	copy(img[pvdLBA*sector:], volDesc(1, label, total, lpt, mpt, dirRecord([]byte{0}, pRoot, uint32(pDirLen), true, now), now))
	copy(img[svdLBA*sector:], volDesc(2, label, total, jlpt, jmpt, dirRecord([]byte{0}, jRoot, uint32(jDirLen), true, now), now))
	t := img[termLBA*sector:]
	t[0] = 255
	copy(t[1:], "CD001")
	t[6] = 1
	copy(img[lpt*sector:], pathTable(pRoot, binary.LittleEndian))
	copy(img[mpt*sector:], pathTable(pRoot, binary.BigEndian))
	copy(img[jlpt*sector:], pathTable(jRoot, binary.LittleEndian))
	copy(img[jmpt*sector:], pathTable(jRoot, binary.BigEndian))
	copy(img[pRoot*sector:], pDir)
	copy(img[jRoot*sector:], jDir)
	if _, err := w.Write(img); err != nil {
		return err
	}
	for _, e := range es {
		if err := writeData(w, e); err != nil {
			return err
		}
	}
	return nil
}

// WriteFile writes the image to path (mode 0600).
func WriteFile(path, label string, files []File) error {
	f, err := os.OpenFile(path, os.O_CREATE|os.O_TRUNC|os.O_WRONLY, 0o600)
	if err != nil {
		return err
	}
	if err := Write(f, label, files); err != nil {
		f.Close()
		return err
	}
	return f.Close()
}

func writeData(w io.Writer, e *entry) error {
	var n int64
	if e.Path != "" {
		f, err := os.Open(e.Path)
		if err != nil {
			return err
		}
		n, err = io.Copy(w, io.LimitReader(f, e.size))
		f.Close()
		if err != nil {
			return err
		}
		if n != e.size {
			return fmt.Errorf("iso: %s changed size while writing", e.Path)
		}
	} else {
		if _, err := w.Write(e.Data); err != nil {
			return err
		}
		n = e.size
	}
	if pad := (sector - n%sector) % sector; pad > 0 {
		_, err := w.Write(make([]byte, pad))
		return err
	}
	return nil
}

// primaryName mangles name to ISO9660 level 2 d-characters: "NAME.EXT;1".
func primaryName(name string) string {
	up := []byte(strings.ToUpper(name))
	dot := strings.LastIndexByte(string(up), '.')
	for i, c := range up {
		if !(c >= 'A' && c <= 'Z' || c >= '0' && c <= '9' || c == '_' || c == '.' && i == dot) {
			up[i] = '_'
		}
	}
	s := string(up)
	if dot < 0 {
		s += "."
	}
	if len(s) > 30 {
		s = s[:30]
	}
	return s + ";1"
}

func ucs2(s string) []byte {
	u := utf16.Encode([]rune(s))
	b := make([]byte, 2*len(u))
	for i, c := range u {
		binary.BigEndian.PutUint16(b[2*i:], c)
	}
	return b
}

func dirExtent(es []*entry, name func(*entry) []byte, self, selfLen uint32, t time.Time) []byte {
	var dir []byte
	add := func(r []byte) {
		if rem := sector - len(dir)%sector; len(r) > rem {
			dir = append(dir, make([]byte, rem)...)
		}
		dir = append(dir, r...)
	}
	add(dirRecord([]byte{0}, self, selfLen, true, t))
	add(dirRecord([]byte{1}, self, selfLen, true, t))
	for _, e := range es {
		add(dirRecord(name(e), e.lba, uint32(e.size), false, t))
	}
	if r := len(dir) % sector; r != 0 {
		dir = append(dir, make([]byte, sector-r)...)
	}
	return dir
}

func dirRecord(name []byte, lba, size uint32, dir bool, t time.Time) []byte {
	l := 33 + len(name)
	l += l % 2
	b := make([]byte, l)
	b[0] = byte(l)
	both32(b[2:], lba)
	both32(b[10:], size)
	b[18] = byte(t.Year() - 1900)
	b[19], b[20], b[21], b[22], b[23] = byte(t.Month()), byte(t.Day()), byte(t.Hour()), byte(t.Minute()), byte(t.Second())
	if dir {
		b[25] = 2
	}
	both16(b[28:], 1)
	b[32] = byte(len(name))
	copy(b[33:], name)
	return b
}

func pathTable(root uint32, bo binary.ByteOrder) []byte {
	b := make([]byte, 10)
	b[0] = 1
	bo.PutUint32(b[2:], root)
	bo.PutUint16(b[6:], 1)
	return b
}

func volDesc(typ byte, label string, total, lpt, mpt uint32, root []byte, t time.Time) []byte {
	b := make([]byte, sector)
	b[0] = typ
	copy(b[1:], "CD001")
	b[6] = 1
	joliet := typ == 2
	str := func(off, n int, s string) {
		if joliet {
			u := ucs2(s)
			for i := 0; i+1 < n; i += 2 {
				if i+1 < len(u) {
					b[off+i], b[off+i+1] = u[i], u[i+1]
				} else {
					b[off+i], b[off+i+1] = 0, ' '
				}
			}
			return
		}
		for i := range n {
			if i < len(s) {
				b[off+i] = s[i]
			} else {
				b[off+i] = ' '
			}
		}
	}
	str(8, 32, "LINUX")
	str(40, 32, strings.ToUpper(label))
	both32(b[80:], total)
	if joliet {
		copy(b[88:], "%/E") // UCS-2 level 3
	}
	both16(b[120:], 1)
	both16(b[124:], 1)
	both16(b[128:], sector)
	both32(b[132:], 10)
	binary.LittleEndian.PutUint32(b[140:], lpt)
	binary.BigEndian.PutUint32(b[148:], mpt)
	copy(b[156:190], root)
	str(190, 128, "")        // volume set
	str(318, 128, "")        // publisher
	str(446, 128, "")        // preparer
	str(574, 128, "CROWDVM") // application
	str(702, 37+37+37-1, "") // copyright, abstract, bibliographic file ids
	date := []byte(t.Format("20060102150405") + "00\x00")
	copy(b[813:], date)
	copy(b[830:], date)
	copy(b[847:], "0000000000000000\x00")
	copy(b[864:], "0000000000000000\x00")
	b[881] = 1
	return b
}

func both16(b []byte, v uint16) {
	binary.LittleEndian.PutUint16(b, v)
	binary.BigEndian.PutUint16(b[2:], v)
}

func both32(b []byte, v uint32) {
	binary.LittleEndian.PutUint32(b, v)
	binary.BigEndian.PutUint32(b[4:], v)
}
