package patcher

import (
	"bytes"
	"debug/pe"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"os"
	"runtime"

	"rdpmux/internal/logx"
	"rdpmux/internal/winutil"
)

type processMemory interface {
	Read(uintptr, []byte) error
	ReadU32(uintptr) (uint32, error)
	Write(uintptr, []byte) error
	WriteCode(uintptr, []byte) error
	Lock() error
	Unlock() error
	Close()
}

type Patcher struct {
	proc    processMemory
	base    uintptr
	spec    *Spec
	db      *DB
	disk    []byte
	peFile  *pe.File
	Version string
	Log     *logx.Logger
}

func New(pid uint32, db *DB, arch string, log *logx.Logger) (*Patcher, error) {
	if arch != "x64" || runtime.GOARCH != "amd64" {
		return nil, fmt.Errorf("only native Windows x64 patching is supported")
	}
	if log == nil {
		log = logx.Discard
	}
	proc, err := OpenProcess(pid)
	if err != nil {
		return nil, err
	}
	log.Debug("%s", tr("OpenedProcess", "opened process %d", pid))
	base, path, err := FindTermsrv(pid)
	if err != nil {
		proc.Close()
		return nil, err
	}
	log.Debug("%s", tr("TermsrvBase", "termsrv.dll base=0x%X path=%s", base, path))
	file, err := os.Open(path)
	if err != nil {
		proc.Close()
		return nil, err
	}
	defer file.Close()
	fv, err := winutil.GetFileVersion(path)
	if err != nil {
		proc.Close()
		return nil, fmt.Errorf("termsrv version: %w", err)
	}
	version := fv.String()
	log.Debug("%s", tr("TermsrvVersion", "termsrv numeric version=%s", version))
	spec, ok := db.Spec(version)
	if !ok {
		proc.Close()
		if reason := db.Invalid[version]; reason != nil {
			return nil, fmt.Errorf("termsrv %s: invalid patch data: %w", version, reason)
		}
		return nil, fmt.Errorf("termsrv %s: no patch data (unsupported)", version)
	}
	disk, err := io.ReadAll(file)
	if err != nil {
		proc.Close()
		return nil, err
	}
	peFile, err := pe.NewFile(bytes.NewReader(disk))
	if err != nil {
		proc.Close()
		return nil, err
	}
	log.Debug("%s", tr("LoadedDisk", "loaded on-disk termsrv.dll (%d bytes) for validation", len(disk)))
	p := &Patcher{proc: proc, base: base, spec: spec, db: db, disk: disk, peFile: peFile, Version: version, Log: log}
	if err := p.validateImage(); err != nil {
		p.Close()
		return nil, err
	}
	if err := p.validateTargets(); err != nil {
		p.Close()
		return nil, err
	}
	return p, nil
}

func (p *Patcher) logger() *logx.Logger {
	if p.Log != nil {
		return p.Log
	}
	return logx.Discard
}

func (p *Patcher) Close() {
	p.peFile.Close()
	p.proc.Close()
}

func (p *Patcher) validateImage() error {
	oh, ok := p.peFile.OptionalHeader.(*pe.OptionalHeader64)
	if !ok || p.peFile.Machine != pe.IMAGE_FILE_MACHINE_AMD64 || oh.SizeOfImage == 0 {
		return fmt.Errorf("termsrv.dll is not an AMD64 PE image")
	}
	if len(p.disk) < 64 {
		return fmt.Errorf("truncated PE header")
	}
	peOffset := uint64(binary.LittleEndian.Uint32(p.disk[0x3c:]))
	imageBaseOffset := peOffset + 24 + 24
	size := peOffset + 24 + uint64(p.peFile.SizeOfOptionalHeader) + uint64(p.peFile.NumberOfSections)*40
	if imageBaseOffset+8 > size || size > uint64(len(p.disk)) || size > uint64(oh.SizeOfHeaders) || size > uint64(oh.SizeOfImage) {
		return fmt.Errorf("invalid PE header range")
	}
	header := make([]byte, int(size))
	if err := p.proc.Read(p.base, header); err != nil {
		return err
	}
	loadedBase := binary.LittleEndian.Uint64(header[imageBaseOffset:])
	if loadedBase != oh.ImageBase && loadedBase != uint64(p.base) {
		return fmt.Errorf("loaded termsrv.dll has an unexpected image base")
	}
	// ASLR can rewrite ImageBase; all other header bytes must remain identical.
	binary.LittleEndian.PutUint64(header[imageBaseOffset:], oh.ImageBase)
	if !bytes.Equal(header, p.disk[:size]) {
		return fmt.Errorf("loaded termsrv.dll headers differ from disk; restart TermService before patching")
	}
	return nil
}

func (p *Patcher) section(rva uint32, size int, flags uint32) (*pe.Section, error) {
	if size <= 0 {
		return nil, fmt.Errorf("empty memory range")
	}
	end := uint64(rva) + uint64(size)
	oh, ok := p.peFile.OptionalHeader.(*pe.OptionalHeader64)
	if !ok || end > uint64(oh.SizeOfImage) {
		return nil, fmt.Errorf("RVA 0x%X outside image", rva)
	}
	for _, s := range p.peFile.Sections {
		if uint64(rva) >= uint64(s.VirtualAddress) && end <= uint64(s.VirtualAddress)+uint64(s.VirtualSize) {
			if s.Characteristics&flags != flags {
				return nil, fmt.Errorf("RVA 0x%X has unexpected section permissions", rva)
			}
			return s, nil
		}
	}
	return nil, fmt.Errorf("RVA 0x%X is not contained in a section", rva)
}

func (p *Patcher) diskBytes(rva uint32, size int) ([]byte, error) {
	s, err := p.section(rva, size, pe.IMAGE_SCN_MEM_EXECUTE)
	if err != nil {
		return nil, err
	}
	rel := uint64(rva) - uint64(s.VirtualAddress)
	off := uint64(s.Offset) + rel
	if rel+uint64(size) > uint64(s.Size) || off+uint64(size) > uint64(len(p.disk)) {
		return nil, fmt.Errorf("RVA 0x%X outside section file data", rva)
	}
	return p.disk[off : off+uint64(size)], nil
}

func (p *Patcher) validateTargets() error {
	type span struct{ start, end uint64 }
	var spans []span
	add := func(rva uint32, size int) error {
		s := span{uint64(rva), uint64(rva) + uint64(size)}
		for _, other := range spans {
			if s.start < other.end && other.start < s.end {
				return fmt.Errorf("overlapping patch ranges at RVA 0x%X", rva)
			}
		}
		spans = append(spans, s)
		return nil
	}
	for _, cp := range []*CodePatch{p.spec.LocalOnly, p.spec.SingleUser, p.spec.DefPolicy} {
		if cp == nil {
			continue
		}
		if _, err := p.diskBytes(cp.Offset, len(cp.Code)); err != nil {
			return fmt.Errorf("%s: %w", cp.Name, err)
		}
		if err := add(cp.Offset, len(cp.Code)); err != nil {
			return err
		}
	}
	for name, rva := range p.spec.SLInitGlobals {
		if rva%4 != 0 {
			return fmt.Errorf("SLInit %s: unaligned RVA", name)
		}
		s, err := p.section(rva, 4, pe.IMAGE_SCN_MEM_WRITE)
		if err != nil {
			return fmt.Errorf("SLInit %s: %w", name, err)
		}
		if s.Characteristics&pe.IMAGE_SCN_MEM_EXECUTE != 0 {
			return fmt.Errorf("SLInit %s points into executable memory", name)
		}
		if err := add(rva, 4); err != nil {
			return err
		}
	}
	return nil
}

type memoryWrite struct {
	name          string
	addr          uintptr
	before, after []byte
	code          bool
}

func (p *Patcher) planCode() ([]memoryWrite, error) {
	var writes []memoryWrite
	for _, cp := range []*CodePatch{p.spec.LocalOnly, p.spec.SingleUser, p.spec.DefPolicy} {
		if cp == nil {
			continue
		}
		orig, err := p.diskBytes(cp.Offset, len(cp.Code))
		if err != nil {
			return nil, err
		}
		mem := make([]byte, len(cp.Code))
		addr := p.base + uintptr(cp.Offset)
		if err := p.proc.Read(addr, mem); err != nil {
			return nil, err
		}
		if bytes.Equal(mem, cp.Code) {
			continue
		}
		if !bytes.Equal(mem, orig) {
			return nil, fmt.Errorf("%s site 0x%X: memory differs from on-disk termsrv.dll, refusing to patch", cp.Name, cp.Offset)
		}
		p.logger().Debug("%s rva=0x%X mem=% X patch=% X", cp.Name, cp.Offset, mem, cp.Code)
		writes = append(writes, memoryWrite{cp.Name, addr, mem, cp.Code, true})
	}
	return writes, nil
}

func (p *Patcher) write(w memoryWrite, data []byte) error {
	if w.code {
		return p.proc.WriteCode(w.addr, data)
	}
	return p.proc.Write(w.addr, data)
}

func (p *Patcher) Apply() (changed bool, err error) {
	if err = p.proc.Lock(); err != nil {
		return false, err
	}
	defer func() { err = errors.Join(err, p.proc.Unlock()) }()
	writes, err := p.planCode()
	if err != nil {
		return false, err
	}
	globals, err := p.planSLInit()
	if err != nil {
		return false, err
	}
	writes = append(writes, globals...)
	for i, w := range writes {
		err = p.write(w, w.after)
		if err == nil {
			got := make([]byte, len(w.after))
			err = p.proc.Read(w.addr, got)
			if err == nil && !bytes.Equal(got, w.after) {
				err = fmt.Errorf("write verification failed")
			}
		}
		if err != nil {
			failure := fmt.Errorf("%s: %w", w.name, err)
			for j := i; j >= 0; j-- {
				if restoreErr := p.write(writes[j], writes[j].before); restoreErr != nil {
					failure = errors.Join(failure, fmt.Errorf("restore %s: %w", writes[j].name, restoreErr))
				}
			}
			return false, failure
		}
		p.logger().Info("%s applied at 0x%X", w.name, w.addr)
	}
	return len(writes) > 0, nil
}

var slInitOrder = []string{
	"bServerSku", "bRemoteConnAllowed", "bFUSEnabled", "bAppServerAllowed",
	"bMultimonAllowed", "lMaxUserSessions", "ulMaxDebugSessions", "bInitialized",
}

func (p *Patcher) planSLInit() ([]memoryWrite, error) {
	if p.spec.SLInitGlobals == nil {
		return nil, nil
	}
	initialized, err := p.proc.ReadU32(p.base + uintptr(p.spec.SLInitGlobals["bInitialized"]))
	if err != nil {
		return nil, err
	}
	if initialized == 0 {
		p.logger().Debug("%s", tr("SLInitNotReady", "SLInit not ready: bInitialized=0 (CSLQuery::Initialize pending)"))
		return nil, nil
	}
	if initialized != 1 {
		return nil, fmt.Errorf("unexpected SLInit initialization state: %d", initialized)
	}
	var writes []memoryWrite
	for _, name := range slInitOrder {
		rva := p.spec.SLInitGlobals[name]
		addr := p.base + uintptr(rva)
		before := make([]byte, 4)
		if err := p.proc.Read(addr, before); err != nil {
			return nil, err
		}
		after := make([]byte, 4)
		binary.LittleEndian.PutUint32(after, p.db.SLInitValues[name])
		if !bytes.Equal(before, after) {
			p.logger().Debug("SLInit %s rva=0x%X %d -> %d", name, rva, binary.LittleEndian.Uint32(before), p.db.SLInitValues[name])
			writes = append(writes, memoryWrite{name, addr, before, after, false})
		}
	}
	return writes, nil
}

func (p *Patcher) Verify() (bool, error) {
	log := p.logger()
	for _, cp := range []*CodePatch{p.spec.LocalOnly, p.spec.SingleUser, p.spec.DefPolicy} {
		if cp == nil {
			continue
		}
		mem := make([]byte, len(cp.Code))
		if err := p.proc.Read(p.base+uintptr(cp.Offset), mem); err != nil {
			return false, err
		}
		if !bytes.Equal(mem, cp.Code) {
			log.Debug("%s", tr("VerifyNotPatched", "verify: %s rva=0x%X not patched (mem=% X)", cp.Name, cp.Offset, mem))
			return false, nil
		}
	}
	for name, rva := range p.spec.SLInitGlobals {
		got, err := p.proc.ReadU32(p.base + uintptr(rva))
		if err != nil {
			return false, err
		}
		if got != p.db.SLInitValues[name] {
			log.Debug("%s", tr("VerifySLInit", "verify: SLInit %s rva=0x%X = %d, want %d", name, rva, got, p.db.SLInitValues[name]))
			return false, nil
		}
	}
	log.Trace("%s", tr("VerifyOK", "verify: all patches active"))
	return true, nil
}
