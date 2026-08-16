// Command genrsrc turns a Windows application manifest into a COFF object that
// the Go linker embeds as an RT_MANIFEST resource.
//
// A manifest is not cosmetic here: walk sends TTM_ADDTOOL using the comctl32
// version 6 TOOLINFO layout, so without the Common-Controls 6.0 dependency
// Windows loads comctl32 version 5, the message is rejected and both GUIs panic
// before their main window appears.
//
// This is written in pure Go on purpose. Requiring windres or a MinGW toolchain
// just to produce a few hundred bytes of resource data would make the release
// build depend on tools that are not part of the Go installation.
package main

import (
	"bytes"
	"encoding/binary"
	"encoding/xml"
	"flag"
	"fmt"
	"io"
	"os"
	"path/filepath"
)

const (
	imageFileMachineAMD64 = 0x8664
	// IMAGE_SCN_MEM_READ | IMAGE_SCN_ALIGN_8BYTES | IMAGE_SCN_CNT_INITIALIZED_DATA
	sectionCharacteristics = 0x40000040
	// IMAGE_REL_AMD64_ADDR32NB: the RVA of the resource bytes, relative to the
	// image base, which is what IMAGE_RESOURCE_DATA_ENTRY.OffsetToData needs.
	relocationAddr32NB = 0x0003
	symbolClassStatic  = 3

	// CREATEPROCESS_MANIFEST_RESOURCE_ID and RT_MANIFEST.
	manifestResourceID = 1
	manifestType       = 24
	languageNeutral    = 0x0409

	directorySize = 16
	entrySize     = 8
	dataEntrySize = 16
)

func main() {
	input := flag.String("manifest", "", "path to the application manifest")
	output := flag.String("out", "", "path of the .syso object to write")
	flag.Parse()
	if *input == "" || *output == "" {
		fmt.Fprintln(os.Stderr, "usage: genrsrc -manifest <file.manifest> -out <file.syso>")
		os.Exit(2)
	}
	manifest, err := os.ReadFile(*input)
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
	object, err := buildObject(manifest)
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
	if err := writeAtomic(*output, object); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
	fmt.Printf("MANIFEST=%s\nSYSO=%s\nBYTES=%d\n", filepath.Clean(*input), filepath.Clean(*output), len(object))
}

// buildObject lays out a single-resource .rsrc section:
//
//	 0  type directory        -> one entry, RT_MANIFEST
//	24  name directory        -> one entry, resource id 1
//	48  language directory    -> one entry, language 0x0409
//	72  IMAGE_RESOURCE_DATA_ENTRY (its OffsetToData is relocated)
//	88  the manifest bytes
func buildObject(manifest []byte) ([]byte, error) {
	if len(manifest) == 0 {
		return nil, fmt.Errorf("manifest is empty")
	}
	// A malformed manifest still links, but Windows then refuses to start the
	// binary with an opaque "side-by-side configuration is incorrect" error.
	// Parsing here turns that into a build failure that names the problem. It
	// also catches the easy trap of a double hyphen inside an XML comment.
	if err := validateXML(manifest); err != nil {
		return nil, fmt.Errorf("manifest is not well-formed XML: %w", err)
	}
	const (
		typeDirectory     = 0
		nameDirectory     = typeDirectory + directorySize + entrySize
		languageDirectory = nameDirectory + directorySize + entrySize
		dataEntryOffset   = languageDirectory + directorySize + entrySize
		manifestOffset    = dataEntryOffset + dataEntrySize
	)
	section := new(bytes.Buffer)
	writeDirectory := func(id uint32, target uint32, isSubdirectory bool) {
		// IMAGE_RESOURCE_DIRECTORY: no named entries, exactly one id entry.
		binary.Write(section, binary.LittleEndian, uint32(0)) // Characteristics
		binary.Write(section, binary.LittleEndian, uint32(0)) // TimeDateStamp
		binary.Write(section, binary.LittleEndian, uint16(0)) // MajorVersion
		binary.Write(section, binary.LittleEndian, uint16(0)) // MinorVersion
		binary.Write(section, binary.LittleEndian, uint16(0)) // NumberOfNamedEntries
		binary.Write(section, binary.LittleEndian, uint16(1)) // NumberOfIdEntries
		if isSubdirectory {
			target |= 0x80000000
		}
		binary.Write(section, binary.LittleEndian, id)
		binary.Write(section, binary.LittleEndian, target)
	}
	writeDirectory(manifestType, nameDirectory, true)
	writeDirectory(manifestResourceID, languageDirectory, true)
	writeDirectory(languageNeutral, dataEntryOffset, false)

	// IMAGE_RESOURCE_DATA_ENTRY. OffsetToData is written as the section-relative
	// offset and turned into an RVA by the relocation below.
	binary.Write(section, binary.LittleEndian, uint32(manifestOffset))
	binary.Write(section, binary.LittleEndian, uint32(len(manifest)))
	binary.Write(section, binary.LittleEndian, uint32(0)) // CodePage
	binary.Write(section, binary.LittleEndian, uint32(0)) // Reserved
	if section.Len() != manifestOffset {
		return nil, fmt.Errorf("resource header is %d bytes, expected %d", section.Len(), manifestOffset)
	}
	section.Write(manifest)
	for section.Len()%8 != 0 {
		section.WriteByte(0)
	}
	sectionBytes := section.Bytes()

	const (
		fileHeaderSize    = 20
		sectionHeaderSize = 40
		relocationSize    = 10
		symbolSize        = 18
	)
	rawDataPointer := uint32(fileHeaderSize + sectionHeaderSize)
	relocationPointer := rawDataPointer + uint32(len(sectionBytes))
	symbolPointer := relocationPointer + relocationSize

	object := new(bytes.Buffer)
	// IMAGE_FILE_HEADER
	binary.Write(object, binary.LittleEndian, uint16(imageFileMachineAMD64))
	binary.Write(object, binary.LittleEndian, uint16(1)) // NumberOfSections
	binary.Write(object, binary.LittleEndian, uint32(0)) // TimeDateStamp, kept zero for reproducibility
	binary.Write(object, binary.LittleEndian, symbolPointer)
	binary.Write(object, binary.LittleEndian, uint32(1)) // NumberOfSymbols
	binary.Write(object, binary.LittleEndian, uint16(0)) // SizeOfOptionalHeader
	binary.Write(object, binary.LittleEndian, uint16(0)) // Characteristics

	// IMAGE_SECTION_HEADER
	var name [8]byte
	copy(name[:], ".rsrc")
	object.Write(name[:])
	binary.Write(object, binary.LittleEndian, uint32(0)) // VirtualSize
	binary.Write(object, binary.LittleEndian, uint32(0)) // VirtualAddress
	binary.Write(object, binary.LittleEndian, uint32(len(sectionBytes)))
	binary.Write(object, binary.LittleEndian, rawDataPointer)
	binary.Write(object, binary.LittleEndian, relocationPointer)
	binary.Write(object, binary.LittleEndian, uint32(0)) // PointerToLinenumbers
	binary.Write(object, binary.LittleEndian, uint16(1)) // NumberOfRelocations
	binary.Write(object, binary.LittleEndian, uint16(0)) // NumberOfLinenumbers
	binary.Write(object, binary.LittleEndian, uint32(sectionCharacteristics))

	object.Write(sectionBytes)

	// IMAGE_RELOCATION for IMAGE_RESOURCE_DATA_ENTRY.OffsetToData.
	binary.Write(object, binary.LittleEndian, uint32(dataEntryOffset))
	binary.Write(object, binary.LittleEndian, uint32(0)) // SymbolTableIndex -> the .rsrc section symbol
	binary.Write(object, binary.LittleEndian, uint16(relocationAddr32NB))

	// IMAGE_SYMBOL for the section itself.
	object.Write(name[:])
	binary.Write(object, binary.LittleEndian, uint32(0)) // Value
	binary.Write(object, binary.LittleEndian, int16(1))  // SectionNumber
	binary.Write(object, binary.LittleEndian, uint16(0)) // Type
	object.WriteByte(symbolClassStatic)
	object.WriteByte(0) // NumberOfAuxSymbols

	// An empty string table still needs its four byte length.
	binary.Write(object, binary.LittleEndian, uint32(4))
	return object.Bytes(), nil
}

func validateXML(manifest []byte) error {
	decoder := xml.NewDecoder(bytes.NewReader(manifest))
	decoder.Strict = true
	for {
		_, err := decoder.Token()
		if err == io.EOF {
			return nil
		}
		if err != nil {
			return err
		}
	}
}

func writeAtomic(path string, content []byte) error {
	temp := path + ".tmp"
	if err := os.WriteFile(temp, content, 0o644); err != nil {
		return err
	}
	if err := os.Rename(temp, path); err != nil {
		_ = os.Remove(temp)
		return err
	}
	return nil
}
