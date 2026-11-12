package media

import (
	"encoding/binary"
	"errors"
	"io"
	"math"
	"regexp"
	"strconv"
	"strings"
	"time"
)

// box is one ISO base media file format box (QuickTime "atom").
type box struct {
	kind       string
	start      int64 // offset of the header
	dataStart  int64 // offset of the payload
	end        int64 // offset just past the box
	headerSize int64
}

// walkBoxes lists the boxes in [start, end). A box larger than the readable
// range (a partial upload) is reported with its declared end, so callers must
// bound their reads by size.
func walkBoxes(r io.ReaderAt, start, end, size int64) ([]box, error) {
	var boxes []box
	header := make([]byte, 16)
	for position := start; position+8 <= end && position+8 <= size; {
		if len(boxes) > 10000 {
			return boxes, errors.New("too many boxes")
		}
		read, err := r.ReadAt(header, position)
		if read < 8 {
			if err == nil {
				err = io.ErrUnexpectedEOF
			}
			return boxes, err
		}
		length := int64(binary.BigEndian.Uint32(header))
		kind := string(header[4:8])
		headerSize := int64(8)
		switch length {
		case 0:
			length = end - position
		case 1:
			if read < 16 {
				return boxes, io.ErrUnexpectedEOF
			}
			large := binary.BigEndian.Uint64(header[8:])
			if large > math.MaxInt64/2 {
				return boxes, errors.New("box too large")
			}
			length = int64(large)
			headerSize = 16
		}
		if length < headerSize || position+length > end {
			// A truncated last box is still useful for partial uploads.
			if length < headerSize {
				return boxes, errors.New("invalid box size")
			}
			length = end - position
		}
		boxes = append(boxes, box{kind: kind, start: position, dataStart: position + headerSize, end: position + length, headerSize: headerSize})
		position += length
	}
	return boxes, nil
}

func readBox(r io.ReaderAt, b box, size int64, limit int64) ([]byte, error) {
	end := b.end
	if end > size {
		end = size
	}
	if end-b.dataStart > limit {
		return nil, errors.New("box exceeds read limit")
	}
	if end <= b.dataStart {
		return nil, io.ErrUnexpectedEOF
	}
	data := make([]byte, end-b.dataStart)
	read, err := r.ReadAt(data, b.dataStart)
	if err != nil && !errors.Is(err, io.EOF) {
		return nil, err
	}
	return data[:read], nil
}

func findBox(boxes []box, kind string) (box, bool) {
	for _, candidate := range boxes {
		if candidate.kind == kind {
			return candidate, true
		}
	}
	return box{}, false
}

// --------------------------------------------------------------------- HEIF

type heifItem struct {
	id     uint32
	kind   string
	method uint16
	base   uint64
	extent [][2]uint64 // offset, length
}

// parseHEIF reads the primary image size (ispe + irot) and the Exif item of a
// HEIC/HEIF/AVIF still image.
func parseHEIF(r io.ReaderAt, size int64, meta *Metadata, location *time.Location) error {
	top, err := walkBoxes(r, 0, size, size)
	if err != nil && len(top) == 0 {
		return err
	}
	metaBox, ok := findBox(top, "meta")
	if !ok {
		return errors.New("HEIF meta box missing")
	}
	data, err := readBox(r, metaBox, size, maxBoxRead)
	if err != nil || len(data) < 4 {
		return errors.New("HEIF meta box unreadable")
	}
	children := parseChildren(data[4:])
	var primary uint32
	if pitm, ok := children["pitm"]; ok && len(pitm) >= 6 {
		if pitm[0] == 0 {
			primary = uint32(binary.BigEndian.Uint16(pitm[4:]))
		} else if len(pitm) >= 8 {
			primary = binary.BigEndian.Uint32(pitm[4:])
		}
	}
	items := parseItemInfo(children["iinf"])
	locations := parseItemLocations(children["iloc"])
	for id, location := range locations {
		if item, ok := items[id]; ok {
			location.kind = item.kind
		}
		locations[id] = location
	}
	if iprp, ok := children["iprp"]; ok {
		applyItemProperties(parseChildren(iprp), primary, meta)
		meta.sizeOriented = true
	}
	var idat []byte
	if value, ok := children["idat"]; ok {
		idat = value
	}
	for _, item := range locations {
		if item.kind != "Exif" || len(item.extent) == 0 {
			continue
		}
		exif := readItem(r, size, item, idat, maxExifRead)
		if len(exif) < 4 {
			continue
		}
		skip := int(binary.BigEndian.Uint32(exif))
		if skip < 0 || 4+skip >= len(exif) {
			continue
		}
		_ = parseTIFF(exif[4+skip:], meta, location, false)
		break
	}
	return nil
}

// parseChildren maps the boxes directly inside a payload by type (first wins).
func parseChildren(data []byte) map[string][]byte {
	children := make(map[string][]byte)
	for position := 0; position+8 <= len(data); {
		length := int(binary.BigEndian.Uint32(data[position:]))
		kind := string(data[position+4 : position+8])
		header := 8
		if length == 1 && position+16 <= len(data) {
			large := binary.BigEndian.Uint64(data[position+8:])
			if large > uint64(len(data)) {
				return children
			}
			length = int(large)
			header = 16
		} else if length == 0 {
			length = len(data) - position
		}
		if length < header || position+length > len(data) {
			return children
		}
		if _, exists := children[kind]; !exists {
			children[kind] = data[position+header : position+length]
		}
		position += length
	}
	return children
}

func parseItemInfo(data []byte) map[uint32]heifItem {
	items := make(map[uint32]heifItem)
	if len(data) < 6 {
		return items
	}
	position := 6
	if data[0] != 0 {
		position = 8
	}
	for position+8 <= len(data) {
		length := int(binary.BigEndian.Uint32(data[position:]))
		if length < 8 || position+length > len(data) {
			return items
		}
		if string(data[position+4:position+8]) == "infe" {
			entry := data[position+8 : position+length]
			if len(entry) >= 4 {
				version := entry[0]
				switch {
				case version == 2 && len(entry) >= 12:
					id := uint32(binary.BigEndian.Uint16(entry[4:]))
					items[id] = heifItem{id: id, kind: string(entry[8:12])}
				case version >= 3 && len(entry) >= 14:
					id := binary.BigEndian.Uint32(entry[4:])
					items[id] = heifItem{id: id, kind: string(entry[10:14])}
				}
			}
		}
		position += length
	}
	return items
}

func parseItemLocations(data []byte) map[uint32]heifItem {
	locations := make(map[uint32]heifItem)
	if len(data) < 8 {
		return locations
	}
	version := data[0]
	offsetSize := int(data[4] >> 4)
	lengthSize := int(data[4] & 0x0F)
	baseSize := int(data[5] >> 4)
	indexSize := 0
	if version == 1 || version == 2 {
		indexSize = int(data[5] & 0x0F)
	}
	position := 6
	var count uint32
	if version < 2 {
		count = uint32(binary.BigEndian.Uint16(data[position:]))
		position += 2
	} else {
		if len(data) < 10 {
			return locations
		}
		count = binary.BigEndian.Uint32(data[position:])
		position += 4
	}
	readSized := func(width int) (uint64, bool) {
		if width == 0 {
			return 0, true
		}
		if width != 4 && width != 8 || position+width > len(data) {
			return 0, false
		}
		var value uint64
		if width == 4 {
			value = uint64(binary.BigEndian.Uint32(data[position:]))
		} else {
			value = binary.BigEndian.Uint64(data[position:])
		}
		position += width
		return value, true
	}
	for index := uint32(0); index < count && index < 10000; index++ {
		var item heifItem
		if version < 2 {
			if position+2 > len(data) {
				return locations
			}
			item.id = uint32(binary.BigEndian.Uint16(data[position:]))
			position += 2
		} else {
			if position+4 > len(data) {
				return locations
			}
			item.id = binary.BigEndian.Uint32(data[position:])
			position += 4
		}
		if version == 1 || version == 2 {
			if position+2 > len(data) {
				return locations
			}
			item.method = binary.BigEndian.Uint16(data[position:]) & 0x0F
			position += 2
		}
		position += 2 // data_reference_index
		base, ok := readSized(baseSize)
		if !ok || position+2 > len(data) {
			return locations
		}
		item.base = base
		extents := int(binary.BigEndian.Uint16(data[position:]))
		position += 2
		for extent := 0; extent < extents; extent++ {
			if _, ok := readSized(indexSize); !ok {
				return locations
			}
			offset, okOffset := readSized(offsetSize)
			length, okLength := readSized(lengthSize)
			if !okOffset || !okLength {
				return locations
			}
			if extent < 16 {
				item.extent = append(item.extent, [2]uint64{offset, length})
			}
		}
		locations[item.id] = item
	}
	return locations
}

func readItem(r io.ReaderAt, size int64, item heifItem, idat []byte, limit int) []byte {
	var out []byte
	for _, extent := range item.extent {
		offset := item.base + extent[0]
		length := extent[1]
		if length == 0 || length > uint64(limit) || len(out)+int(length) > limit {
			return out
		}
		switch item.method {
		case 0:
			if offset+length > uint64(size) {
				return out
			}
			chunk := make([]byte, length)
			if _, err := r.ReadAt(chunk, int64(offset)); err != nil && !errors.Is(err, io.EOF) {
				return out
			}
			out = append(out, chunk...)
		case 1:
			if offset+length > uint64(len(idat)) {
				return out
			}
			out = append(out, idat[offset:offset+length]...)
		default:
			return out
		}
	}
	return out
}

// applyItemProperties finds the primary item's ispe (size) and irot (rotation).
func applyItemProperties(iprp map[string][]byte, primary uint32, meta *Metadata) {
	ipco, ok := iprp["ipco"]
	if !ok {
		return
	}
	type property struct {
		kind string
		data []byte
	}
	var properties []property
	for position := 0; position+8 <= len(ipco) && len(properties) < 4096; {
		length := int(binary.BigEndian.Uint32(ipco[position:]))
		if length < 8 || position+length > len(ipco) {
			break
		}
		properties = append(properties, property{kind: string(ipco[position+4 : position+8]), data: ipco[position+8 : position+length]})
		position += length
	}
	ipma, ok := iprp["ipma"]
	if !ok || len(ipma) < 8 {
		return
	}
	version, flags := ipma[0], ipma[3]
	count := binary.BigEndian.Uint32(ipma[4:])
	position := 8
	rotation := 0
	for entry := uint32(0); entry < count && entry < 100000; entry++ {
		var id uint32
		if version < 1 {
			if position+2 > len(ipma) {
				return
			}
			id = uint32(binary.BigEndian.Uint16(ipma[position:]))
			position += 2
		} else {
			if position+4 > len(ipma) {
				return
			}
			id = binary.BigEndian.Uint32(ipma[position:])
			position += 4
		}
		if position+1 > len(ipma) {
			return
		}
		associations := int(ipma[position])
		position++
		for association := 0; association < associations; association++ {
			var index int
			if flags&1 == 1 {
				if position+2 > len(ipma) {
					return
				}
				index = int(binary.BigEndian.Uint16(ipma[position:]) & 0x7FFF)
				position += 2
			} else {
				if position+1 > len(ipma) {
					return
				}
				index = int(ipma[position] & 0x7F)
				position++
			}
			if id != primary || index < 1 || index > len(properties) {
				continue
			}
			value := properties[index-1]
			switch value.kind {
			case "ispe":
				if len(value.data) >= 12 {
					meta.Width = int(binary.BigEndian.Uint32(value.data[4:]))
					meta.Height = int(binary.BigEndian.Uint32(value.data[8:]))
				}
			case "irot":
				if len(value.data) >= 1 {
					rotation = int(value.data[0] & 3)
				}
			}
		}
	}
	if rotation == 1 || rotation == 3 {
		meta.Width, meta.Height = meta.Height, meta.Width
	}
}

// ---------------------------------------------------------------- QuickTime

var iso6709 = regexp.MustCompile(`^([+-]\d{1,2}(?:\.\d+)?)([+-]\d{1,3}(?:\.\d+)?)([+-]\d+(?:\.\d+)?)?`)

// parseQuickTime reads mvhd (duration, creation time), the video track's
// tkhd (size and rotation) and Apple's mdta metadata keys from moov.
func parseQuickTime(r io.ReaderAt, size int64, meta *Metadata, location *time.Location) error {
	top, err := walkBoxes(r, 0, size, size)
	if err != nil && len(top) == 0 {
		return err
	}
	moov, ok := findBox(top, "moov")
	if !ok {
		return errors.New("moov box missing")
	}
	children, err := walkBoxes(r, moov.dataStart, moov.end, size)
	if err != nil && len(children) == 0 {
		return err
	}
	for _, child := range children {
		switch child.kind {
		case "mvhd":
			data, err := readBox(r, child, size, 4096)
			if err == nil {
				parseMovieHeader(data, meta)
			}
		case "trak":
			parseTrack(r, child, size, meta)
		case "meta":
			data, err := readBox(r, child, size, maxBoxRead)
			if err == nil {
				parseQuickTimeMeta(data, meta, location)
			}
		case "udta":
			data, err := readBox(r, child, size, maxBoxRead)
			if err == nil {
				parseUserData(data, meta, location)
			}
		}
	}
	return nil
}

var quickTimeEpoch = time.Date(1904, 1, 1, 0, 0, 0, 0, time.UTC)

func parseMovieHeader(data []byte, meta *Metadata) {
	if len(data) < 20 {
		return
	}
	var created, timescale, duration uint64
	if data[0] == 1 {
		if len(data) < 32 {
			return
		}
		created = binary.BigEndian.Uint64(data[4:])
		timescale = uint64(binary.BigEndian.Uint32(data[20:]))
		duration = binary.BigEndian.Uint64(data[24:])
	} else {
		created = uint64(binary.BigEndian.Uint32(data[4:]))
		timescale = uint64(binary.BigEndian.Uint32(data[12:]))
		duration = uint64(binary.BigEndian.Uint32(data[16:]))
	}
	if timescale > 0 && duration < math.MaxInt64/1000 {
		meta.DurationMS = int64(duration * 1000 / timescale)
	}
	// Apple writes com.apple.quicktime.creationdate with the local offset; the
	// header time (UTC) is only a fallback.
	if meta.CapturedAt == nil && created > 0 && created < 1<<40 {
		instant := quickTimeEpoch.Add(time.Duration(created) * time.Second)
		meta.CapturedAt = &instant
	}
}

func parseTrack(r io.ReaderAt, trak box, size int64, meta *Metadata) {
	children, _ := walkBoxes(r, trak.dataStart, trak.end, size)
	tkhd, ok := findBox(children, "tkhd")
	if !ok {
		return
	}
	data, err := readBox(r, tkhd, size, 4096)
	if err != nil || len(data) < 84 {
		return
	}
	offset := 4 + 20 // v0: times, track id, reserved, duration
	if data[0] == 1 {
		offset = 4 + 32
	}
	offset += 8 + 2 + 2 + 2 + 2 // reserved, layer, alternate group, volume, reserved
	if offset+36+8 > len(data) {
		return
	}
	matrix := data[offset : offset+36]
	width := int(binary.BigEndian.Uint32(data[offset+36:]) >> 16)
	height := int(binary.BigEndian.Uint32(data[offset+40:]) >> 16)
	if width == 0 || height == 0 || meta.Width != 0 {
		return
	}
	a := int32(binary.BigEndian.Uint32(matrix[0:]))
	b := int32(binary.BigEndian.Uint32(matrix[4:]))
	if a == 0 && b != 0 {
		width, height = height, width
	}
	meta.Width, meta.Height = width, height
}

// parseQuickTimeMeta reads an mdta "meta" atom: hdlr, keys, then ilst whose
// item types are 1-based indexes into keys.
func parseQuickTimeMeta(data []byte, meta *Metadata, location *time.Location) {
	// QuickTime's meta atom has no version/flags, ISO's does; detect which.
	if len(data) >= 12 && string(data[4:8]) != "hdlr" && string(data[8:12]) == "hdlr" {
		data = data[4:]
	}
	children := parseChildren(data)
	keysData := children["keys"]
	if len(keysData) < 8 {
		return
	}
	count := int(binary.BigEndian.Uint32(keysData[4:]))
	keys := make([]string, 0, min(count, 512))
	for position := 8; position+8 <= len(keysData) && len(keys) < count && len(keys) < 512; {
		length := int(binary.BigEndian.Uint32(keysData[position:]))
		if length < 8 || position+length > len(keysData) {
			break
		}
		keys = append(keys, string(keysData[position+8:position+length]))
		position += length
	}
	ilst := children["ilst"]
	values := make(map[string]string)
	for position := 0; position+8 <= len(ilst); {
		length := int(binary.BigEndian.Uint32(ilst[position:]))
		if length < 8 || position+length > len(ilst) {
			break
		}
		index := int(binary.BigEndian.Uint32(ilst[position+4:]))
		item := parseChildren(ilst[position+8 : position+length])
		if value, ok := item["data"]; ok && index >= 1 && index <= len(keys) && len(value) >= 8 {
			if binary.BigEndian.Uint32(value) == 1 { // UTF-8
				values[keys[index-1]] = string(value[8:])
			}
		}
		position += length
	}
	applyAppleKeys(values, meta, location)
}

func applyAppleKeys(values map[string]string, meta *Metadata, location *time.Location) {
	if created := values["com.apple.quicktime.creationdate"]; created != "" {
		if parsed, wall := parseISODate(created, location); parsed != nil {
			meta.CapturedAt, meta.LocalTime = parsed, wall
		}
	}
	if id := values["com.apple.quicktime.content.identifier"]; id != "" {
		meta.LivePhotoID = id
	}
	if point := values["com.apple.quicktime.location.ISO6709"]; point != "" {
		applyISO6709(point, meta)
	}
	meta.Make = firstNonEmpty(meta.Make, values["com.apple.quicktime.make"])
	meta.Model = firstNonEmpty(meta.Model, values["com.apple.quicktime.model"])
	meta.Software = firstNonEmpty(meta.Software, values["com.apple.quicktime.software"])
	meta.Lens = firstNonEmpty(meta.Lens, values["com.apple.quicktime.camera.lens_model"])
	if strings.EqualFold(values["com.apple.quicktime.camera.identifier"], "Front") {
		meta.frontCamera = true
	}
}

// parseUserData reads the classic udta atoms other cameras write: ©xyz
// (ISO 6709 location), ©day (date), ©mak/©mod (camera).
func parseUserData(data []byte, meta *Metadata, location *time.Location) {
	children := parseChildren(data)
	text := func(kind string) string {
		value, ok := children[kind]
		if !ok || len(value) < 4 {
			return ""
		}
		length := int(binary.BigEndian.Uint16(value))
		if 4+length > len(value) {
			return string(value[4:])
		}
		return string(value[4 : 4+length])
	}
	if point := text("\xa9xyz"); point != "" && meta.Latitude == nil {
		applyISO6709(point, meta)
	}
	if day := text("\xa9day"); day != "" && meta.LocalTime == "" {
		if parsed, wall := parseISODate(day, location); parsed != nil {
			meta.CapturedAt, meta.LocalTime = parsed, wall
		}
	}
	meta.Make = firstNonEmpty(meta.Make, text("\xa9mak"))
	meta.Model = firstNonEmpty(meta.Model, text("\xa9mod"))
}

func applyISO6709(point string, meta *Metadata) {
	match := iso6709.FindStringSubmatch(strings.TrimSpace(point))
	if match == nil {
		return
	}
	lat, errLat := strconv.ParseFloat(match[1], 64)
	lon, errLon := strconv.ParseFloat(match[2], 64)
	if errLat != nil || errLon != nil {
		return
	}
	meta.Latitude, meta.Longitude = &lat, &lon
	if match[3] != "" {
		if altitude, err := strconv.ParseFloat(match[3], 64); err == nil {
			meta.Altitude = &altitude
		}
	}
}

// parseISODate accepts Apple's "2006-01-02T15:04:05-0700" and RFC 3339.
func parseISODate(value string, location *time.Location) (*time.Time, string) {
	value = strings.TrimSpace(strings.TrimRight(value, "\x00"))
	for _, layout := range []string{"2006-01-02T15:04:05-0700", time.RFC3339, "2006-01-02T15:04:05Z0700", "2006-01-02T15:04:05.000-0700"} {
		if parsed, err := time.Parse(layout, value); err == nil {
			utc := parsed.UTC()
			return &utc, parsed.Format("2006-01-02T15:04:05")
		}
	}
	if len(value) >= 19 {
		if parsed, err := time.ParseInLocation("2006-01-02T15:04:05", value[:19], location); err == nil {
			utc := parsed.UTC()
			return &utc, parsed.Format("2006-01-02T15:04:05")
		}
	}
	return nil, ""
}
