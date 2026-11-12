package patcher

import (
	"bytes"
	"debug/pe"
	"encoding/binary"
	"errors"
	"fmt"
	"testing"
)

type mockProcess struct {
	memory    []byte
	writes    int
	failWrite int
	failRead  uintptr
	locked    bool
}

func (m *mockProcess) Read(addr uintptr, buf []byte) error {
	if addr == m.failRead || uint64(addr)+uint64(len(buf)) > uint64(len(m.memory)) {
		return fmt.Errorf("unreadable mock address")
	}
	copy(buf, m.memory[int(addr):int(addr)+len(buf)])
	return nil
}
func (m *mockProcess) ReadU32(addr uintptr) (uint32, error) {
	var b [4]byte
	err := m.Read(addr, b[:])
	return binary.LittleEndian.Uint32(b[:]), err
}
func (m *mockProcess) Write(addr uintptr, data []byte) error {
	m.writes++
	if !m.locked {
		return errors.New("write without lock")
	}
	if m.writes == m.failWrite {
		m.memory[addr] = data[0]
		return errors.New("injected partial write")
	}
	copy(m.memory[int(addr):int(addr)+len(data)], data)
	return nil
}
func (m *mockProcess) WriteCode(addr uintptr, data []byte) error { return m.Write(addr, data) }
func (m *mockProcess) Lock() error                               { m.locked = true; return nil }
func (m *mockProcess) Unlock() error                             { m.locked = false; return nil }
func (m *mockProcess) Close()                                    {}

func testPatcher() (*Patcher, *mockProcess) {
	m := &mockProcess{memory: make([]byte, 0x3000), failRead: ^uintptr(0)}
	disk := bytes.Repeat([]byte{0x90}, 0x200)
	copy(m.memory[0x1000:], disk)
	p := &Patcher{
		proc: m, disk: disk,
		peFile: &pe.File{OptionalHeader: &pe.OptionalHeader64{SizeOfImage: 0x3000}, Sections: []*pe.Section{
			{SectionHeader: pe.SectionHeader{VirtualAddress: 0x1000, VirtualSize: 0x200, Size: 0x200, Characteristics: pe.IMAGE_SCN_MEM_EXECUTE}},
			{SectionHeader: pe.SectionHeader{VirtualAddress: 0x2000, VirtualSize: 0x100, Characteristics: pe.IMAGE_SCN_MEM_WRITE}},
		}},
		spec: &Spec{
			LocalOnly:  &CodePatch{Name: "first", Offset: 0x1010, Code: []byte{0xEB}},
			SingleUser: &CodePatch{Name: "second", Offset: 0x1020, Code: []byte{0xB8, 1, 0, 0, 0}},
		},
		db: &DB{SLInitValues: map[string]uint32{}},
	}
	return p, m
}

func imageValidationPatcher(t *testing.T) (*Patcher, *mockProcess) {
	t.Helper()
	const peOffset = 0x80
	oh := pe.OptionalHeader64{
		Magic: 0x20B, ImageBase: 0x180000000, SizeOfImage: 0x3000, SizeOfHeaders: 0x400,
		SectionAlignment: 0x1000, FileAlignment: 0x200, NumberOfRvaAndSizes: 16,
	}
	fh := pe.FileHeader{
		Machine: pe.IMAGE_FILE_MACHINE_AMD64, NumberOfSections: 1,
		SizeOfOptionalHeader: uint16(binary.Size(oh)), TimeDateStamp: 0x12345678,
	}
	section := pe.SectionHeader32{
		Name: [8]byte{'.', 't', 'e', 'x', 't'}, VirtualSize: 0x1000, VirtualAddress: 0x1000,
		SizeOfRawData: 0x200, PointerToRawData: 0x400,
		Characteristics: pe.IMAGE_SCN_MEM_EXECUTE | pe.IMAGE_SCN_MEM_READ,
	}
	var headers bytes.Buffer
	headers.Write(make([]byte, peOffset))
	copy(headers.Bytes(), "MZ")
	binary.LittleEndian.PutUint32(headers.Bytes()[0x3c:], peOffset)
	headers.WriteString("PE\x00\x00")
	for _, value := range []any{fh, oh, section} {
		if err := binary.Write(&headers, binary.LittleEndian, value); err != nil {
			t.Fatal(err)
		}
	}
	disk := make([]byte, 0x600)
	copy(disk, headers.Bytes())
	file, err := pe.NewFile(bytes.NewReader(disk))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { file.Close() })
	m := &mockProcess{memory: make([]byte, 0x2000), failRead: ^uintptr(0)}
	copy(m.memory[0x1000:], disk)
	return &Patcher{proc: m, base: 0x1000, disk: disk, peFile: file}, m
}

func TestValidateImageAcceptsASLRWithoutChangingMemory(t *testing.T) {
	for _, relocated := range []bool{false, true} {
		t.Run(fmt.Sprintf("relocated=%v", relocated), func(t *testing.T) {
			p, m := imageValidationPatcher(t)
			imageBaseOffset := uintptr(binary.LittleEndian.Uint32(p.disk[0x3c:])) + 24 + 24
			if relocated {
				binary.LittleEndian.PutUint64(m.memory[p.base+imageBaseOffset:], uint64(p.base))
			}
			disk, memory := bytes.Clone(p.disk), bytes.Clone(m.memory)
			if err := p.validateImage(); err != nil {
				t.Fatalf("valid loaded image rejected: %v", err)
			}
			if m.writes != 0 || !bytes.Equal(disk, p.disk) || !bytes.Equal(memory, m.memory) {
				t.Fatal("header validation modified disk or target memory")
			}
		})
	}
}

func TestValidateImageStillRejectsUnexpectedHeaderChanges(t *testing.T) {
	for _, field := range []string{"ImageBase", "TimeDateStamp", "Section"} {
		t.Run(field, func(t *testing.T) {
			p, m := imageValidationPatcher(t)
			peOffset := uintptr(binary.LittleEndian.Uint32(p.disk[0x3c:]))
			imageBaseOffset := peOffset + 24 + 24
			binary.LittleEndian.PutUint64(m.memory[p.base+imageBaseOffset:], uint64(p.base))
			offset := imageBaseOffset
			switch field {
			case "TimeDateStamp":
				offset = peOffset + 8
			case "Section":
				offset = peOffset + 24 + uintptr(p.peFile.SizeOfOptionalHeader) + 8
			}
			m.memory[p.base+offset] ^= 1
			if err := p.validateImage(); err == nil {
				t.Fatal("unexpected header mutation was accepted")
			}
			if m.writes != 0 {
				t.Fatal("header rejection modified target memory")
			}
		})
	}
}

func TestApplyPreflightsEntireBatch(t *testing.T) {
	p, m := testPatcher()
	m.memory[0x1020] = 0xCC
	before := bytes.Clone(m.memory)
	if _, err := p.Apply(); err == nil {
		t.Fatal("expected validation failure")
	}
	if m.writes != 0 || !bytes.Equal(before, m.memory) {
		t.Fatal("preflight failure changed memory")
	}
}

func TestApplyRestoresPartialWrite(t *testing.T) {
	p, m := testPatcher()
	before := bytes.Clone(m.memory)
	m.failWrite = 2
	if _, err := p.Apply(); err == nil {
		t.Fatal("expected write failure")
	}
	if !bytes.Equal(before, m.memory) {
		t.Fatal("failed batch left modified bytes")
	}
	if m.locked {
		t.Fatal("writer lock leaked")
	}
}

func TestApplyIsIdempotent(t *testing.T) {
	p, m := testPatcher()
	if changed, err := p.Apply(); err != nil || !changed {
		t.Fatalf("first apply: %v %v", changed, err)
	}
	if active, err := p.Verify(); err != nil || !active {
		t.Fatalf("verify: %v %v", active, err)
	}
	writes := m.writes
	if changed, err := p.Apply(); err != nil || changed || m.writes != writes {
		t.Fatalf("second apply: %v %v writes=%d", changed, err, m.writes)
	}
}

func TestApplyPreflightsGlobalsBeforeCodeWrites(t *testing.T) {
	p, m := testPatcher()
	p.spec.SLInitGlobals = map[string]uint32{}
	for i, name := range slInitOrder {
		p.spec.SLInitGlobals[name] = uint32(0x2000 + 4*i)
		p.db.SLInitValues[name] = 1
	}
	binary.LittleEndian.PutUint32(m.memory[p.spec.SLInitGlobals["bInitialized"]:], 1)
	m.failRead = uintptr(p.spec.SLInitGlobals["bServerSku"])
	if _, err := p.Apply(); err == nil {
		t.Fatal("expected global read error")
	}
	if m.writes != 0 {
		t.Fatal("code changed before global validation")
	}
}

func TestDiskBytesRejectsVirtualTailAndCrossSection(t *testing.T) {
	p, _ := testPatcher()
	p.peFile.Sections[0].Size = 0x20
	for _, rva := range []uint32{0x1020, 0x11FF, 0x2000, 0xFFFFFFFE} {
		if _, err := p.diskBytes(rva, 4); err == nil {
			t.Errorf("accepted invalid range 0x%X", rva)
		}
	}
}

func TestValidateTargetsRejectsOverlapAndExecutableGlobals(t *testing.T) {
	p, _ := testPatcher()
	p.spec.SingleUser.Offset = p.spec.LocalOnly.Offset
	if err := p.validateTargets(); err == nil {
		t.Fatal("accepted overlapping code patches")
	}
	p, _ = testPatcher()
	p.spec.SLInitGlobals = map[string]uint32{"bInitialized": 0x1040}
	if err := p.validateTargets(); err == nil {
		t.Fatal("accepted executable global")
	}
}
