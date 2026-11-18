package main

import (
	"bytes"
	"debug/pe"
	"encoding/binary"
	"fmt"
)

type resourceTree struct {
	data   []byte
	relocs []uint32
}

func (r *resourceTree) allocate(size int) int {
	r.data = append(r.data, make([]byte, (-len(r.data))&3)...)
	offset := len(r.data)
	r.data = append(r.data, make([]byte, size)...)
	return offset
}

func (r *resourceTree) directory(ids []uint32) (int, []int) {
	offset := r.allocate(16 + 8*len(ids))
	binary.LittleEndian.PutUint16(r.data[offset+14:], uint16(len(ids)))
	entries := make([]int, len(ids))
	for i, id := range ids {
		entries[i] = offset + 16 + i*8
		binary.LittleEndian.PutUint32(r.data[entries[i]:], id)
	}
	return offset, entries
}

func (r *resourceTree) branch(entry, directory int) {
	binary.LittleEndian.PutUint32(r.data[entry+4:], uint32(directory)|0x80000000)
}

func (r *resourceTree) leaf(entry int, data []byte) {
	descriptor := r.allocate(16)
	binary.LittleEndian.PutUint32(r.data[entry+4:], uint32(descriptor))
	payload := r.allocate(len(data))
	copy(r.data[payload:], data)
	binary.LittleEndian.PutUint32(r.data[descriptor:], uint32(payload))
	binary.LittleEndian.PutUint32(r.data[descriptor+4:], uint32(len(data)))
	r.relocs = append(r.relocs, uint32(descriptor))
}

// windowsResource emits a deterministic amd64 COFF object without a C toolchain.
// The linker fixes only the payload RVAs; resource-tree offsets stay section-relative.
func windowsResource(ico []byte) ([]byte, error) {
	if len(ico) < 6 || binary.LittleEndian.Uint16(ico[:2]) != 0 || binary.LittleEndian.Uint16(ico[2:4]) != 1 {
		return nil, fmt.Errorf("invalid ICO header")
	}
	count := int(binary.LittleEndian.Uint16(ico[4:6]))
	if count == 0 || count > 256 || len(ico) < 6+16*count {
		return nil, fmt.Errorf("invalid ICO image count")
	}
	var tree resourceTree
	_, types := tree.directory([]uint32{3, 14}) // RT_ICON, RT_GROUP_ICON.
	ids := make([]uint32, count)
	for i := range ids {
		ids[i] = uint32(i + 1)
	}
	names, images := tree.directory(ids)
	tree.branch(types[0], names)
	group := append([]byte(nil), ico[:6]...)
	for i, entry := range images {
		icon := ico[6+16*i : 6+16*(i+1)]
		length := uint64(binary.LittleEndian.Uint32(icon[8:12]))
		offset := uint64(binary.LittleEndian.Uint32(icon[12:16]))
		if length == 0 || offset < uint64(6+16*count) || offset+length > uint64(len(ico)) {
			return nil, fmt.Errorf("invalid ICO image %d range", i+1)
		}
		languages, language := tree.directory([]uint32{0})
		tree.branch(entry, languages)
		tree.leaf(language[0], ico[offset:offset+length])
		group = append(group, icon[:12]...)
		group = binary.LittleEndian.AppendUint16(group, uint16(i+1))
	}
	names, groups := tree.directory([]uint32{1})
	tree.branch(types[1], names)
	languages, language := tree.directory([]uint32{0})
	tree.branch(groups[0], languages)
	tree.leaf(language[0], group)
	tree.allocate(0)

	const (
		headerSize = 20 + 40
		align4     = 0x00300000 // IMAGE_SCN_ALIGN_4BYTES.
	)
	relocationOffset := uint32(headerSize + len(tree.data))
	symbolOffset := relocationOffset + uint32(10*len(tree.relocs))
	header := pe.FileHeader{
		Machine: pe.IMAGE_FILE_MACHINE_AMD64, NumberOfSections: 1,
		PointerToSymbolTable: symbolOffset, NumberOfSymbols: 1,
	}
	name := [8]byte{'.', 'r', 's', 'r', 'c'}
	section := pe.SectionHeader32{
		Name: name, SizeOfRawData: uint32(len(tree.data)), PointerToRawData: headerSize,
		PointerToRelocations: relocationOffset, NumberOfRelocations: uint16(len(tree.relocs)),
		Characteristics: pe.IMAGE_SCN_CNT_INITIALIZED_DATA | pe.IMAGE_SCN_MEM_READ | align4,
	}
	var object bytes.Buffer
	for _, value := range []any{header, section, tree.data} {
		if err := binary.Write(&object, binary.LittleEndian, value); err != nil {
			return nil, fmt.Errorf("encode resource section: %w", err)
		}
	}
	for _, address := range tree.relocs {
		relocation := pe.Reloc{VirtualAddress: address, SymbolTableIndex: 0, Type: 3} // IMAGE_REL_AMD64_ADDR32NB.
		if err := binary.Write(&object, binary.LittleEndian, relocation); err != nil {
			return nil, fmt.Errorf("encode resource relocation: %w", err)
		}
	}
	for _, value := range []any{
		pe.COFFSymbol{Name: name, SectionNumber: 1, StorageClass: 3},
		uint32(4), // Empty COFF string table, including its size field.
	} {
		if err := binary.Write(&object, binary.LittleEndian, value); err != nil {
			return nil, fmt.Errorf("encode resource symbol: %w", err)
		}
	}
	return object.Bytes(), nil
}
