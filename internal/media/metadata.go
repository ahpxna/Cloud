// Package media reads capture metadata (date taken, dimensions, duration,
// camera, location, Live Photo identity) from the photo and video formats an
// iPhone and common cameras produce: JPEG, HEIC/HEIF/AVIF, PNG, TIFF/DNG and
// QuickTime/MP4. It only parses headers and metadata boxes with bounded reads;
// pixels are never decoded and originals are never modified.
package media

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"math"
	"strings"
	"time"
)

// Metadata is what the library needs to sort, group and describe an item.
// Zero values mean "not present in the file".
type Metadata struct {
	CapturedAt *time.Time `json:"-"`
	// LocalTime is the wall-clock capture time exactly as the camera wrote it.
	LocalTime   string   `json:"local_time,omitempty"`
	Width       int      `json:"-"`
	Height      int      `json:"-"`
	DurationMS  int64    `json:"-"`
	Make        string   `json:"make,omitempty"`
	Model       string   `json:"model,omitempty"`
	Lens        string   `json:"lens,omitempty"`
	Software    string   `json:"software,omitempty"`
	FocalLength string   `json:"focal_length,omitempty"`
	Aperture    string   `json:"aperture,omitempty"`
	Exposure    string   `json:"exposure,omitempty"`
	ISO         int      `json:"iso,omitempty"`
	LivePhotoID string   `json:"-"`
	Latitude    *float64 `json:"-"`
	Longitude   *float64 `json:"-"`
	Altitude    *float64 `json:"altitude,omitempty"`
	// Subtype is "screenshot", "selfie", "panorama" or empty.
	Subtype string `json:"-"`
	// Orientation is the EXIF orientation (1-8); Width/Height are already the
	// displayed (rotated) dimensions.
	Orientation int `json:"orientation,omitempty"`

	userComment string
	// sizeOriented is set when the container already applied rotation (HEIF
	// irot), so EXIF orientation must not swap the size a second time.
	sizeOriented bool
	frontCamera  bool
	isPNG        bool
	isVideo      bool
}

// ErrUnsupported means the leading bytes are not a recognised format.
var ErrUnsupported = errors.New("unsupported media format")

const (
	maxHeaderRead = 1 << 20 // JPEG/PNG metadata lives in the first MiB
	maxBoxRead    = 4 << 20 // largest metadata box read into memory
	maxExifRead   = 256 << 10
)

// Extract reads metadata from the first bytes and metadata boxes of a file.
// size may be smaller than the declared file size for a partial upload; any
// structure beyond it is simply not found. location interprets capture times
// that carry no UTC offset (nil means UTC).
func Extract(r io.ReaderAt, size int64, location *time.Location) (meta Metadata, err error) {
	// Uploads come from family devices, but the parsers must never take the
	// server down on a malformed file.
	defer func() {
		if recovered := recover(); recovered != nil {
			meta, err = Metadata{}, fmt.Errorf("parse media metadata: %v", recovered)
		}
	}()
	if location == nil {
		location = time.UTC
	}
	head := make([]byte, min64(size, 64))
	if _, err := r.ReadAt(head, 0); err != nil && !errors.Is(err, io.EOF) {
		return Metadata{}, err
	}
	switch {
	case bytes.HasPrefix(head, []byte{0xFF, 0xD8, 0xFF}):
		err = parseJPEG(r, size, &meta, location)
	case bytes.HasPrefix(head, []byte("\x89PNG\r\n\x1a\n")):
		err = parsePNG(r, size, &meta, location)
	case bytes.HasPrefix(head, []byte("II*\x00")) || bytes.HasPrefix(head, []byte("MM\x00*")):
		data := make([]byte, min64(size, maxHeaderRead))
		if _, readErr := r.ReadAt(data, 0); readErr != nil && !errors.Is(readErr, io.EOF) {
			return Metadata{}, readErr
		}
		err = parseTIFF(data, &meta, location, true)
	case len(head) >= 12 && string(head[4:8]) == "ftyp":
		brand := string(head[8:12])
		compatible := head[min(len(head), 16):]
		if isHEIFBrand(brand) || bytes.Contains(compatible, []byte("heic")) || bytes.Contains(compatible, []byte("mif1")) {
			err = parseHEIF(r, size, &meta, location)
		} else {
			meta.isVideo = true
			err = parseQuickTime(r, size, &meta, location)
		}
	case len(head) >= 8 && (string(head[4:8]) == "moov" || string(head[4:8]) == "wide" || string(head[4:8]) == "mdat" || string(head[4:8]) == "free"):
		meta.isVideo = true
		err = parseQuickTime(r, size, &meta, location)
	default:
		return Metadata{}, ErrUnsupported
	}
	meta.finish()
	return meta, err
}

func isHEIFBrand(brand string) bool {
	switch brand {
	case "heic", "heix", "hevc", "hevx", "heim", "heis", "mif1", "msf1", "avif", "avis":
		return true
	}
	return false
}

// finish derives the subtype and normalises dimensions after parsing.
func (m *Metadata) finish() {
	if m.Orientation >= 5 && m.Orientation <= 8 && !m.sizeOriented {
		m.Width, m.Height = m.Height, m.Width
	}
	m.Make = cleanText(m.Make, 64)
	m.Model = cleanText(m.Model, 64)
	m.Lens = cleanText(m.Lens, 128)
	m.Software = cleanText(m.Software, 64)
	m.LivePhotoID = cleanText(m.LivePhotoID, 128)
	switch {
	case strings.Contains(strings.ToLower(m.userComment), "screenshot"):
		m.Subtype = "screenshot"
	case m.isPNG && m.Make == "":
		m.Subtype = "screenshot"
	case m.frontCamera || strings.Contains(strings.ToLower(m.Lens), "front"):
		m.Subtype = "selfie"
	case !m.isVideo && m.Make != "" && m.Width > 0 && m.Height > 0 &&
		float64(max(m.Width, m.Height))/float64(min(m.Width, m.Height)) >= 2.4:
		m.Subtype = "panorama"
	}
	if m.CapturedAt != nil {
		year := m.CapturedAt.Year()
		if year < 1900 || m.CapturedAt.After(time.Now().Add(48*time.Hour)) {
			m.CapturedAt = nil
			m.LocalTime = ""
		}
	}
	if m.Latitude != nil && m.Longitude != nil {
		lat, lon := *m.Latitude, *m.Longitude
		if math.IsNaN(lat) || math.IsNaN(lon) || lat < -90 || lat > 90 || lon < -180 || lon > 180 || (lat == 0 && lon == 0) {
			m.Latitude, m.Longitude = nil, nil
		}
	} else {
		m.Latitude, m.Longitude = nil, nil
	}
	if m.Width < 0 || m.Height < 0 || m.Width > 1<<17 || m.Height > 1<<17 {
		m.Width, m.Height = 0, 0
	}
	if m.DurationMS < 0 || m.DurationMS > 100*24*3600*1000 {
		m.DurationMS = 0
	}
}

// cleanText trims NULs and whitespace and drops control characters.
func cleanText(value string, limit int) string {
	value = strings.TrimRight(value, "\x00 ")
	value = strings.TrimSpace(value)
	var builder strings.Builder
	for _, r := range value {
		if r < 0x20 || r == 0x7f || r == 0xFFFD {
			continue
		}
		builder.WriteRune(r)
		if builder.Len() >= limit {
			break
		}
	}
	return builder.String()
}

// parseCaptureTime parses an EXIF "2006:01:02 15:04:05" time with an optional
// "+07:00" offset.
func parseCaptureTime(value, offset string, location *time.Location) (*time.Time, string) {
	value = strings.TrimSpace(strings.TrimRight(value, "\x00"))
	if len(value) < 19 {
		return nil, ""
	}
	local, err := time.ParseInLocation("2006:01:02 15:04:05", value[:19], time.UTC)
	if err != nil {
		local, err = time.ParseInLocation("2006-01-02T15:04:05", value[:19], time.UTC)
		if err != nil {
			return nil, ""
		}
	}
	wall := local.Format("2006-01-02T15:04:05")
	offset = strings.TrimSpace(strings.TrimRight(offset, "\x00"))
	if parsed, err := time.Parse("-07:00", offset); err == nil && offset != "" {
		_, seconds := parsed.Zone()
		instant := time.Date(local.Year(), local.Month(), local.Day(), local.Hour(), local.Minute(), local.Second(), 0, time.FixedZone("", seconds)).UTC()
		return &instant, wall
	}
	instant := time.Date(local.Year(), local.Month(), local.Day(), local.Hour(), local.Minute(), local.Second(), 0, location).UTC()
	return &instant, wall
}

func min64(a, b int64) int64 {
	if a < b {
		return a
	}
	return b
}
