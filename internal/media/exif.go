package media

import (
	"bytes"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"strconv"
	"strings"
	"time"
)

// tiff is a bounds-checked view of a TIFF/EXIF block.
type tiff struct {
	data  []byte
	order binary.ByteOrder
}

type tiffEntry struct {
	tag   uint16
	typ   uint16
	count uint32
	value []byte // the raw value bytes, inline or at their offset
}

var tiffTypeSizes = map[uint16]int{1: 1, 2: 1, 3: 2, 4: 4, 5: 8, 6: 1, 7: 1, 8: 2, 9: 4, 10: 8, 11: 4, 12: 8}

func newTIFF(data []byte) (*tiff, error) {
	if len(data) < 8 {
		return nil, errors.New("short TIFF header")
	}
	switch string(data[:4]) {
	case "II*\x00":
		return &tiff{data: data, order: binary.LittleEndian}, nil
	case "MM\x00*":
		return &tiff{data: data, order: binary.BigEndian}, nil
	}
	return nil, errors.New("not a TIFF header")
}

// ifd reads the entries of one image file directory. base is added to value
// offsets (Apple's maker note uses offsets relative to its own start).
func (t *tiff) ifd(offset uint32, base int) (map[uint16]tiffEntry, uint32, error) {
	start := base + int(offset)
	if offset == 0 || start < 0 || start+2 > len(t.data) {
		return nil, 0, errors.New("IFD offset out of range")
	}
	count := int(t.order.Uint16(t.data[start:]))
	if count > 1000 || start+2+count*12 > len(t.data) {
		return nil, 0, errors.New("IFD entries out of range")
	}
	entries := make(map[uint16]tiffEntry, count)
	for index := 0; index < count; index++ {
		raw := t.data[start+2+index*12 : start+2+index*12+12]
		entry := tiffEntry{tag: t.order.Uint16(raw), typ: t.order.Uint16(raw[2:]), count: t.order.Uint32(raw[4:])}
		size, ok := tiffTypeSizes[entry.typ]
		if !ok || entry.count > 1<<20 {
			continue
		}
		total := size * int(entry.count)
		if total <= 4 {
			entry.value = raw[8 : 8+total]
		} else {
			valueOffset := base + int(t.order.Uint32(raw[8:]))
			if valueOffset < 0 || valueOffset+total > len(t.data) {
				continue
			}
			entry.value = t.data[valueOffset : valueOffset+total]
		}
		entries[entry.tag] = entry
	}
	var next uint32
	if end := start + 2 + count*12; end+4 <= len(t.data) {
		next = t.order.Uint32(t.data[end:])
	}
	return entries, next, nil
}

func (t *tiff) text(entries map[uint16]tiffEntry, tag uint16) string {
	entry, ok := entries[tag]
	if !ok || (entry.typ != 2 && entry.typ != 7 && entry.typ != 1) {
		return ""
	}
	value := entry.value
	if index := bytes.IndexByte(value, 0); index >= 0 && entry.typ == 2 {
		value = value[:index]
	}
	return string(value)
}

func (t *tiff) uint(entries map[uint16]tiffEntry, tag uint16) (uint32, bool) {
	entry, ok := entries[tag]
	if !ok || entry.count < 1 {
		return 0, false
	}
	switch entry.typ {
	case 1, 7:
		return uint32(entry.value[0]), true
	case 3, 8:
		return uint32(t.order.Uint16(entry.value)), true
	case 4, 9:
		return t.order.Uint32(entry.value), true
	}
	return 0, false
}

func (t *tiff) rationals(entries map[uint16]tiffEntry, tag uint16) []float64 {
	entry, ok := entries[tag]
	if !ok || (entry.typ != 5 && entry.typ != 10) {
		return nil
	}
	values := make([]float64, 0, entry.count)
	for index := 0; index+8 <= len(entry.value); index += 8 {
		var numerator, denominator float64
		if entry.typ == 5 {
			numerator = float64(t.order.Uint32(entry.value[index:]))
			denominator = float64(t.order.Uint32(entry.value[index+4:]))
		} else {
			numerator = float64(int32(t.order.Uint32(entry.value[index:])))
			denominator = float64(int32(t.order.Uint32(entry.value[index+4:])))
		}
		if denominator == 0 {
			values = append(values, 0)
			continue
		}
		values = append(values, numerator/denominator)
	}
	return values
}

const (
	tagImageWidth   = 0x0100
	tagImageLength  = 0x0101
	tagMake         = 0x010F
	tagModel        = 0x0110
	tagOrientation  = 0x0112
	tagSoftware     = 0x0131
	tagExifIFD      = 0x8769
	tagGPSIFD       = 0x8825
	tagExposure     = 0x829A
	tagFNumber      = 0x829D
	tagISO          = 0x8827
	tagDateOriginal = 0x9003
	tagDateDigitize = 0x9004
	tagOffsetTime   = 0x9010
	tagOffsetOrig   = 0x9011
	tagFocalLength  = 0x920A
	tagMakerNote    = 0x927C
	tagUserComment  = 0x9286
	tagPixelX       = 0xA002
	tagPixelY       = 0xA003
	tagLensModel    = 0xA434
	tagThumbOffset  = 0x0201
	tagThumbLength  = 0x0202
)

// parseTIFF reads IFD0, the EXIF and GPS directories and Apple's maker note.
// standalone is true for a TIFF/DNG file, whose IFD0 also gives dimensions.
func parseTIFF(data []byte, meta *Metadata, location *time.Location, standalone bool) error {
	t, err := newTIFF(data)
	if err != nil {
		return err
	}
	ifd0, _, err := t.ifd(t.order.Uint32(data[4:]), 0)
	if err != nil {
		return err
	}
	meta.Make = firstNonEmpty(meta.Make, t.text(ifd0, tagMake))
	meta.Model = firstNonEmpty(meta.Model, t.text(ifd0, tagModel))
	meta.Software = firstNonEmpty(meta.Software, t.text(ifd0, tagSoftware))
	if orientation, ok := t.uint(ifd0, tagOrientation); ok && orientation >= 1 && orientation <= 8 {
		meta.Orientation = int(orientation)
	}
	if standalone {
		if width, ok := t.uint(ifd0, tagImageWidth); ok {
			meta.Width = int(width)
		}
		if height, ok := t.uint(ifd0, tagImageLength); ok {
			meta.Height = int(height)
		}
	}
	if pointer, ok := t.uint(ifd0, tagExifIFD); ok {
		if exif, _, err := t.ifd(pointer, 0); err == nil {
			parseExifIFD(t, exif, meta, location, standalone)
		}
	}
	if pointer, ok := t.uint(ifd0, tagGPSIFD); ok {
		if gps, _, err := t.ifd(pointer, 0); err == nil {
			parseGPS(t, gps, meta)
		}
	}
	return nil
}

func parseExifIFD(t *tiff, exif map[uint16]tiffEntry, meta *Metadata, location *time.Location, standalone bool) {
	offset := t.text(exif, tagOffsetOrig)
	if offset == "" {
		offset = t.text(exif, tagOffsetTime)
	}
	captured := t.text(exif, tagDateOriginal)
	if captured == "" {
		captured = t.text(exif, tagDateDigitize)
	}
	if meta.CapturedAt == nil {
		meta.CapturedAt, meta.LocalTime = parseCaptureTime(captured, offset, location)
	}
	if !standalone && meta.Width == 0 {
		if width, ok := t.uint(exif, tagPixelX); ok {
			meta.Width = int(width)
		}
		if height, ok := t.uint(exif, tagPixelY); ok {
			meta.Height = int(height)
		}
	}
	meta.Lens = firstNonEmpty(meta.Lens, t.text(exif, tagLensModel))
	if values := t.rationals(exif, tagFocalLength); len(values) > 0 && values[0] > 0 {
		meta.FocalLength = strconv.FormatFloat(values[0], 'f', -1, 64) + " mm"
	}
	if values := t.rationals(exif, tagFNumber); len(values) > 0 && values[0] > 0 {
		meta.Aperture = "ƒ/" + strconv.FormatFloat(values[0], 'f', 1, 64)
	}
	if values := t.rationals(exif, tagExposure); len(values) > 0 && values[0] > 0 {
		if values[0] < 1 {
			meta.Exposure = fmt.Sprintf("1/%d s", int(1/values[0]+0.5))
		} else {
			meta.Exposure = strconv.FormatFloat(values[0], 'f', -1, 64) + " s"
		}
	}
	if iso, ok := t.uint(exif, tagISO); ok && iso > 0 {
		meta.ISO = int(iso)
	}
	if comment, ok := exif[tagUserComment]; ok && len(comment.value) > 8 {
		meta.userComment = string(bytes.Trim(comment.value[8:], "\x00 "))
	}
	if note, ok := exif[tagMakerNote]; ok {
		parseAppleMakerNote(note.value, meta)
	}
}

// parseAppleMakerNote reads "Apple iOS" maker notes: a 14-byte header, then a
// big-endian IFD whose offsets are relative to the start of the note.
func parseAppleMakerNote(note []byte, meta *Metadata) {
	if !bytes.HasPrefix(note, []byte("Apple iOS\x00")) || len(note) < 16 {
		return
	}
	order := binary.ByteOrder(binary.BigEndian)
	if string(note[12:14]) == "II" {
		order = binary.LittleEndian
	}
	t := &tiff{data: note, order: order}
	entries, _, err := t.ifd(14, 0)
	if err != nil {
		return
	}
	// Tag 0x0011 is the content identifier shared by a Live Photo's still and
	// its motion video.
	if id := t.text(entries, 0x0011); id != "" {
		meta.LivePhotoID = id
	}
}

func parseGPS(t *tiff, gps map[uint16]tiffEntry, meta *Metadata) {
	latitude := t.rationals(gps, 0x0002)
	longitude := t.rationals(gps, 0x0004)
	if len(latitude) < 3 || len(longitude) < 3 {
		return
	}
	lat := latitude[0] + latitude[1]/60 + latitude[2]/3600
	lon := longitude[0] + longitude[1]/60 + longitude[2]/3600
	if strings.EqualFold(strings.TrimSpace(t.text(gps, 0x0001)), "S") {
		lat = -lat
	}
	if strings.EqualFold(strings.TrimSpace(t.text(gps, 0x0003)), "W") {
		lon = -lon
	}
	meta.Latitude, meta.Longitude = &lat, &lon
	if altitude := t.rationals(gps, 0x0006); len(altitude) > 0 {
		value := altitude[0]
		if reference, ok := t.uint(gps, 0x0005); ok && reference == 1 {
			value = -value
		}
		meta.Altitude = &value
	}
}

func firstNonEmpty(values ...string) string {
	for _, value := range values {
		if strings.TrimSpace(strings.Trim(value, "\x00")) != "" {
			return value
		}
	}
	return ""
}

// parseJPEG walks JPEG segments up to the start of scan: APP1 Exif carries
// the metadata and the first SOF marker the pixel dimensions.
func parseJPEG(r io.ReaderAt, size int64, meta *Metadata, location *time.Location) error {
	data := make([]byte, min64(size, maxHeaderRead))
	read, err := r.ReadAt(data, 0)
	if err != nil && !errors.Is(err, io.EOF) {
		return err
	}
	data = data[:read]
	position := 2
	for position+4 <= len(data) {
		if data[position] != 0xFF {
			return nil
		}
		marker := data[position+1]
		if marker == 0xFF {
			position++
			continue
		}
		if marker == 0xD8 || marker == 0x01 || (marker >= 0xD0 && marker <= 0xD7) {
			position += 2
			continue
		}
		if marker == 0xDA || marker == 0xD9 {
			return nil
		}
		length := int(binary.BigEndian.Uint16(data[position+2:]))
		if length < 2 || position+2+length > len(data) {
			return nil
		}
		segment := data[position+4 : position+2+length]
		switch {
		case marker == 0xE1 && bytes.HasPrefix(segment, []byte("Exif\x00\x00")):
			_ = parseTIFF(segment[6:], meta, location, false)
		case isStartOfFrame(marker) && len(segment) >= 5:
			// The first frame header is the primary image (an MPO's extra
			// images come later); it is authoritative over EXIF dimensions.
			meta.Height = int(binary.BigEndian.Uint16(segment[1:]))
			meta.Width = int(binary.BigEndian.Uint16(segment[3:]))
			return nil
		}
		position += 2 + length
	}
	return nil
}

func isStartOfFrame(marker byte) bool {
	return marker >= 0xC0 && marker <= 0xCF && marker != 0xC4 && marker != 0xC8 && marker != 0xCC
}

// ExifThumbnail returns the small JPEG preview a camera embeds in a JPEG's
// EXIF block, which is often available even when only the first few hundred
// KB of an upload have arrived.
func ExifThumbnail(r io.ReaderAt, size int64) ([]byte, bool) {
	data := make([]byte, min64(size, 128<<10))
	read, err := r.ReadAt(data, 0)
	if err != nil && !errors.Is(err, io.EOF) {
		return nil, false
	}
	data = data[:read]
	if !bytes.HasPrefix(data, []byte{0xFF, 0xD8}) {
		return nil, false
	}
	position := 2
	for position+4 <= len(data) {
		if data[position] != 0xFF || data[position+1] == 0xDA {
			return nil, false
		}
		length := int(binary.BigEndian.Uint16(data[position+2:]))
		if length < 2 || position+2+length > len(data) {
			return nil, false
		}
		segment := data[position+4 : position+2+length]
		if data[position+1] == 0xE1 && bytes.HasPrefix(segment, []byte("Exif\x00\x00")) {
			t, err := newTIFF(segment[6:])
			if err != nil {
				return nil, false
			}
			_, next, err := t.ifd(t.order.Uint32(t.data[4:]), 0)
			if err != nil || next == 0 {
				return nil, false
			}
			ifd1, _, err := t.ifd(next, 0)
			if err != nil {
				return nil, false
			}
			offset, okOffset := t.uint(ifd1, tagThumbOffset)
			length, okLength := t.uint(ifd1, tagThumbLength)
			if !okOffset || !okLength || length == 0 || length > 64<<10 || int(offset)+int(length) > len(t.data) {
				return nil, false
			}
			thumbnail := t.data[offset : offset+length]
			if !bytes.HasPrefix(thumbnail, []byte{0xFF, 0xD8}) {
				return nil, false
			}
			return append([]byte(nil), thumbnail...), true
		}
		position += 2 + length
	}
	return nil, false
}

// parsePNG reads IHDR for dimensions and eXIf/XMP chunks for metadata. iOS
// marks screenshots with the EXIF user comment "Screenshot".
func parsePNG(r io.ReaderAt, size int64, meta *Metadata, location *time.Location) error {
	meta.isPNG = true
	data := make([]byte, min64(size, maxHeaderRead))
	read, err := r.ReadAt(data, 0)
	if err != nil && !errors.Is(err, io.EOF) {
		return err
	}
	data = data[:read]
	position := 8
	for position+8 <= len(data) {
		length := int(binary.BigEndian.Uint32(data[position:]))
		kind := string(data[position+4 : position+8])
		if length < 0 || position+8+length > len(data) {
			return nil
		}
		chunk := data[position+8 : position+8+length]
		switch kind {
		case "IHDR":
			if len(chunk) >= 8 {
				meta.Width = int(binary.BigEndian.Uint32(chunk))
				meta.Height = int(binary.BigEndian.Uint32(chunk[4:]))
			}
		case "eXIf":
			_ = parseTIFF(chunk, meta, location, false)
		case "iTXt", "tEXt":
			if bytes.HasPrefix(chunk, []byte("XML:com.adobe.xmp")) {
				parseXMP(chunk, meta, location)
			}
		case "IDAT", "IEND":
			return nil
		}
		position += 12 + length
	}
	return nil
}

// parseXMP picks the few XMP properties iOS writes into screenshots.
func parseXMP(packet []byte, meta *Metadata, location *time.Location) {
	text := string(packet)
	if comment := xmpValue(text, "exif:UserComment"); comment != "" {
		meta.userComment = comment
	} else if strings.Contains(text, ">Screenshot<") {
		meta.userComment = "Screenshot"
	}
	if meta.CapturedAt == nil {
		for _, name := range []string{"photoshop:DateCreated", "exif:DateTimeOriginal", "xmp:CreateDate"} {
			if value := xmpValue(text, name); value != "" {
				if parsed, err := time.Parse(time.RFC3339, value); err == nil {
					utc := parsed.UTC()
					meta.CapturedAt, meta.LocalTime = &utc, parsed.Format("2006-01-02T15:04:05")
					break
				}
				if parsed, wall := parseCaptureTime(strings.ReplaceAll(value, "-", ":"), "", location); parsed != nil {
					meta.CapturedAt, meta.LocalTime = parsed, wall
					break
				}
			}
		}
	}
}

func xmpValue(text, name string) string {
	start := strings.Index(text, "<"+name+">")
	if start < 0 {
		if attribute := strings.Index(text, name+"=\""); attribute >= 0 {
			rest := text[attribute+len(name)+2:]
			if end := strings.IndexByte(rest, '"'); end >= 0 && end < 200 {
				return rest[:end]
			}
		}
		return ""
	}
	rest := text[start+len(name)+2:]
	end := strings.Index(rest, "</"+name+">")
	if end < 0 || end > 4096 {
		return ""
	}
	value := rest[:end]
	// rdf:Alt/rdf:li wrappers around the value.
	for strings.Contains(value, "<") {
		open := strings.IndexByte(value, '>')
		closeTag := strings.LastIndexByte(value, '<')
		if open < 0 || closeTag <= open {
			break
		}
		inner := value[open+1 : closeTag]
		if inner == value {
			break
		}
		value = inner
	}
	return strings.TrimSpace(value)
}
