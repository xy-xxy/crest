package main

import (
	"bytes"
	"debug/pe"
	"encoding/binary"
	"image"
	"image/png"
	"testing"
)

func TestBuildSYSOArchitectures(t *testing.T) {
	sizes := []int{16, 256}
	var pngs [][]byte
	for _, size := range sizes {
		var data bytes.Buffer
		if err := png.Encode(&data, image.NewNRGBA(image.Rect(0, 0, size, size))); err != nil {
			t.Fatal(err)
		}
		pngs = append(pngs, data.Bytes())
	}
	wantBlobs := append(append([][]byte(nil), pngs...), buildGroupIcon(sizes, pngs))
	for _, tc := range []struct {
		arch       string
		machine    uint16
		relocation uint16
	}{
		{"amd64", pe.IMAGE_FILE_MACHINE_AMD64, 0x0003},
		{"386", pe.IMAGE_FILE_MACHINE_I386, 0x0007},
	} {
		t.Run(tc.arch, func(t *testing.T) {
			object, err := buildSYSO(sizes, pngs, tc.arch)
			if err != nil {
				t.Fatal(err)
			}
			file, err := pe.NewFile(bytes.NewReader(object))
			if err != nil {
				t.Fatal(err)
			}
			defer file.Close()
			if file.Machine != tc.machine {
				t.Fatalf("machine = %#x, want %#x", file.Machine, tc.machine)
			}
			section := file.Section(".rsrc")
			if section == nil || len(section.Relocs) != len(wantBlobs) {
				t.Fatal("missing icon resource section or relocations")
			}
			data, err := section.Data()
			if err != nil {
				t.Fatal(err)
			}
			for i, relocation := range section.Relocs {
				if relocation.Type != tc.relocation || relocation.SymbolTableIndex != 0 {
					t.Fatalf("relocation %d = %+v", i, relocation)
				}
				entry := relocation.VirtualAddress
				offset := binary.LittleEndian.Uint32(data[entry:])
				size := binary.LittleEndian.Uint32(data[entry+4:])
				if !bytes.Equal(data[offset:offset+size], wantBlobs[i]) {
					t.Fatalf("resource %d does not point to the expected icon data", i)
				}
			}
		})
	}
}

func TestBuildSYSORejectsUnsupportedArchitecture(t *testing.T) {
	if _, err := buildSYSO(nil, nil, "arm64"); err == nil {
		t.Fatal("unsupported architecture accepted")
	}
}
