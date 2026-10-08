package media

import (
	"bytes"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"math"
	"sort"
)

// A Clip is a lossless cut of a QuickTime/MP4 video: the samples from the
// keyframe at or before the start through the end are copied unchanged into a
// new file, and an edit list makes playback begin exactly at the start. No
// frame is decoded or re-encoded, so quality is identical to the original.
type Clip struct {
	source     io.ReaderAt
	ftyp       []byte
	moov       []byte
	mdatHeader []byte
	ranges     []byteRange // source byte ranges, in output order
	size       int64
	duration   float64
}

type byteRange struct {
	offset int64
	length int64
}

// ErrClipUnsupported is returned for layouts the cutter does not handle
// (fragmented MP4, complex edit lists, missing sample tables).
var ErrClipUnsupported = errors.New("video layout is not supported for cutting")

const maxMoovSize = 64 << 20

// Size is the exact byte length of the clip file.
func (c *Clip) Size() int64 { return c.size }

// Duration is the clip length in seconds.
func (c *Clip) Duration() float64 { return c.duration }

// WriteTo streams the clip.
func (c *Clip) WriteTo(w io.Writer) (int64, error) {
	var written int64
	for _, part := range [][]byte{c.ftyp, c.moov, c.mdatHeader} {
		n, err := w.Write(part)
		written += int64(n)
		if err != nil {
			return written, err
		}
	}
	for _, part := range c.ranges {
		n, err := io.Copy(w, io.NewSectionReader(c.source, part.offset, part.length))
		written += n
		if err != nil {
			return written, err
		}
		if n != part.length {
			return written, io.ErrUnexpectedEOF
		}
	}
	return written, nil
}

// ---------------------------------------------------------------- box tree

type node struct {
	kind     string
	payload  []byte // leaf payload, or the version/flags prefix of a container
	children []*node
}

var containerKinds = map[string]bool{
	"moov": true, "trak": true, "mdia": true, "minf": true, "stbl": true, "edts": true, "dinf": true,
}

func parseTree(data []byte, depth int) ([]*node, error) {
	if depth > 8 {
		return nil, ErrClipUnsupported
	}
	var nodes []*node
	for position := 0; position < len(data); {
		if position+8 > len(data) {
			return nil, ErrClipUnsupported
		}
		length := int(binary.BigEndian.Uint32(data[position:]))
		kind := string(data[position+4 : position+8])
		header := 8
		if length == 1 {
			if position+16 > len(data) {
				return nil, ErrClipUnsupported
			}
			large := binary.BigEndian.Uint64(data[position+8:])
			if large > uint64(len(data)) {
				return nil, ErrClipUnsupported
			}
			length, header = int(large), 16
		} else if length == 0 {
			length = len(data) - position
		}
		if length < header || position+length > len(data) {
			return nil, ErrClipUnsupported
		}
		body := data[position+header : position+length]
		item := &node{kind: kind}
		if containerKinds[kind] {
			children, err := parseTree(body, depth+1)
			if err != nil {
				return nil, err
			}
			item.children = children
		} else {
			item.payload = body
		}
		nodes = append(nodes, item)
		position += length
	}
	return nodes, nil
}

func (n *node) child(kind string) *node {
	for _, candidate := range n.children {
		if candidate.kind == kind {
			return candidate
		}
	}
	return nil
}

func (n *node) path(kinds ...string) *node {
	current := n
	for _, kind := range kinds {
		if current = current.child(kind); current == nil {
			return nil
		}
	}
	return current
}

func (n *node) encode(out *bytes.Buffer) {
	var body bytes.Buffer
	body.Write(n.payload)
	for _, child := range n.children {
		child.encode(&body)
	}
	length := body.Len() + 8
	if length > math.MaxUint32 {
		_ = binary.Write(out, binary.BigEndian, uint32(1))
		out.WriteString(n.kind)
		_ = binary.Write(out, binary.BigEndian, uint64(length+8))
	} else {
		_ = binary.Write(out, binary.BigEndian, uint32(length))
		out.WriteString(n.kind)
	}
	out.Write(body.Bytes())
}

// ---------------------------------------------------------------- tracks

type sample struct {
	offset   int64
	size     uint32
	dts      int64
	duration uint32
	cto      int32 // composition time offset
	sync     bool
}

type track struct {
	trak      *node
	handler   string
	timescale uint32
	samples   []sample
	// Source edit: an optional leading empty edit, then one normal edit.
	emptyMovie int64 // movie-timescale delay before media starts
	mediaStart int64 // media time shown at the start of the normal edit
	cttsV1     bool
	hasCTTS    bool
	hasSync    bool
	enabled    bool
}

// PlanClip prepares a cut of [start, end) seconds of a QuickTime/MP4 file.
func PlanClip(r io.ReaderAt, size int64, start, end float64) (*Clip, error) {
	if math.IsNaN(start) || math.IsNaN(end) || start < 0 || end <= start {
		return nil, errors.New("clip range is invalid")
	}
	top, err := walkBoxes(r, 0, size, size)
	if err != nil && len(top) == 0 {
		return nil, err
	}
	ftypBox, okFtyp := findBox(top, "ftyp")
	moovBox, okMoov := findBox(top, "moov")
	if !okFtyp || !okMoov || moovBox.end > size {
		return nil, ErrClipUnsupported
	}
	if _, fragmented := findBox(top, "moof"); fragmented {
		return nil, ErrClipUnsupported
	}
	ftyp := make([]byte, ftypBox.end-ftypBox.start)
	if _, err := r.ReadAt(ftyp, ftypBox.start); err != nil && !errors.Is(err, io.EOF) {
		return nil, err
	}
	if moovBox.end-moovBox.dataStart > maxMoovSize {
		return nil, ErrClipUnsupported
	}
	moovData := make([]byte, moovBox.end-moovBox.dataStart)
	if _, err := r.ReadAt(moovData, moovBox.dataStart); err != nil && !errors.Is(err, io.EOF) {
		return nil, err
	}
	children, err := parseTree(moovData, 1)
	if err != nil {
		return nil, err
	}
	moov := &node{kind: "moov", children: children}
	if moov.child("mvex") != nil {
		return nil, ErrClipUnsupported
	}
	mvhd := moov.child("mvhd")
	if mvhd == nil || len(mvhd.payload) < 20 {
		return nil, ErrClipUnsupported
	}
	movieScale, movieDuration := movieHeaderTiming(mvhd.payload)
	if movieScale == 0 {
		return nil, ErrClipUnsupported
	}
	totalSeconds := float64(movieDuration) / float64(movieScale)
	if end > totalSeconds {
		end = totalSeconds
	}
	if end-start < 0.1 {
		return nil, errors.New("clip range is outside the video")
	}

	var kept []*track
	videoTracks := 0
	for _, item := range moov.children {
		if item.kind != "trak" {
			continue
		}
		parsed, err := parseClipTrack(item, movieScale, size)
		if err != nil {
			return nil, err
		}
		if !parsed.enabled || len(parsed.samples) == 0 {
			continue
		}
		switch parsed.handler {
		case "vide":
			// Keep the main picture; Cinematic-mode depth tracks are extra.
			if videoTracks == 0 {
				kept = append(kept, parsed)
			}
			videoTracks++
		case "soun":
			kept = append(kept, parsed)
		}
	}
	if videoTracks == 0 {
		return nil, ErrClipUnsupported
	}

	var chunks []clipChunk
	segmentDuration := uint64(math.Round((end - start) * float64(movieScale)))
	var newTraks []*node
	for _, current := range kept {
		first, last, mediaStart, err := current.selectRange(start, end, movieScale)
		if err != nil {
			return nil, err
		}
		selected := current.samples[first : last+1]
		base := selected[0].dts
		// One chunk per ~0.5 s, ordered by presentation time across tracks,
		// so audio and video interleave for smooth playback.
		var pending []sample
		chunkStart := selected[0].dts
		flush := func() {
			chunks = append(chunks, clipChunk{track: current, samples: pending, start: float64(chunkStart-mediaStart) / float64(current.timescale)})
		}
		for _, item := range selected {
			if len(pending) > 0 && float64(item.dts-chunkStart)/float64(current.timescale) >= 0.5 {
				flush()
				pending, chunkStart = nil, item.dts
			}
			pending = append(pending, item)
		}
		flush()
		current.samples = selected
		newTraks = append(newTraks, current.rebuild(segmentDuration, mediaStart-base))
	}
	sort.SliceStable(chunks, func(i, j int) bool { return chunks[i].start < chunks[j].start })

	// Lay out mdat: chunk offsets are known once moov's size is known, and
	// co64 entries are fixed-width, so build moov twice.
	var mdatSize int64
	relative := make([]int64, len(chunks))
	for index, item := range chunks {
		relative[index] = mdatSize
		for _, s := range item.samples {
			mdatSize += int64(s.size)
		}
	}
	buildMoov := func(dataStart int64) []byte {
		offsets := make(map[*track][]uint64)
		counts := make(map[*track][]int)
		for index, item := range chunks {
			offsets[item.track] = append(offsets[item.track], uint64(dataStart+relative[index]))
			counts[item.track] = append(counts[item.track], len(item.samples))
		}
		out := &node{kind: "moov"}
		for _, item := range moov.children {
			switch item.kind {
			case "trak":
				continue
			case "mvhd":
				out.children = append(out.children, &node{kind: "mvhd", payload: withMovieDuration(item.payload, segmentDuration)})
			case "meta":
				out.children = append(out.children, &node{kind: "meta", payload: withoutLivePhotoKey(item.payload)})
			default:
				out.children = append(out.children, item)
			}
		}
		for index, current := range kept {
			out.children = append(out.children, withSampleTables(newTraks[index], sampleToChunk(counts[current]), chunkOffsetBox(offsets[current])))
		}
		var buffer bytes.Buffer
		out.encode(&buffer)
		return buffer.Bytes()
	}
	mdatHeaderSize := int64(8)
	if mdatSize+8 > math.MaxUint32 {
		mdatHeaderSize = 16
	}
	draft := buildMoov(0)
	dataStart := int64(len(ftyp)) + int64(len(draft)) + mdatHeaderSize
	final := buildMoov(dataStart)
	if len(final) != len(draft) {
		return nil, fmt.Errorf("clip layout changed size: %d != %d", len(final), len(draft))
	}
	var header bytes.Buffer
	if mdatHeaderSize == 16 {
		_ = binary.Write(&header, binary.BigEndian, uint32(1))
		header.WriteString("mdat")
		_ = binary.Write(&header, binary.BigEndian, uint64(mdatSize+16))
	} else {
		_ = binary.Write(&header, binary.BigEndian, uint32(mdatSize+8))
		header.WriteString("mdat")
	}
	clip := &Clip{source: r, ftyp: ftyp, moov: final, mdatHeader: header.Bytes(), duration: float64(segmentDuration) / float64(movieScale)}
	for _, item := range chunks {
		for _, s := range item.samples {
			if count := len(clip.ranges); count > 0 && clip.ranges[count-1].offset+clip.ranges[count-1].length == s.offset {
				clip.ranges[count-1].length += int64(s.size)
				continue
			}
			clip.ranges = append(clip.ranges, byteRange{offset: s.offset, length: int64(s.size)})
		}
	}
	clip.size = int64(len(ftyp)) + int64(len(final)) + mdatHeaderSize + mdatSize
	return clip, nil
}

type clipChunk struct {
	track   *track
	samples []sample
	start   float64 // seconds relative to the clip start
}

// withSampleTables returns a copy of trak whose stbl also has stsc and co64.
func withSampleTables(trak *node, extra ...*node) *node {
	copyPath := func(parent *node, kind string, replace func(*node) *node) *node {
		clone := &node{kind: parent.kind, payload: parent.payload}
		for _, child := range parent.children {
			if child.kind == kind {
				clone.children = append(clone.children, replace(child))
			} else {
				clone.children = append(clone.children, child)
			}
		}
		return clone
	}
	return copyPath(trak, "mdia", func(mdia *node) *node {
		return copyPath(mdia, "minf", func(minf *node) *node {
			return copyPath(minf, "stbl", func(stbl *node) *node {
				clone := &node{kind: "stbl", children: append(append([]*node(nil), stbl.children...), extra...)}
				return clone
			})
		})
	})
}

func movieHeaderTiming(payload []byte) (uint32, uint64) {
	if payload[0] == 1 {
		if len(payload) < 32 {
			return 0, 0
		}
		return binary.BigEndian.Uint32(payload[20:]), binary.BigEndian.Uint64(payload[24:])
	}
	return binary.BigEndian.Uint32(payload[12:]), uint64(binary.BigEndian.Uint32(payload[16:]))
}

func withMovieDuration(payload []byte, duration uint64) []byte {
	out := append([]byte(nil), payload...)
	if out[0] == 1 {
		binary.BigEndian.PutUint64(out[24:], duration)
	} else {
		binary.BigEndian.PutUint32(out[16:], uint32(min(duration, math.MaxUint32)))
	}
	return out
}

func parseClipTrack(trak *node, movieScale uint32, fileSize int64) (*track, error) {
	result := &track{trak: trak}
	tkhd := trak.child("tkhd")
	if tkhd == nil || len(tkhd.payload) < 4 {
		return nil, ErrClipUnsupported
	}
	result.enabled = tkhd.payload[3]&1 == 1
	hdlr := trak.path("mdia", "hdlr")
	if hdlr == nil || len(hdlr.payload) < 12 {
		return nil, ErrClipUnsupported
	}
	result.handler = string(hdlr.payload[8:12])
	if result.handler != "vide" && result.handler != "soun" {
		return result, nil
	}
	mdhd := trak.path("mdia", "mdhd")
	if mdhd == nil || len(mdhd.payload) < 20 {
		return nil, ErrClipUnsupported
	}
	result.timescale, _ = movieHeaderTiming(mdhd.payload)
	if result.timescale == 0 {
		return nil, ErrClipUnsupported
	}
	stbl := trak.path("mdia", "minf", "stbl")
	if stbl == nil {
		return nil, ErrClipUnsupported
	}
	if err := result.readSamples(stbl, fileSize); err != nil {
		return nil, err
	}
	if err := result.readEdits(trak.path("edts", "elst"), movieScale); err != nil {
		return nil, err
	}
	return result, nil
}

func fullBoxEntries(item *node, entrySize int, header int) ([]byte, int, error) {
	if item == nil || len(item.payload) < header {
		return nil, 0, ErrClipUnsupported
	}
	count := int(binary.BigEndian.Uint32(item.payload[header-4:]))
	if count < 0 || count > (len(item.payload)-header)/entrySize {
		return nil, 0, ErrClipUnsupported
	}
	return item.payload[header:], count, nil
}

func (t *track) readSamples(stbl *node, fileSize int64) error {
	stsz := stbl.child("stsz")
	if stsz == nil || len(stsz.payload) < 12 {
		return ErrClipUnsupported
	}
	constant := binary.BigEndian.Uint32(stsz.payload[4:])
	count := int(binary.BigEndian.Uint32(stsz.payload[8:]))
	if count <= 0 || count > 10_000_000 || (constant == 0 && count > (len(stsz.payload)-12)/4) {
		return ErrClipUnsupported
	}
	samples := make([]sample, count)
	for index := range samples {
		if constant != 0 {
			samples[index].size = constant
		} else {
			samples[index].size = binary.BigEndian.Uint32(stsz.payload[12+index*4:])
		}
	}

	// Decode times from stts.
	sttsData, sttsCount, err := fullBoxEntries(stbl.child("stts"), 8, 8)
	if err != nil {
		return err
	}
	var dts int64
	index := 0
	for entry := 0; entry < sttsCount && index < count; entry++ {
		run := int(binary.BigEndian.Uint32(sttsData[entry*8:]))
		delta := binary.BigEndian.Uint32(sttsData[entry*8+4:])
		for ; run > 0 && index < count; run-- {
			samples[index].dts, samples[index].duration = dts, delta
			dts += int64(delta)
			index++
		}
	}
	if index != count {
		return ErrClipUnsupported
	}

	// Composition offsets.
	if ctts := stbl.child("ctts"); ctts != nil {
		cttsData, cttsCount, err := fullBoxEntries(ctts, 8, 8)
		if err != nil {
			return err
		}
		t.hasCTTS, t.cttsV1 = true, ctts.payload[0] == 1
		index = 0
		for entry := 0; entry < cttsCount && index < count; entry++ {
			run := int(binary.BigEndian.Uint32(cttsData[entry*8:]))
			offset := int32(binary.BigEndian.Uint32(cttsData[entry*8+4:]))
			for ; run > 0 && index < count; run-- {
				samples[index].cto = offset
				index++
			}
		}
	}

	// Sync samples; without stss every sample is a sync sample.
	if stss := stbl.child("stss"); stss != nil {
		stssData, stssCount, err := fullBoxEntries(stss, 4, 8)
		if err != nil {
			return err
		}
		t.hasSync = true
		for entry := 0; entry < stssCount; entry++ {
			number := int(binary.BigEndian.Uint32(stssData[entry*4:]))
			if number >= 1 && number <= count {
				samples[number-1].sync = true
			}
		}
	} else {
		for index := range samples {
			samples[index].sync = true
		}
	}

	// Chunk offsets and sample-to-chunk give each sample's file offset.
	var offsets []uint64
	if stco := stbl.child("stco"); stco != nil {
		data, chunks, err := fullBoxEntries(stco, 4, 8)
		if err != nil {
			return err
		}
		for entry := 0; entry < chunks; entry++ {
			offsets = append(offsets, uint64(binary.BigEndian.Uint32(data[entry*4:])))
		}
	} else if co64 := stbl.child("co64"); co64 != nil {
		data, chunks, err := fullBoxEntries(co64, 8, 8)
		if err != nil {
			return err
		}
		for entry := 0; entry < chunks; entry++ {
			offsets = append(offsets, binary.BigEndian.Uint64(data[entry*8:]))
		}
	} else {
		return ErrClipUnsupported
	}
	stscData, stscCount, err := fullBoxEntries(stbl.child("stsc"), 12, 8)
	if err != nil || stscCount == 0 {
		return ErrClipUnsupported
	}
	index = 0
	for entry := 0; entry < stscCount; entry++ {
		firstChunk := int(binary.BigEndian.Uint32(stscData[entry*12:]))
		perChunk := int(binary.BigEndian.Uint32(stscData[entry*12+4:]))
		lastChunk := len(offsets)
		if entry+1 < stscCount {
			lastChunk = int(binary.BigEndian.Uint32(stscData[(entry+1)*12:])) - 1
		}
		if firstChunk < 1 || lastChunk > len(offsets) || perChunk < 0 || perChunk > count {
			return ErrClipUnsupported
		}
		for chunkNumber := firstChunk; chunkNumber <= lastChunk && index < count; chunkNumber++ {
			position := offsets[chunkNumber-1]
			for inChunk := 0; inChunk < perChunk && index < count; inChunk++ {
				samples[index].offset = int64(position)
				position += uint64(samples[index].size)
				if position > uint64(fileSize) {
					return ErrClipUnsupported
				}
				index++
			}
		}
	}
	if index != count {
		return ErrClipUnsupported
	}
	t.samples = samples
	return nil
}

// readEdits accepts no edit list, one normal edit, or a leading empty edit
// followed by one normal edit at normal speed.
func (t *track) readEdits(elst *node, movieScale uint32) error {
	if elst == nil {
		return nil
	}
	if len(elst.payload) < 8 {
		return ErrClipUnsupported
	}
	version := elst.payload[0]
	entrySize := 12
	if version == 1 {
		entrySize = 20
	}
	data, count, err := fullBoxEntries(elst, entrySize, 8)
	if err != nil {
		return err
	}
	normal := 0
	for entry := 0; entry < count; entry++ {
		raw := data[entry*entrySize:]
		var duration uint64
		var mediaTime int64
		var rate uint32
		if version == 1 {
			duration, mediaTime, rate = binary.BigEndian.Uint64(raw), int64(binary.BigEndian.Uint64(raw[8:])), binary.BigEndian.Uint32(raw[16:])
		} else {
			duration, mediaTime, rate = uint64(binary.BigEndian.Uint32(raw)), int64(int32(binary.BigEndian.Uint32(raw[4:]))), binary.BigEndian.Uint32(raw[8:])
		}
		switch {
		case mediaTime == -1 && normal == 0:
			t.emptyMovie += int64(duration)
		case mediaTime >= 0 && normal == 0 && rate>>16 == 1:
			t.mediaStart = mediaTime
			normal++
		default:
			return ErrClipUnsupported
		}
	}
	return nil
}

// mediaTime maps a movie time in seconds to this track's media timescale.
func (t *track) mediaTime(seconds float64, movieScale uint32) int64 {
	afterDelay := seconds - float64(t.emptyMovie)/float64(movieScale)
	if afterDelay < 0 {
		afterDelay = 0
	}
	return t.mediaStart + int64(math.Round(afterDelay*float64(t.timescale)))
}

// selectRange picks samples to copy and returns the media time at which
// playback of the clip must start.
func (t *track) selectRange(start, end float64, movieScale uint32) (int, int, int64, error) {
	mediaStart := t.mediaTime(start, movieScale)
	mediaEnd := t.mediaTime(end, movieScale)
	first := -1
	for index, item := range t.samples {
		presentation := item.dts + int64(item.cto)
		if item.sync && presentation <= mediaStart {
			first = index
		}
		if presentation > mediaStart && first >= 0 {
			break
		}
	}
	if first < 0 {
		for index, item := range t.samples {
			if item.sync {
				first = index
				break
			}
		}
	}
	if first < 0 {
		return 0, 0, 0, ErrClipUnsupported
	}
	last := first
	for index := first; index < len(t.samples); index++ {
		if t.samples[index].dts+int64(t.samples[index].cto) < mediaEnd {
			last = index
		}
	}
	if mediaStart < t.samples[first].dts {
		mediaStart = t.samples[first].dts
	}
	return first, last, mediaStart, nil
}

// rebuild returns a new trak for the selected samples, with a single edit
// that starts at editMediaTime. stsc/co64 are appended by the caller.
func (t *track) rebuild(segmentDuration uint64, editMediaTime int64) *node {
	var mediaDuration uint64
	for _, item := range t.samples {
		mediaDuration += uint64(item.duration)
	}
	trak := &node{kind: "trak"}
	for _, child := range t.trak.children {
		switch child.kind {
		case "tkhd":
			trak.children = append(trak.children, &node{kind: "tkhd", payload: withTrackDuration(child.payload, segmentDuration)})
		case "edts", "tref":
			// Replaced below / references tracks that are not kept.
		case "mdia":
			trak.children = append(trak.children, t.rebuildMedia(child, mediaDuration))
		default:
			trak.children = append(trak.children, child)
		}
	}
	elst := make([]byte, 8+20)
	elst[0] = 1
	binary.BigEndian.PutUint32(elst[4:], 1)
	binary.BigEndian.PutUint64(elst[8:], segmentDuration)
	binary.BigEndian.PutUint64(elst[16:], uint64(editMediaTime))
	binary.BigEndian.PutUint32(elst[24:], 1<<16)
	edts := &node{kind: "edts", children: []*node{{kind: "elst", payload: elst}}}
	// edts belongs right after tkhd.
	result := &node{kind: "trak"}
	for _, child := range trak.children {
		result.children = append(result.children, child)
		if child.kind == "tkhd" {
			result.children = append(result.children, edts)
		}
	}
	return result
}

func withTrackDuration(payload []byte, duration uint64) []byte {
	out := append([]byte(nil), payload...)
	if out[0] == 1 && len(out) >= 36 {
		binary.BigEndian.PutUint64(out[28:], duration)
	} else if len(out) >= 24 {
		binary.BigEndian.PutUint32(out[20:], uint32(min(duration, math.MaxUint32)))
	}
	return out
}

func (t *track) rebuildMedia(mdia *node, mediaDuration uint64) *node {
	result := &node{kind: "mdia"}
	for _, child := range mdia.children {
		switch child.kind {
		case "mdhd":
			result.children = append(result.children, &node{kind: "mdhd", payload: withMovieDuration(child.payload, mediaDuration)})
		case "minf":
			minf := &node{kind: "minf"}
			for _, part := range child.children {
				if part.kind != "stbl" {
					minf.children = append(minf.children, part)
					continue
				}
				minf.children = append(minf.children, t.rebuildSampleTable(part))
			}
			result.children = append(result.children, minf)
		default:
			result.children = append(result.children, child)
		}
	}
	return result
}

func (t *track) rebuildSampleTable(stbl *node) *node {
	result := &node{kind: "stbl"}
	if stsd := stbl.child("stsd"); stsd != nil {
		result.children = append(result.children, stsd)
	}
	// stts: run-length encoded durations.
	var stts bytes.Buffer
	runs := 0
	for index := 0; index < len(t.samples); {
		run := 1
		for index+run < len(t.samples) && t.samples[index+run].duration == t.samples[index].duration {
			run++
		}
		_ = binary.Write(&stts, binary.BigEndian, uint32(run))
		_ = binary.Write(&stts, binary.BigEndian, t.samples[index].duration)
		runs++
		index += run
	}
	result.children = append(result.children, &node{kind: "stts", payload: append(fullHeader(0, uint32(runs)), stts.Bytes()...)})
	if t.hasCTTS {
		var ctts bytes.Buffer
		runs = 0
		for index := 0; index < len(t.samples); {
			run := 1
			for index+run < len(t.samples) && t.samples[index+run].cto == t.samples[index].cto {
				run++
			}
			_ = binary.Write(&ctts, binary.BigEndian, uint32(run))
			_ = binary.Write(&ctts, binary.BigEndian, uint32(t.samples[index].cto))
			runs++
			index += run
		}
		version := byte(0)
		if t.cttsV1 {
			version = 1
		}
		result.children = append(result.children, &node{kind: "ctts", payload: append(fullHeader(version, uint32(runs)), ctts.Bytes()...)})
	}
	if t.hasSync {
		var stss bytes.Buffer
		count := 0
		for index, item := range t.samples {
			if item.sync {
				_ = binary.Write(&stss, binary.BigEndian, uint32(index+1))
				count++
			}
		}
		result.children = append(result.children, &node{kind: "stss", payload: append(fullHeader(0, uint32(count)), stss.Bytes()...)})
	}
	stsz := make([]byte, 12+4*len(t.samples))
	binary.BigEndian.PutUint32(stsz[8:], uint32(len(t.samples)))
	for index, item := range t.samples {
		binary.BigEndian.PutUint32(stsz[12+index*4:], item.size)
	}
	result.children = append(result.children, &node{kind: "stsz", payload: stsz})
	return result
}

func fullHeader(version byte, count uint32) []byte {
	header := make([]byte, 8)
	header[0] = version
	binary.BigEndian.PutUint32(header[4:], count)
	return header
}

func sampleToChunk(counts []int) *node {
	var body bytes.Buffer
	entries := 0
	for index, count := range counts {
		if index > 0 && counts[index-1] == count {
			continue
		}
		_ = binary.Write(&body, binary.BigEndian, uint32(index+1))
		_ = binary.Write(&body, binary.BigEndian, uint32(count))
		_ = binary.Write(&body, binary.BigEndian, uint32(1))
		entries++
	}
	return &node{kind: "stsc", payload: append(fullHeader(0, uint32(entries)), body.Bytes()...)}
}

func chunkOffsetBox(offsets []uint64) *node {
	body := make([]byte, 8+8*len(offsets))
	binary.BigEndian.PutUint32(body[4:], uint32(len(offsets)))
	for index, offset := range offsets {
		binary.BigEndian.PutUint64(body[8+index*8:], offset)
	}
	return &node{kind: "co64", payload: body}
}

// withoutLivePhotoKey removes com.apple.quicktime.content.identifier from an
// mdta meta atom, so a clip saved back to the library is not paired with the
// Live Photo still. Unknown layouts are returned unchanged.
func withoutLivePhotoKey(payload []byte) []byte {
	prefix := 0
	if len(payload) >= 12 && string(payload[4:8]) != "hdlr" && string(payload[8:12]) == "hdlr" {
		prefix = 4
	}
	type part struct {
		kind string
		data []byte
	}
	var parts []part
	for position := prefix; position+8 <= len(payload); {
		length := int(binary.BigEndian.Uint32(payload[position:]))
		if length < 8 || position+length > len(payload) {
			return payload
		}
		parts = append(parts, part{kind: string(payload[position+4 : position+8]), data: payload[position+8 : position+length]})
		position += length
	}
	var keys, ilst []byte
	for _, item := range parts {
		switch item.kind {
		case "keys":
			keys = item.data
		case "ilst":
			ilst = item.data
		}
	}
	if len(keys) < 8 || ilst == nil {
		return payload
	}
	var names [][]byte
	for position := 8; position+8 <= len(keys); {
		length := int(binary.BigEndian.Uint32(keys[position:]))
		if length < 8 || position+length > len(keys) {
			return payload
		}
		names = append(names, keys[position:position+length])
		position += length
	}
	drop := -1
	for index, entry := range names {
		if string(entry[8:]) == "com.apple.quicktime.content.identifier" {
			drop = index + 1
		}
	}
	if drop < 0 {
		return payload
	}
	var newKeys bytes.Buffer
	newKeys.Write(keys[:4])
	_ = binary.Write(&newKeys, binary.BigEndian, uint32(len(names)-1))
	for index, entry := range names {
		if index+1 != drop {
			newKeys.Write(entry)
		}
	}
	var newIlst bytes.Buffer
	for position := 0; position+8 <= len(ilst); {
		length := int(binary.BigEndian.Uint32(ilst[position:]))
		if length < 8 || position+length > len(ilst) {
			return payload
		}
		item := append([]byte(nil), ilst[position:position+length]...)
		index := int(binary.BigEndian.Uint32(item[4:]))
		position += length
		if index == drop {
			continue
		}
		if index > drop {
			binary.BigEndian.PutUint32(item[4:], uint32(index-1))
		}
		newIlst.Write(item)
	}
	var out bytes.Buffer
	out.Write(payload[:prefix])
	for _, item := range parts {
		data := item.data
		switch item.kind {
		case "keys":
			data = newKeys.Bytes()
		case "ilst":
			data = newIlst.Bytes()
		}
		_ = binary.Write(&out, binary.BigEndian, uint32(len(data)+8))
		out.WriteString(item.kind)
		out.Write(data)
	}
	return out.Bytes()
}
