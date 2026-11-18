package main

import (
	"bytes"
	"debug/pe"
	"encoding/binary"
	"os"
	"path/filepath"
	"testing"
)

func readBrandAsset(t *testing.T, name string) []byte {
	t.Helper()
	data, err := os.ReadFile(filepath.Join("..", "..", name))
	if err != nil {
		t.Fatal(err)
	}
	return data
}

func TestWindowsIconResource(t *testing.T) {
	ico := readBrandAsset(t, "assets/app-icon.ico")
	object, err := windowsResource(ico)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(object, readBrandAsset(t, "icon_windows_amd64.syso")) {
		t.Fatal("regenerate the Windows resource after changing icon assets")
	}
	f, err := pe.NewFile(bytes.NewReader(object))
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	if f.Machine != pe.IMAGE_FILE_MACHINE_AMD64 || len(f.Sections) != 1 || f.Sections[0].Name != ".rsrc" || len(f.COFFSymbols) != 1 {
		t.Fatal("invalid icon COFF structure")
	}
	section := f.Sections[0]
	data, err := section.Data()
	if err != nil {
		t.Fatal(err)
	}
	count := int(binary.LittleEndian.Uint16(ico[4:6]))
	if len(section.Relocs) != count+1 {
		t.Fatal("missing resource payload RVA relocation")
	}
	for _, r := range section.Relocs {
		if r.Type != 3 || r.SymbolTableIndex != 0 || int(r.VirtualAddress)+16 > len(data) {
			t.Fatal("invalid resource RVA relocation")
		}
	}
	group := resourceAt(t, data, 14, 1, 0)
	if !bytes.Equal(group[:6], ico[:6]) || len(group) != 6+14*count {
		t.Fatal("icon group does not cover all optical sizes")
	}
	for i := range count {
		entry := ico[6+i*16 : 6+(i+1)*16]
		groupEntry := group[6+i*14 : 6+(i+1)*14]
		id := binary.LittleEndian.Uint16(groupEntry[12:14])
		if !bytes.Equal(groupEntry[:12], entry[:12]) || id != uint16(i+1) {
			t.Fatal("icon group lost size metadata")
		}
		length := int(binary.LittleEndian.Uint32(entry[8:12]))
		offset := int(binary.LittleEndian.Uint32(entry[12:16]))
		if !bytes.Equal(resourceAt(t, data, 3, uint32(id), 0), ico[offset:offset+length]) {
			t.Fatalf("embedded icon %d differs from ICO input", id)
		}
	}
}

func resourceAt(t *testing.T, data []byte, ids ...uint32) []byte {
	t.Helper()
	offset := 0
	for depth, id := range ids {
		if offset+16 > len(data) {
			t.Fatal("truncated resource directory")
		}
		count := int(binary.LittleEndian.Uint16(data[offset+14:]))
		found := false
		for i := range count {
			entry := offset + 16 + i*8
			if entry+8 > len(data) {
				t.Fatal("truncated resource entry")
			}
			if binary.LittleEndian.Uint32(data[entry:]) != id {
				continue
			}
			target := binary.LittleEndian.Uint32(data[entry+4:])
			if depth < len(ids)-1 {
				if target&0x80000000 == 0 {
					t.Fatal("resource directory points to a leaf")
				}
				offset, found = int(target&0x7fffffff), true
				break
			}
			if target&0x80000000 != 0 || uint64(target)+16 > uint64(len(data)) {
				t.Fatal("invalid resource leaf")
			}
			start := uint64(binary.LittleEndian.Uint32(data[target:]))
			length := uint64(binary.LittleEndian.Uint32(data[target+4:]))
			if start+length > uint64(len(data)) {
				t.Fatal("resource leaf outside section")
			}
			return data[start : start+length]
		}
		if !found {
			t.Fatalf("missing resource ID %d at depth %d", id, depth)
		}
	}
	t.Fatal("resource path has no payload")
	return nil
}

func TestWindowsResourceRejectsMalformedICO(t *testing.T) {
	ico := readBrandAsset(t, "assets/app-icon.ico")
	badOffset := bytes.Clone(ico)
	binary.LittleEndian.PutUint32(badOffset[18:22], ^uint32(0))
	for _, data := range [][]byte{nil, {0, 0, 2, 0, 1, 0}, {0, 0, 1, 0, 0, 0}, ico[:21], ico[:len(ico)-1], badOffset} {
		if _, err := windowsResource(data); err == nil {
			t.Fatal("malformed ICO was accepted")
		}
	}
}
