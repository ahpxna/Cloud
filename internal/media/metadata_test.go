package media

import (
	"bytes"
	"encoding/binary"
	"testing"
	"time"
)

// ---------------------------------------------------------------- builders

type ifdEntry struct {
	tag   uint16
	typ   uint16
	count uint32
	data  []byte
}

func asciiEntry(tag uint16, value string) ifdEntry {
	return ifdEntry{tag: tag, typ: 2, count: uint32(len(value) + 1), data: append([]byte(value), 0)}
}

func longEntry(tag uint16, value uint32) ifdEntry {
	data := make([]byte, 4)
	binary.BigEndian.PutUint32(data, value)
	return ifdEntry{tag: tag, typ: 4, count: 1, data: data}
}

func shortEntry(tag uint16, value uint16) ifdEntry {
	data := make([]byte, 2)
	binary.BigEndian.PutUint16(data, value)
	return ifdEntry{tag: tag, typ: 3, count: 1, data: data}
}

func rationalEntry(tag uint16, values ...[2]uint32) ifdEntry {
	data := make([]byte, 8*len(values))
	for index, value := range values {
		binary.BigEndian.PutUint32(data[index*8:], value[0])
		binary.BigEndian.PutUint32(data[index*8+4:], value[1])
	}
	return ifdEntry{tag: tag, typ: 5, count: uint32(len(values)), data: data}
}

func undefinedEntry(tag uint16, data []byte) ifdEntry {
	return ifdEntry{tag: tag, typ: 7, count: uint32(len(data)), data: data}
}

// writeIFD appends a big-endian IFD at the end of out (offsets relative to
// base) and returns its offset. Pointer entries are patched by the caller via
// the returned value offsets.
func writeIFD(out *bytes.Buffer, base int, entries []ifdEntry) int {
	start := out.Len()
	header := make([]byte, 2+12*len(entries)+4)
	binary.BigEndian.PutUint16(header, uint16(len(entries)))
	var extra bytes.Buffer
	extraStart := start + len(header)
	for index, entry := range entries {
		raw := header[2+index*12:]
		binary.BigEndian.PutUint16(raw, entry.tag)
		binary.BigEndian.PutUint16(raw[2:], entry.typ)
		binary.BigEndian.PutUint32(raw[4:], entry.count)
		if len(entry.data) <= 4 {
			copy(raw[8:], entry.data)
		} else {
			binary.BigEndian.PutUint32(raw[8:], uint32(extraStart+extra.Len()-base))
			extra.Write(entry.data)
			if extra.Len()%2 == 1 {
				extra.WriteByte(0)
			}
		}
	}
	out.Write(header)
	out.Write(extra.Bytes())
	return start - base
}

// buildExifTIFF builds a TIFF block like an iPhone writes: IFD0 with camera,
// an EXIF IFD with dates, lens and an Apple maker note, and a GPS IFD.
func buildExifTIFF(t *testing.T, liveID string, comment string) []byte {
	t.Helper()
	var exif bytes.Buffer
	exif.WriteString("MM\x00*")
	exif.Write([]byte{0, 0, 0, 8})

	// Lay out IFD0 last-known pointers by building sub-IFDs first in scratch
	// buffers, then concatenating with fixed offsets.
	makerNote := buildAppleMakerNote(liveID)
	userComment := append([]byte("ASCII\x00\x00\x00"), []byte(comment)...)

	ifd0 := []ifdEntry{
		asciiEntry(tagMake, "Apple"),
		asciiEntry(tagModel, "iPhone 14 Pro"),
		shortEntry(tagOrientation, 6),
		asciiEntry(tagSoftware, "26.5.2"),
		longEntry(tagExifIFD, 0),
		longEntry(tagGPSIFD, 0),
	}
	// First pass to learn sizes.
	var scratch bytes.Buffer
	scratch.Write(exif.Bytes())
	writeIFD(&scratch, 0, ifd0)
	exifOffset := scratch.Len()
	exifEntries := []ifdEntry{
		rationalEntry(tagExposure, [2]uint32{1, 120}),
		rationalEntry(tagFNumber, [2]uint32{178, 100}),
		shortEntry(tagISO, 64),
		asciiEntry(tagDateOriginal, "2026:09:09 11:36:31"),
		asciiEntry(tagOffsetOrig, "-04:00"),
		rationalEntry(tagFocalLength, [2]uint32{686, 100}),
		undefinedEntry(tagMakerNote, makerNote),
		undefinedEntry(tagUserComment, userComment),
		longEntry(tagPixelX, 4032),
		longEntry(tagPixelY, 3024),
		asciiEntry(tagLensModel, "iPhone 14 Pro back triple camera 6.86mm f/1.78"),
	}
	writeIFD(&scratch, 0, exifEntries)
	gpsOffset := scratch.Len()

	binary.BigEndian.PutUint32(ifd0[4].data, uint32(exifOffset))
	binary.BigEndian.PutUint32(ifd0[5].data, uint32(gpsOffset))
	writeIFD(&exif, 0, ifd0)
	if exif.Len() != exifOffset {
		t.Fatalf("IFD0 layout changed: %d != %d", exif.Len(), exifOffset)
	}
	writeIFD(&exif, 0, exifEntries)
	writeIFD(&exif, 0, []ifdEntry{
		asciiEntry(0x0001, "N"),
		rationalEntry(0x0002, [2]uint32{40, 1}, [2]uint32{16, 1}, [2]uint32{3504, 100}),
		asciiEntry(0x0003, "W"),
		rationalEntry(0x0004, [2]uint32{74, 1}, [2]uint32{44, 1}, [2]uint32{2796, 100}),
		{tag: 0x0005, typ: 1, count: 1, data: []byte{0}},
		rationalEntry(0x0006, [2]uint32{38376, 1000}),
	})
	return exif.Bytes()
}

func buildAppleMakerNote(liveID string) []byte {
	var note bytes.Buffer
	note.WriteString("Apple iOS\x00")
	note.Write([]byte{0, 1})
	note.WriteString("MM")
	entries := []ifdEntry{asciiEntry(0x0001, "x"), asciiEntry(0x0011, liveID)}
	writeIFD(&note, 0, entries)
	return note.Bytes()
}

func buildJPEG(t *testing.T, exif []byte, width, height uint16) []byte {
	t.Helper()
	var out bytes.Buffer
	out.Write([]byte{0xFF, 0xD8})
	segment := append([]byte("Exif\x00\x00"), exif...)
	out.Write([]byte{0xFF, 0xE1})
	_ = binary.Write(&out, binary.BigEndian, uint16(len(segment)+2))
	out.Write(segment)
	frame := []byte{8, 0, 0, 0, 0, 3}
	binary.BigEndian.PutUint16(frame[1:], height)
	binary.BigEndian.PutUint16(frame[3:], width)
	out.Write([]byte{0xFF, 0xC0})
	_ = binary.Write(&out, binary.BigEndian, uint16(len(frame)+2))
	out.Write(frame)
	out.Write([]byte{0xFF, 0xDA, 0, 2, 0xFF, 0xD9})
	return out.Bytes()
}

func atom(kind string, payload ...[]byte) []byte {
	body := bytes.Join(payload, nil)
	out := make([]byte, 8, 8+len(body))
	binary.BigEndian.PutUint32(out, uint32(8+len(body)))
	copy(out[4:], kind)
	return append(out, body...)
}

func u32(value uint32) []byte {
	out := make([]byte, 4)
	binary.BigEndian.PutUint32(out, value)
	return out
}

func u16(value uint16) []byte {
	out := make([]byte, 2)
	binary.BigEndian.PutUint16(out, value)
	return out
}

func buildQuickTime(rotated bool, keys map[string]string) []byte {
	created := uint32(time.Date(2026, 10, 3, 0, 43, 23, 0, time.UTC).Sub(quickTimeEpoch) / time.Second)
	mvhd := atom("mvhd", u32(0), u32(created), u32(created), u32(600), u32(600*15), make([]byte, 80))
	matrix := make([]byte, 36)
	if rotated {
		binary.BigEndian.PutUint32(matrix[4:], 0x00010000)
		binary.BigEndian.PutUint32(matrix[12:], 0xFFFF0000)
	} else {
		binary.BigEndian.PutUint32(matrix[0:], 0x00010000)
		binary.BigEndian.PutUint32(matrix[16:], 0x00010000)
	}
	tkhd := atom("tkhd", u32(7), u32(created), u32(created), u32(1), u32(0), u32(9000), make([]byte, 8), u16(0), u16(0), u16(0), u16(0), matrix, u32(3840<<16), u32(2160<<16))
	trak := atom("trak", tkhd)
	var keyEntries, items [][]byte
	index := uint32(0)
	for _, name := range []string{"com.apple.quicktime.creationdate", "com.apple.quicktime.content.identifier", "com.apple.quicktime.location.ISO6709", "com.apple.quicktime.make", "com.apple.quicktime.camera.identifier"} {
		value, ok := keys[name]
		if !ok {
			continue
		}
		index++
		keyEntries = append(keyEntries, append(u32(uint32(8+len(name))), append([]byte("mdta"), []byte(name)...)...))
		data := atom("data", u32(1), u32(0), []byte(value))
		item := make([]byte, 8)
		binary.BigEndian.PutUint32(item, uint32(8+len(data)))
		binary.BigEndian.PutUint32(item[4:], index)
		items = append(items, append(item, data...))
	}
	metaAtom := atom("meta",
		atom("hdlr", u32(0), u32(0), []byte("mdta"), make([]byte, 13)),
		atom("keys", u32(0), u32(index), bytes.Join(keyEntries, nil)),
		atom("ilst", bytes.Join(items, nil)),
	)
	moov := atom("moov", mvhd, trak, metaAtom)
	ftyp := atom("ftyp", []byte("qt  "), u32(0), []byte("qt  "))
	return bytes.Join([][]byte{ftyp, atom("wide"), moov, atom("mdat", make([]byte, 64))}, nil)
}

func buildHEIF(exif []byte, rotate bool) []byte {
	ftyp := atom("ftyp", []byte("heic"), u32(0), []byte("mif1heic"))
	pitm := atom("pitm", u32(0), u16(1))
	infe1 := atom("infe", []byte{2, 0, 0, 0}, u16(1), u16(0), []byte("grid"), []byte{0})
	infe2 := atom("infe", []byte{2, 0, 0, 0}, u16(2), u16(0), []byte("Exif"), []byte{0})
	iinf := atom("iinf", u32(0), u16(2), infe1, infe2)
	ispe := atom("ispe", u32(0), u32(4032), u32(3024))
	irot := atom("irot", []byte{1})
	ipco := atom("ipco", ispe, irot)
	associations := []byte{0x81}
	if rotate {
		associations = []byte{0x81, 0x82}
	}
	ipma := atom("ipma", u32(0), u32(1), u16(1), []byte{byte(len(associations))}, associations)
	iprp := atom("iprp", ipco, ipma)
	exifPayload := append(u32(6), append([]byte("Exif\x00\x00"), exif...)...)
	// iloc v0: offset_size=4, length_size=4, base_offset_size=0.
	build := func(exifOffset uint32) []byte {
		iloc := atom("iloc", u32(0), []byte{0x44, 0x00}, u16(1), u16(2), u16(0), u16(1), u32(exifOffset), u32(uint32(len(exifPayload))))
		return atom("meta", u32(0), atom("hdlr", u32(0), u32(0), []byte("pict"), make([]byte, 13)), pitm, iinf, iloc, iprp)
	}
	metaBox := build(0)
	offset := uint32(len(ftyp) + len(metaBox) + 8)
	metaBox = build(offset)
	return bytes.Join([][]byte{ftyp, metaBox, atom("mdat", exifPayload)}, nil)
}

func buildPNG(xmp string) []byte {
	chunk := func(kind string, data []byte) []byte {
		out := append(u32(uint32(len(data))), []byte(kind)...)
		out = append(out, data...)
		return append(out, 0, 0, 0, 0)
	}
	ihdr := append(append(u32(1179), u32(2556)...), 8, 6, 0, 0, 0)
	text := append([]byte("XML:com.adobe.xmp\x00\x00\x00\x00\x00"), []byte(xmp)...)
	return bytes.Join([][]byte{[]byte("\x89PNG\r\n\x1a\n"), chunk("IHDR", ihdr), chunk("iTXt", text), chunk("IDAT", []byte{0}), chunk("IEND", nil)}, nil)
}

// ---------------------------------------------------------------- tests

func extract(t *testing.T, data []byte) Metadata {
	t.Helper()
	meta, err := Extract(bytes.NewReader(data), int64(len(data)), nil)
	if err != nil {
		t.Fatalf("Extract: %v", err)
	}
	return meta
}

func TestJPEGFromIPhone(t *testing.T) {
	meta := extract(t, buildJPEG(t, buildExifTIFF(t, "8F3F4267-637B-4C5E-9D49-AD758212F122", ""), 4032, 3024))
	want := time.Date(2026, 9, 9, 15, 36, 31, 0, time.UTC)
	if meta.CapturedAt == nil || !meta.CapturedAt.Equal(want) || meta.LocalTime != "2026-09-09T11:36:31" {
		t.Fatalf("captured = %v %q", meta.CapturedAt, meta.LocalTime)
	}
	// Orientation 6 means the displayed photo is portrait.
	if meta.Width != 3024 || meta.Height != 4032 || meta.Orientation != 6 {
		t.Fatalf("size = %dx%d orientation %d", meta.Width, meta.Height, meta.Orientation)
	}
	if meta.Make != "Apple" || meta.Model != "iPhone 14 Pro" || meta.Lens == "" || meta.Software != "26.5.2" {
		t.Fatalf("camera = %+v", meta)
	}
	if meta.LivePhotoID != "8F3F4267-637B-4C5E-9D49-AD758212F122" {
		t.Fatalf("live photo id = %q", meta.LivePhotoID)
	}
	if meta.Latitude == nil || *meta.Latitude < 40.27 || *meta.Latitude > 40.28 || meta.Longitude == nil || *meta.Longitude > -74.73 || *meta.Longitude < -74.75 {
		t.Fatalf("location = %v,%v", meta.Latitude, meta.Longitude)
	}
	if meta.Aperture != "ƒ/1.8" || meta.Exposure != "1/120 s" || meta.ISO != 64 || meta.FocalLength != "6.86 mm" {
		t.Fatalf("exposure = %q %q %d %q", meta.Aperture, meta.Exposure, meta.ISO, meta.FocalLength)
	}
	if meta.Subtype != "" {
		t.Fatalf("subtype = %q", meta.Subtype)
	}
}

func TestScreenshotAndSelfieSubtypes(t *testing.T) {
	jpeg := buildJPEG(t, buildExifTIFF(t, "", "Screenshot"), 1179, 2556)
	if meta := extract(t, jpeg); meta.Subtype != "screenshot" {
		t.Fatalf("JPEG screenshot subtype = %q", meta.Subtype)
	}
	png := buildPNG(`<x:xmpmeta><rdf:RDF><rdf:Description><exif:UserComment><rdf:Alt><rdf:li xml:lang="x-default">Screenshot</rdf:li></rdf:Alt></exif:UserComment><photoshop:DateCreated>2026-10-05T12:01:22+07:00</photoshop:DateCreated></rdf:Description></rdf:RDF></x:xmpmeta>`)
	meta := extract(t, png)
	if meta.Subtype != "screenshot" || meta.Width != 1179 || meta.Height != 2556 {
		t.Fatalf("PNG = %+v", meta)
	}
	if meta.CapturedAt == nil || !meta.CapturedAt.Equal(time.Date(2026, 10, 5, 5, 1, 22, 0, time.UTC)) {
		t.Fatalf("PNG captured = %v", meta.CapturedAt)
	}
	video := buildQuickTime(false, map[string]string{"com.apple.quicktime.camera.identifier": "Front"})
	if meta := extract(t, video); meta.Subtype != "selfie" {
		t.Fatalf("front camera video subtype = %q", meta.Subtype)
	}
}

func TestLivePhotoVideoFromIPhone(t *testing.T) {
	meta := extract(t, buildQuickTime(true, map[string]string{
		"com.apple.quicktime.creationdate":       "2026-10-02T20:43:23-0400",
		"com.apple.quicktime.content.identifier": "8F3F4267-637B-4C5E-9D49-AD758212F122",
		"com.apple.quicktime.location.ISO6709":   "+40.2764-074.7411+038.376/",
		"com.apple.quicktime.make":               "Apple",
	}))
	if meta.CapturedAt == nil || !meta.CapturedAt.Equal(time.Date(2026, 10, 3, 0, 43, 23, 0, time.UTC)) || meta.LocalTime != "2026-10-02T20:43:23" {
		t.Fatalf("captured = %v %q", meta.CapturedAt, meta.LocalTime)
	}
	if meta.DurationMS != 15000 {
		t.Fatalf("duration = %d", meta.DurationMS)
	}
	if meta.Width != 2160 || meta.Height != 3840 {
		t.Fatalf("rotated size = %dx%d", meta.Width, meta.Height)
	}
	if meta.LivePhotoID != "8F3F4267-637B-4C5E-9D49-AD758212F122" || meta.Make != "Apple" {
		t.Fatalf("keys = %+v", meta)
	}
	if meta.Latitude == nil || *meta.Latitude != 40.2764 || *meta.Longitude != -74.7411 || meta.Altitude == nil {
		t.Fatalf("location = %v %v %v", meta.Latitude, meta.Longitude, meta.Altitude)
	}
}

func TestVideoWithoutAppleKeysUsesHeaderTime(t *testing.T) {
	meta := extract(t, buildQuickTime(false, nil))
	if meta.CapturedAt == nil || !meta.CapturedAt.Equal(time.Date(2026, 10, 3, 0, 43, 23, 0, time.UTC)) {
		t.Fatalf("captured = %v", meta.CapturedAt)
	}
	if meta.Width != 3840 || meta.Height != 2160 {
		t.Fatalf("size = %dx%d", meta.Width, meta.Height)
	}
}

func TestHEIFPrimaryItemSizeRotationAndExif(t *testing.T) {
	exif := buildExifTIFF(t, "03C9114D-8EF3-48F3-8D76-72315E901403", "")
	meta := extract(t, buildHEIF(exif, false))
	if meta.Width != 4032 || meta.Height != 3024 {
		t.Fatalf("size = %dx%d", meta.Width, meta.Height)
	}
	if meta.LivePhotoID != "03C9114D-8EF3-48F3-8D76-72315E901403" || meta.CapturedAt == nil || meta.Make != "Apple" {
		t.Fatalf("exif = %+v", meta)
	}
	// irot quarter turn swaps the displayed size. (EXIF orientation 6 swaps it
	// back in this synthetic file, so check irot alone.)
	rotated := extract(t, buildHEIF(buildJPEGlessExif(t), true))
	if rotated.Width != 3024 || rotated.Height != 4032 {
		t.Fatalf("rotated size = %dx%d", rotated.Width, rotated.Height)
	}
}

// buildJPEGlessExif is a minimal TIFF block with no orientation tag.
func buildJPEGlessExif(t *testing.T) []byte {
	t.Helper()
	var out bytes.Buffer
	out.WriteString("MM\x00*")
	out.Write([]byte{0, 0, 0, 8})
	writeIFD(&out, 0, []ifdEntry{asciiEntry(tagMake, "Apple")})
	return out.Bytes()
}

func TestPartialUploadsAndGarbageNeverPanic(t *testing.T) {
	samples := [][]byte{
		buildJPEG(t, buildExifTIFF(t, "id", "Screenshot"), 10, 10),
		buildQuickTime(true, map[string]string{"com.apple.quicktime.creationdate": "2026-10-02T20:43:23-0400"}),
		buildHEIF(buildExifTIFF(t, "id", ""), true),
		buildPNG("<exif:UserComment>Screenshot</exif:UserComment>"),
	}
	for _, sample := range samples {
		for cut := 0; cut <= len(sample); cut++ {
			_, _ = Extract(bytes.NewReader(sample[:cut]), int64(cut), nil)
		}
		corrupted := append([]byte(nil), sample...)
		for index := 8; index < len(corrupted); index += 7 {
			corrupted[index] ^= 0xFF
			_, _ = Extract(bytes.NewReader(corrupted), int64(len(corrupted)), nil)
		}
	}
	if _, err := Extract(bytes.NewReader([]byte("plain text")), 10, nil); err != ErrUnsupported {
		t.Fatalf("text error = %v", err)
	}
}

func TestPartialVideoStillYieldsHeaderMetadata(t *testing.T) {
	video := buildQuickTime(false, map[string]string{"com.apple.quicktime.creationdate": "2026-10-02T20:43:23-0400"})
	// Only the moov box has arrived; mdat is cut off.
	partial := video[:len(video)-40]
	meta := extract(t, partial)
	if meta.DurationMS != 15000 || meta.CapturedAt == nil {
		t.Fatalf("partial = %+v", meta)
	}
}

func TestExifThumbnail(t *testing.T) {
	thumbnail := []byte{0xFF, 0xD8, 0xFF, 0xDB, 1, 2, 3, 0xFF, 0xD9}
	var exif bytes.Buffer
	exif.WriteString("MM\x00*")
	exif.Write([]byte{0, 0, 0, 8})
	// IFD0 with one entry, then IFD1 pointing at the thumbnail bytes.
	ifd0Size := 2 + 12 + 4
	ifd1Offset := 8 + ifd0Size
	ifd1Size := 2 + 2*12 + 4
	thumbOffset := ifd1Offset + ifd1Size
	header := make([]byte, ifd0Size)
	binary.BigEndian.PutUint16(header, 1)
	copy(header[2:], append(append(u16(tagOrientation), u16(3)...), append(u32(1), []byte{0, 1, 0, 0}...)...))
	binary.BigEndian.PutUint32(header[14:], uint32(ifd1Offset))
	exif.Write(header)
	ifd1 := make([]byte, ifd1Size)
	binary.BigEndian.PutUint16(ifd1, 2)
	copy(ifd1[2:], append(append(u16(tagThumbOffset), u16(4)...), append(u32(1), u32(uint32(thumbOffset))...)...))
	copy(ifd1[14:], append(append(u16(tagThumbLength), u16(4)...), append(u32(1), u32(uint32(len(thumbnail)))...)...))
	exif.Write(ifd1)
	exif.Write(thumbnail)
	jpeg := buildJPEG(t, exif.Bytes(), 100, 50)
	got, ok := ExifThumbnail(bytes.NewReader(jpeg), int64(len(jpeg)))
	if !ok || !bytes.Equal(got, thumbnail) {
		t.Fatalf("thumbnail = %v %x", ok, got)
	}
}

func FuzzExtract(f *testing.F) {
	f.Add([]byte{0xFF, 0xD8, 0xFF, 0xE1, 0, 8, 'E', 'x', 'i', 'f', 0, 0})
	f.Add([]byte("\x00\x00\x00\x14ftypqt  \x00\x00\x00\x00qt  \x00\x00\x00\x08moov"))
	f.Add([]byte("\x00\x00\x00\x14ftypheic\x00\x00\x00\x00mif1\x00\x00\x00\x0cmeta\x00\x00\x00\x00"))
	f.Add([]byte("\x89PNG\r\n\x1a\n\x00\x00\x00\x0dIHDR"))
	f.Fuzz(func(t *testing.T, data []byte) {
		_, _ = Extract(bytes.NewReader(data), int64(len(data)), time.UTC)
		_, _ = ExifThumbnail(bytes.NewReader(data), int64(len(data)))
	})
}
