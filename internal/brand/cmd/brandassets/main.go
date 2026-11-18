package main

import (
	"bytes"
	"encoding/binary"
	"flag"
	"fmt"
	"os"
	"path/filepath"

	"remote-enet/internal/brand"
)

func main() {
	output := flag.String("output", "", "directory for generated brand assets (required)")
	resource := flag.String("windows-resource", "", "output path for the Windows amd64 COFF icon resource")
	flag.Parse()
	if *output == "" || flag.NArg() != 0 {
		flag.Usage()
		os.Exit(2)
	}
	if err := export(*output); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
	if *resource != "" {
		if err := exportWindowsResource(*output, *resource); err != nil {
			fmt.Fprintln(os.Stderr, err)
			os.Exit(1)
		}
	}
}

func exportWindowsResource(dir, output string) error {
	data, err := os.ReadFile(filepath.Join(dir, "app-icon.ico"))
	if err != nil {
		return fmt.Errorf("read icon resource input: %w", err)
	}
	object, err := windowsResource(data)
	if err != nil {
		return fmt.Errorf("generate icon resource: %w", err)
	}
	if err := os.WriteFile(output, object, 0644); err != nil {
		return fmt.Errorf("write icon resource: %w", err)
	}
	return nil
}

func export(dir string) error {
	if err := os.MkdirAll(dir, 0755); err != nil {
		return err
	}
	files := map[string][]byte{"logo.svg": brand.SVG(), "logo.png": brand.PNG(1024, false, false), "app-icon.png": brand.PNG(1024, true, false), "tray.png": brand.PNG(36, false, true)}
	for _, size := range []int{16, 20, 32} {
		files[fmt.Sprintf("icon-%d.png", size)] = brand.PNG(size, false, false)
		files[fmt.Sprintf("template-%d.png", size)] = brand.PNG(size, false, true)
	}
	sizes := []int{16, 20, 32, 48, 64, 128, 256}
	var ico bytes.Buffer
	binary.Write(&ico, binary.LittleEndian, [3]uint16{0, 1, uint16(len(sizes))})
	offset := uint32(6 + 16*len(sizes))
	var pictures [][]byte
	for _, size := range sizes {
		picture := brand.PNG(size, true, false)
		pictures = append(pictures, picture)
		ico.Write([]byte{byte(size), byte(size), 0, 0})
		binary.Write(&ico, binary.LittleEndian, uint16(1))
		binary.Write(&ico, binary.LittleEndian, uint16(32))
		binary.Write(&ico, binary.LittleEndian, uint32(len(picture)))
		binary.Write(&ico, binary.LittleEndian, offset)
		offset += uint32(len(picture))
	}
	for _, picture := range pictures {
		ico.Write(picture)
	}
	files["app-icon.ico"] = ico.Bytes()
	var blocks bytes.Buffer
	for _, item := range []struct {
		tag  string
		size int
	}{{"icp4", 16}, {"icp5", 32}, {"icp6", 64}, {"ic07", 128}, {"ic08", 256}, {"ic09", 512}, {"ic10", 1024}} {
		picture := brand.PNG(item.size, true, false)
		blocks.WriteString(item.tag)
		binary.Write(&blocks, binary.BigEndian, uint32(8+len(picture)))
		blocks.Write(picture)
	}
	var icns bytes.Buffer
	icns.WriteString("icns")
	binary.Write(&icns, binary.BigEndian, uint32(8+blocks.Len()))
	icns.Write(blocks.Bytes())
	files["app-icon.icns"] = icns.Bytes()
	for name, data := range files {
		if err := os.WriteFile(filepath.Join(dir, name), data, 0644); err != nil {
			return err
		}
	}
	return nil
}
