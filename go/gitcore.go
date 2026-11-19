// The git object and packfile layer: SHA-1 object ids, commit/tree/blob/tag
// encoding, packfile read (plain + OFS/REF deltas, checksum verified) and write
// (plain objects, version 2), and pkt-line framing. Mirrors src/gitcore.ts.
package main

import (
	"bytes"
	"compress/flate"
	"compress/zlib"
	"encoding/hex"
	"fmt"
	"io"
	"sort"
	"strconv"
	"strings"
)

type ObjType string

const (
	ObjCommit ObjType = "commit"
	ObjTree   ObjType = "tree"
	ObjBlob   ObjType = "blob"
	ObjTag    ObjType = "tag"
)

type GitObject struct {
	Type ObjType
	Data []byte
}

func hashObject(typ ObjType, data []byte) string {
	header := fmt.Sprintf("%s %d\x00", string(typ), len(data))
	return sha1Hex([]byte(header), data)
}

func objectHeader(typ ObjType, size int) []byte {
	return []byte(fmt.Sprintf("%s %d\x00", string(typ), size))
}

// ---------------------------------------------------------------- commits

type ParsedCommit struct {
	Tree      string
	Parents   []string
	Author    string
	Committer string
	Message   string
}

func parseCommit(data []byte) ParsedCommit {
	text := string(data)
	headEnd := strings.Index(text, "\n\n")
	head, message := text, ""
	if headEnd >= 0 {
		head = text[:headEnd]
		message = text[headEnd+2:]
	}
	pc := ParsedCommit{}
	for _, line := range strings.Split(head, "\n") {
		switch {
		case strings.HasPrefix(line, "tree "):
			pc.Tree = strings.TrimSpace(line[5:])
		case strings.HasPrefix(line, "parent "):
			pc.Parents = append(pc.Parents, strings.TrimSpace(line[7:]))
		case strings.HasPrefix(line, "author "):
			pc.Author = line[7:]
		case strings.HasPrefix(line, "committer "):
			pc.Committer = line[10:]
		}
	}
	pc.Message = message
	return pc
}

type CommitInput struct {
	Tree      string
	Parents   []string
	Author    string
	Committer string
	Message   string
}

func buildCommit(in CommitInput) []byte {
	var b strings.Builder
	b.WriteString("tree " + in.Tree + "\n")
	for _, p := range in.Parents {
		b.WriteString("parent " + p + "\n")
	}
	b.WriteString("author " + in.Author + "\n")
	b.WriteString("committer " + in.Committer + "\n")
	b.WriteString("\n" + in.Message)
	return []byte(b.String())
}

// ---------------------------------------------------------------- trees

type TreeEntry struct {
	Mode string
	Name string
	OID  string
}

func parseTree(data []byte) []TreeEntry {
	var entries []TreeEntry
	offset := 0
	for offset < len(data) {
		space := bytes.IndexByte(data[offset:], 0x20)
		if space < 0 {
			break
		}
		space += offset
		nul := bytes.IndexByte(data[space+1:], 0x00)
		if nul < 0 {
			break
		}
		nul += space + 1
		if nul+21 > len(data) {
			break
		}
		mode := string(data[offset:space])
		name := string(data[space+1 : nul])
		oid := hex.EncodeToString(data[nul+1 : nul+21])
		entries = append(entries, TreeEntry{Mode: mode, Name: name, OID: oid})
		offset = nul + 21
	}
	return entries
}

func treeCompare(a, b TreeEntry) int {
	aName, bName := a.Name, b.Name
	if a.Mode == "40000" {
		aName += "/"
	}
	if b.Mode == "40000" {
		bName += "/"
	}
	if aName < bName {
		return -1
	}
	if aName > bName {
		return 1
	}
	return 0
}

func buildTree(entries []TreeEntry) []byte {
	sorted := make([]TreeEntry, len(entries))
	copy(sorted, entries)
	sort.Slice(sorted, func(i, j int) bool { return treeCompare(sorted[i], sorted[j]) < 0 })
	var buf bytes.Buffer
	for _, e := range sorted {
		buf.WriteString(e.Mode + " " + e.Name + "\x00")
		raw, _ := hex.DecodeString(e.OID)
		buf.Write(raw)
	}
	return buf.Bytes()
}

// ---------------------------------------------------------------- tags

type ParsedTag struct {
	Object  string
	Type    string
	Tag     string
	Tagger  string
	Message string
}

func parseTag(data []byte) ParsedTag {
	text := string(data)
	headEnd := strings.Index(text, "\n\n")
	head, message := text, ""
	if headEnd >= 0 {
		head = text[:headEnd]
		message = text[headEnd+2:]
	}
	pt := ParsedTag{}
	for _, line := range strings.Split(head, "\n") {
		switch {
		case strings.HasPrefix(line, "object "):
			pt.Object = strings.TrimSpace(line[7:])
		case strings.HasPrefix(line, "type "):
			pt.Type = strings.TrimSpace(line[5:])
		case strings.HasPrefix(line, "tag "):
			pt.Tag = strings.TrimSpace(line[4:])
		case strings.HasPrefix(line, "tagger "):
			pt.Tagger = line[7:]
		}
	}
	pt.Message = message
	return pt
}

// ---------------------------------------------------------------- packfiles

type PackError struct{ Msg string }

func (e *PackError) Error() string { return e.Msg }

func packErr(format string, args ...interface{}) *PackError {
	return &PackError{Msg: fmt.Sprintf(format, args...)}
}

var packTypeCodes = map[int]string{
	1: "commit", 2: "tree", 3: "blob", 4: "tag", 6: "ofs-delta", 7: "ref-delta",
}
var packTypeIDs = map[ObjType]int{ObjCommit: 1, ObjTree: 2, ObjBlob: 3, ObjTag: 4}

// inflateAt inflates one zlib stream at offset; returns bytes and next offset.
func inflateAt(buf []byte, offset int) ([]byte, int, error) {
	r, err := zlib.NewReader(bytes.NewReader(buf[offset:]))
	if err != nil {
		return nil, 0, packErr("zlib stream at offset %d did not inflate: %s", offset, err.Error())
	}
	defer r.Close()
	data, err := io.ReadAll(r)
	if err != nil {
		return nil, 0, packErr("zlib stream at offset %d did not inflate: %s", offset, err.Error())
	}
	if len(data) == 0 {
		return nil, 0, packErr("zlib stream at offset %d consumed no bytes", offset)
	}
	// bytes consumed: zlib reader does not report it directly; measure by
	// re-inflating with a counting reader is wasteful. Instead compute the
	// consumed length from the compressed stream: use zlib.NewReader on a
	// reader that tracks position via a custom reader is not available, so
	// we find the stream end by scanning: the deflate stream's end is where
	// the next object header would start; instead use flate's Resetter trick:
	// fall back to incremental inflate via compress/flate raw counting.
	consumed, err := zlibConsumed(buf[offset:])
	if err != nil {
		return nil, 0, packErr("zlib stream at offset %d did not inflate: %s", offset, err.Error())
	}
	return data, offset + consumed, nil
}

// zlibConsumed returns how many input bytes one zlib stream consumes.
func zlibConsumed(input []byte) (int, error) {
	// Use compress/flate with a counting reader: zlib format = 2-byte header
	// + deflate stream + 4-byte adler32. We inflate the deflate part with
	// flate.NewReader and count bytes read from the underlying reader.
	cr := &countReader{r: bytes.NewReader(input)}
	// skip 2-byte zlib header
	hdr := make([]byte, 2)
	if _, err := io.ReadFull(cr, hdr); err != nil {
		return 0, err
	}
	fr := flate.NewReader(cr)
	data, err := io.ReadAll(fr)
	_ = data
	if err != nil {
		return 0, err
	}
	fr.Close()
	// +4 adler32 bytes
	return cr.n + 4, nil
}

type countReader struct {
	r *bytes.Reader
	n int
}

func (c *countReader) Read(p []byte) (int, error) {
	n, err := c.r.Read(p)
	c.n += n
	return n, err
}

// ReadByte makes countReader an io.ByteReader so flate reads exactly the
// bytes of one deflate stream and never over-reads into the next object.
func (c *countReader) ReadByte() (byte, error) {
	b, err := c.r.ReadByte()
	if err == nil {
		c.n++
	}
	return b, err
}

type PackReadResult struct {
	Objects []GitObject
	ByOID   map[string]GitObject
}

func readPack(pack []byte, resolveBase func(oid string) *GitObject) (*PackReadResult, error) {
	buf := pack
	if len(buf) < 12+20 {
		return nil, packErr("packfile is too short for a header and trailer")
	}
	if string(buf[0:4]) != "PACK" {
		return nil, packErr("packfile does not start with PACK")
	}
	version := be32(buf, 4)
	if version != 2 && version != 3 {
		return nil, packErr("packfile version %d is not 2 or 3", version)
	}
	count := be32(buf, 8)
	trailerStart := len(buf) - 20
	trailer := hex.EncodeToString(buf[trailerStart:])
	if sha1Hex(buf[:trailerStart]) != trailer {
		return nil, packErr("packfile trailer checksum does not match")
	}
	objects := []GitObject{}
	byOID := map[string]GitObject{}
	byOffset := map[int]GitObject{}
	offset := 12
	for index := 0; index < int(count); index++ {
		entryOffset := offset
		if offset >= trailerStart {
			return nil, packErr("packfile ends before its declared entries")
		}
		b := buf[offset]
		offset++
		typeCode := int((b >> 4) & 0x7)
		size := int(b & 0x0f)
		shift := uint(4)
		for (b & 0x80) != 0 {
			if offset >= trailerStart {
				return nil, packErr("packfile ends inside an entry header")
			}
			b = buf[offset]
			offset++
			size |= int(b&0x7f) << shift
			shift += 7
		}
		kind, ok := packTypeCodes[typeCode]
		if !ok {
			return nil, packErr("packfile entry type %d is unknown", typeCode)
		}
		var base *GitObject
		if kind == "ofs-delta" {
			if offset >= trailerStart {
				return nil, packErr("packfile ends inside an ofs-delta offset")
			}
			b = buf[offset]
			offset++
			baseOffset := int(b & 0x7f)
			for (b & 0x80) != 0 {
				if offset >= trailerStart {
					return nil, packErr("packfile ends inside an ofs-delta offset")
				}
				b = buf[offset]
				offset++
				baseOffset = ((baseOffset + 1) << 7) | int(b&0x7f)
			}
			at := entryOffset - baseOffset
			if bo, ok := byOffset[at]; ok {
				bc := bo
				base = &bc
			}
			if base == nil {
				return nil, packErr("ofs-delta base at offset %d is not in the pack", at)
			}
		} else if kind == "ref-delta" {
			if offset+20 > trailerStart {
				return nil, packErr("packfile ends inside a ref-delta base")
			}
			baseOID := hex.EncodeToString(buf[offset : offset+20])
			offset += 20
			if bo, ok := byOID[baseOID]; ok {
				bc := bo
				base = &bc
			} else if resolveBase != nil {
				base = resolveBase(baseOID)
			}
			if base == nil {
				return nil, packErr("ref-delta base %s is not in the pack or the store", baseOID)
			}
		}
		inflated, next, err := inflateAt(buf[:trailerStart], offset)
		if err != nil {
			return nil, err
		}
		if len(inflated) != size {
			return nil, packErr("pack entry at %d declares %d bytes but inflated to %d", entryOffset, size, len(inflated))
		}
		offset = next
		var object GitObject
		if kind == "ofs-delta" || kind == "ref-delta" {
			if base == nil {
				return nil, packErr("delta entry has no base")
			}
			data, err := applyDelta(base.Data, inflated)
			if err != nil {
				return nil, err
			}
			object = GitObject{Type: base.Type, Data: data}
		} else {
			object = GitObject{Type: ObjType(kind), Data: inflated}
		}
		oid := hashObject(object.Type, object.Data)
		objects = append(objects, object)
		byOID[oid] = object
		byOffset[entryOffset] = object
	}
	if offset != trailerStart {
		return nil, packErr("packfile has trailing bytes before the trailer")
	}
	return &PackReadResult{Objects: objects, ByOID: byOID}, nil
}

func be32(b []byte, off int) uint32 {
	return uint32(b[off])<<24 | uint32(b[off+1])<<16 | uint32(b[off+2])<<8 | uint32(b[off+3])
}

func applyDelta(base, delta []byte) ([]byte, error) {
	pos := 0
	readVarint := func() (int, error) {
		value := 0
		shift := uint(0)
		for {
			if pos >= len(delta) {
				return 0, packErr("delta ends inside a varint")
			}
			by := delta[pos]
			pos++
			value |= int(by&0x7f) << shift
			shift += 7
			if (by & 0x80) == 0 {
				break
			}
		}
		return value, nil
	}
	sourceSize, err := readVarint()
	if err != nil {
		return nil, err
	}
	targetSize, err := readVarint()
	if err != nil {
		return nil, err
	}
	if sourceSize != len(base) {
		return nil, packErr("delta source size %d does not match base %d", sourceSize, len(base))
	}
	var parts [][]byte
	size := 0
	for pos < len(delta) {
		op := delta[pos]
		pos++
		if (op & 0x80) != 0 {
			offset := 0
			copySize := 0
			for i := 0; i < 4; i++ {
				if (op & (1 << uint(i))) != 0 {
					if pos >= len(delta) {
						return nil, packErr("delta ends inside a copy offset")
					}
					offset |= int(delta[pos]) << (uint(i) * 8)
					pos++
				}
			}
			for i := 0; i < 3; i++ {
				if (op & (0x10 << uint(i))) != 0 {
					if pos >= len(delta) {
						return nil, packErr("delta ends inside a copy size")
					}
					copySize |= int(delta[pos]) << (uint(i) * 8)
					pos++
				}
			}
			if copySize == 0 {
				copySize = 0x10000
			}
			if offset+copySize > len(base) {
				return nil, packErr("delta copy runs past the base")
			}
			parts = append(parts, base[offset:offset+copySize])
			size += copySize
		} else if op != 0 {
			if pos+int(op) > len(delta) {
				return nil, packErr("delta ends inside an insert")
			}
			parts = append(parts, delta[pos:pos+int(op)])
			pos += int(op)
			size += int(op)
		} else {
			return nil, packErr("delta holds a zero opcode")
		}
	}
	if size != targetSize {
		return nil, packErr("delta target size %d does not match %d", targetSize, size)
	}
	var out []byte
	for _, p := range parts {
		out = append(out, p...)
	}
	return out, nil
}

// writePack writes a packfile of plain objects (no deltas), version 2.
func writePack(objects []GitObject) []byte {
	var buf bytes.Buffer
	header := make([]byte, 12)
	copy(header[0:4], "PACK")
	header[4] = 0
	header[5] = 0
	header[6] = 0
	header[7] = 2
	n := len(objects)
	header[8] = byte(n >> 24)
	header[9] = byte(n >> 16)
	header[10] = byte(n >> 8)
	header[11] = byte(n)
	buf.Write(header)
	for _, object := range objects {
		typeCode := packTypeIDs[object.Type]
		size := len(object.Data)
		var hb []byte
		first := (typeCode << 4) | (size & 0x0f)
		rest := size >> 4
		if rest != 0 {
			first |= 0x80
		}
		hb = append(hb, byte(first))
		for rest != 0 {
			by := rest & 0x7f
			rest >>= 7
			if rest != 0 {
				by |= 0x80
			}
			hb = append(hb, byte(by))
		}
		buf.Write(hb)
		buf.Write(deflate(object.Data))
	}
	body := buf.Bytes()
	trailer, _ := hex.DecodeString(sha1Hex(body))
	return append(body, trailer...)
}

func deflate(data []byte) []byte {
	var buf bytes.Buffer
	w, _ := zlib.NewWriterLevel(&buf, zlib.DefaultCompression)
	w.Write(data)
	w.Close()
	return buf.Bytes()
}

// ---------------------------------------------------------------- pkt-lines

type PktKind int

const (
	PktData PktKind = iota
	PktFlush
	PktDelim
	PktResponseEnd
)

type PktLine struct {
	Kind PktKind
	Data []byte
}

func pktLine(data []byte) []byte {
	length := len(data) + 4
	if length > 65520 {
		panic(fmt.Sprintf("pkt-line of %d bytes exceeds 65520", length))
	}
	header := []byte(fmt.Sprintf("%04x", length))
	out := append(header, data...)
	return out
}

func pktLineStr(s string) []byte { return pktLine([]byte(s)) }

var pktFlush = []byte("0000")
var pktDelim = []byte("0001")
var pktResponseEnd = []byte("0002")

func parsePktLines(buffer []byte) ([]PktLine, error) {
	var lines []PktLine
	offset := 0
	for offset < len(buffer) {
		if offset+4 > len(buffer) {
			return nil, packErr("pkt-line stream ends inside a length prefix")
		}
		text := string(buffer[offset : offset+4])
		if text == "0000" {
			lines = append(lines, PktLine{Kind: PktFlush})
			offset += 4
			continue
		}
		if text == "0001" {
			lines = append(lines, PktLine{Kind: PktDelim})
			offset += 4
			continue
		}
		if text == "0002" {
			lines = append(lines, PktLine{Kind: PktResponseEnd})
			offset += 4
			continue
		}
		length, err := strconv.ParseInt(text, 16, 32)
		if err != nil || length < 4 || length > 65520 {
			return nil, packErr("bad pkt-line length %q", text)
		}
		if offset+int(length) > len(buffer) {
			return nil, packErr("pkt-line stream ends inside a line")
		}
		lines = append(lines, PktLine{Kind: PktData, Data: append([]byte(nil), buffer[offset+4:offset+int(length)]...)})
		offset += int(length)
	}
	return lines, nil
}

func firstFlushOffset(buffer []byte) int {
	offset := 0
	for offset+4 <= len(buffer) {
		text := string(buffer[offset : offset+4])
		if text == "0000" {
			return offset + 4
		}
		if text == "0001" || text == "0002" {
			offset += 4
			continue
		}
		length, err := strconv.ParseInt(text, 16, 32)
		if err != nil || length < 4 || length > 65520 {
			return -1
		}
		offset += int(length)
	}
	return -1
}

func sidebandPack(pack []byte) []byte {
	var out []byte
	const chunk = 65515
	for offset := 0; offset < len(pack); offset += chunk {
		end := offset + chunk
		if end > len(pack) {
			end = len(pack)
		}
		payload := append([]byte{1}, pack[offset:end]...)
		out = append(out, pktLine(payload)...)
	}
	return out
}
