package media

import (
	"bytes"
	"encoding/binary"
	"testing"
)

type synthTrack struct {
	handler   string
	timescale uint32
	durations []uint32
	sizes     []uint32
	sync      []int // 1-based; nil means every sample is sync
	cto       []int32
	editMedia int64 // -1 for no edit list
	fill      byte
}

// buildSynthMovie writes a QuickTime file with one chunk per sample, moov
// first, and per-sample bytes that encode the track and sample index.
func buildSynthMovie(t testing.TB, tracks []synthTrack, movieScale, movieDuration uint32, extra ...[]byte) ([]byte, [][]byte) {
	t.Helper()
	var payloads [][]byte
	sampleData := make([][][]byte, len(tracks))
	for ti, track := range tracks {
		for si, size := range track.sizes {
			data := bytes.Repeat([]byte{track.fill}, int(size))
			binary.BigEndian.PutUint16(data, uint16(si))
			sampleData[ti] = append(sampleData[ti], data)
			payloads = append(payloads, data)
		}
	}
	build := func(dataStart uint32) []byte {
		var traks [][]byte
		offset := dataStart
		for ti, track := range tracks {
			var stts, stsz, stco []byte
			for _, duration := range track.durations {
				stts = append(stts, append(u32(1), u32(duration)...)...)
			}
			for si, size := range track.sizes {
				stsz = append(stsz, u32(size)...)
				stco = append(stco, u32(offset)...)
				offset += uint32(len(sampleData[ti][si]))
			}
			children := [][]byte{
				atom("stsd", u32(0), u32(1), atom("avc1", make([]byte, 8))),
				atom("stts", u32(0), u32(uint32(len(track.durations))), stts),
			}
			if track.cto != nil {
				var ctts []byte
				for _, value := range track.cto {
					ctts = append(ctts, append(u32(1), u32(uint32(value))...)...)
				}
				children = append(children, atom("ctts", u32(0), u32(uint32(len(track.cto))), ctts))
			}
			if track.sync != nil {
				var stss []byte
				for _, number := range track.sync {
					stss = append(stss, u32(uint32(number))...)
				}
				children = append(children, atom("stss", u32(0), u32(uint32(len(track.sync))), stss))
			}
			children = append(children,
				atom("stsc", u32(0), u32(1), u32(1), u32(1), u32(1)),
				atom("stsz", u32(0), u32(0), u32(uint32(len(track.sizes))), stsz),
				atom("stco", u32(0), u32(uint32(len(track.sizes))), stco),
			)
			var mediaDuration uint32
			for _, duration := range track.durations {
				mediaDuration += duration
			}
			trakChildren := [][]byte{
				atom("tkhd", u32(7), u32(0), u32(0), u32(uint32(ti+1)), u32(0), u32(movieDuration), make([]byte, 8), make([]byte, 8), make([]byte, 36), u32(0), u32(0)),
			}
			if track.editMedia >= 0 {
				trakChildren = append(trakChildren, atom("edts", atom("elst", u32(0), u32(1), u32(movieDuration), u32(uint32(track.editMedia)), u32(1<<16))))
			}
			trakChildren = append(trakChildren, atom("mdia",
				atom("mdhd", u32(0), u32(0), u32(0), u32(track.timescale), u32(mediaDuration), u32(0)),
				atom("hdlr", u32(0), u32(0), []byte(track.handler), make([]byte, 13)),
				atom("minf", atom("stbl", children...)),
			))
			traks = append(traks, atom("trak", trakChildren...))
		}
		moovChildren := append([][]byte{atom("mvhd", u32(0), u32(0), u32(0), u32(movieScale), u32(movieDuration), make([]byte, 80))}, traks...)
		moovChildren = append(moovChildren, extra...)
		return atom("moov", moovChildren...)
	}
	ftyp := atom("ftyp", []byte("qt  "), u32(0), []byte("qt  "))
	draft := build(0)
	moov := build(uint32(len(ftyp) + len(draft) + 8))
	file := bytes.Join([][]byte{ftyp, moov, atom("mdat", bytes.Join(payloads, nil))}, nil)
	return file, payloads
}

func repeatUint32(value uint32, count int) []uint32 {
	out := make([]uint32, count)
	for index := range out {
		out[index] = value
	}
	return out
}

func readClipTracks(t *testing.T, file []byte) (map[string]*track, *node) {
	t.Helper()
	top, err := walkBoxes(bytes.NewReader(file), 0, int64(len(file)), int64(len(file)))
	if err != nil {
		t.Fatalf("walk clip: %v", err)
	}
	moovBox, ok := findBox(top, "moov")
	if !ok || top[0].kind != "ftyp" || top[1].kind != "moov" || top[2].kind != "mdat" {
		t.Fatalf("clip layout = %+v", top)
	}
	children, err := parseTree(file[moovBox.dataStart:moovBox.end], 1)
	if err != nil {
		t.Fatalf("parse clip moov: %v", err)
	}
	moov := &node{kind: "moov", children: children}
	tracks := make(map[string]*track)
	for _, child := range moov.children {
		if child.kind == "trak" {
			parsed, err := parseClipTrack(child, 600, int64(len(file)))
			if err != nil {
				t.Fatalf("parse clip track: %v", err)
			}
			tracks[parsed.handler] = parsed
		}
	}
	return tracks, moov
}

func TestClipCutsAtKeyframeWithExactEdit(t *testing.T) {
	video := synthTrack{handler: "vide", timescale: 600, durations: repeatUint32(20, 90), sizes: make([]uint32, 90), sync: []int{1, 31, 61}, editMedia: 0, fill: 0xAA}
	for index := range video.sizes {
		video.sizes[index] = uint32(100 + index)
	}
	audio := synthTrack{handler: "soun", timescale: 48000, durations: repeatUint32(1024, 143), sizes: repeatUint32(12, 143), editMedia: 2112, fill: 0xBB}
	metadata := synthTrack{handler: "meta", timescale: 600, durations: []uint32{1800}, sizes: []uint32{8}, editMedia: -1, fill: 0xCC}
	appleKeys := atom("meta",
		atom("hdlr", u32(0), u32(0), []byte("mdta"), make([]byte, 13)),
		atom("keys", u32(0), u32(2),
			append(u32(8+32), append([]byte("mdta"), []byte("com.apple.quicktime.creationdate")...)...),
			append(u32(8+38), append([]byte("mdta"), []byte("com.apple.quicktime.content.identifier")...)...)),
		atom("ilst",
			append(append(u32(8+8+8+24), u32(1)...), atom("data", u32(1), u32(0), []byte("2026-10-02T20:43:23-0400"))...),
			append(append(u32(8+8+8+4), u32(2)...), atom("data", u32(1), u32(0), []byte("LIVE"))...)),
	)
	file, _ := buildSynthMovie(t, []synthTrack{video, audio, metadata}, 600, 1800, appleKeys)

	clip, err := PlanClip(bytes.NewReader(file), int64(len(file)), 1.25, 2.5)
	if err != nil {
		t.Fatalf("PlanClip: %v", err)
	}
	var out bytes.Buffer
	written, err := clip.WriteTo(&out)
	if err != nil || written != clip.Size() || int64(out.Len()) != clip.Size() {
		t.Fatalf("WriteTo = %d/%d %v", written, clip.Size(), err)
	}
	if clip.Duration() != 1.25 {
		t.Fatalf("duration = %v", clip.Duration())
	}
	tracks, moov := readClipTracks(t, out.Bytes())
	if len(tracks) != 2 || tracks["meta"] != nil {
		t.Fatalf("kept tracks = %v", tracks)
	}
	// Video starts at the keyframe at 1.0 s (sample 31) and ends with the last
	// frame shown before 2.5 s; the edit skips 0.25 s of pre-roll.
	cut := tracks["vide"]
	if len(cut.samples) != 45 || !cut.samples[0].sync || cut.samples[0].size != 130 {
		t.Fatalf("video samples = %d first=%+v", len(cut.samples), cut.samples[0])
	}
	if cut.mediaStart != 150 {
		t.Fatalf("video edit media time = %d", cut.mediaStart)
	}
	syncCount := 0
	for _, item := range cut.samples {
		if item.sync {
			syncCount++
		}
	}
	// Keyframes at source samples 31 and 61 (1-based) remain keyframes.
	if syncCount != 2 || !cut.samples[30].sync {
		t.Fatalf("sync samples = %d", syncCount)
	}
	for index, item := range cut.samples {
		data := out.Bytes()[item.offset : item.offset+int64(item.size)]
		if data[2] != 0xAA || int(binary.BigEndian.Uint16(data)) != 30+index {
			t.Fatalf("video sample %d bytes come from source sample %d", index, binary.BigEndian.Uint16(data))
		}
	}
	sound := tracks["soun"]
	// Audio edit 2112 + 1.25 s = 62112 → sample 60 starts at 61440.
	if sound.samples[0].dts != 0 || sound.mediaStart != 62112-61440 {
		t.Fatalf("audio edit = %d", sound.mediaStart)
	}
	data := out.Bytes()[sound.samples[0].offset:]
	if data[2] != 0xBB || binary.BigEndian.Uint16(data) != 60 {
		t.Fatalf("audio first sample = %d", binary.BigEndian.Uint16(data))
	}
	// The Live Photo identifier is dropped; the capture date is kept.
	if meta := moov.child("meta"); meta == nil {
		t.Fatal("moov meta missing")
	} else {
		var parsed Metadata
		parseQuickTimeMeta(meta.payload, &parsed, nil)
		if parsed.LivePhotoID != "" || parsed.CapturedAt == nil {
			t.Fatalf("clip metadata = %+v", parsed)
		}
	}
	// Interleaved: chunks alternate between tracks instead of all video first.
	offsets := []int64{cut.samples[0].offset, sound.samples[0].offset}
	if offsets[1] > cut.samples[len(cut.samples)-1].offset {
		t.Fatal("audio was not interleaved with video")
	}
}

func TestClipHonoursCompositionOffsetsAndBounds(t *testing.T) {
	video := synthTrack{handler: "vide", timescale: 600, durations: repeatUint32(20, 60), sizes: repeatUint32(50, 60), sync: []int{1, 31}, cto: make([]int32, 60), editMedia: 40, fill: 0xAA}
	for index := range video.cto {
		video.cto[index] = 40
	}
	file, _ := buildSynthMovie(t, []synthTrack{video}, 600, 1200)
	clip, err := PlanClip(bytes.NewReader(file), int64(len(file)), 1.0, 5)
	if err != nil {
		t.Fatal(err)
	}
	if clip.Duration() != 1.0 {
		t.Fatalf("end clamps to the movie: %v", clip.Duration())
	}
	var out bytes.Buffer
	if _, err := clip.WriteTo(&out); err != nil {
		t.Fatal(err)
	}
	cut, _ := readClipTracks(t, out.Bytes())
	// mediaStart = 40 + 600 = 640 → keyframe 31 (dts 600, cts 640).
	if len(cut["vide"].samples) != 30 || cut["vide"].mediaStart != 40 || !cut["vide"].hasCTTS {
		t.Fatalf("cut = %d samples edit %d", len(cut["vide"].samples), cut["vide"].mediaStart)
	}
	for _, bad := range [][2]float64{{-1, 1}, {2, 1}, {1.95, 2.0}, {3, 4}} {
		if _, err := PlanClip(bytes.NewReader(file), int64(len(file)), bad[0], bad[1]); err == nil {
			t.Fatalf("range %v accepted", bad)
		}
	}
}

func TestClipRejectsUnsupportedLayouts(t *testing.T) {
	if _, err := PlanClip(bytes.NewReader([]byte("not a movie")), 11, 0, 1); err == nil {
		t.Fatal("garbage accepted")
	}
	audioOnly := synthTrack{handler: "soun", timescale: 48000, durations: repeatUint32(1024, 100), sizes: repeatUint32(4, 100), editMedia: 0, fill: 1}
	file, _ := buildSynthMovie(t, []synthTrack{audioOnly}, 600, 1200)
	if _, err := PlanClip(bytes.NewReader(file), int64(len(file)), 0, 1); err != ErrClipUnsupported {
		t.Fatalf("audio-only error = %v", err)
	}
	video := synthTrack{handler: "vide", timescale: 600, durations: repeatUint32(20, 30), sizes: repeatUint32(10, 30), editMedia: 0, fill: 2}
	file, _ = buildSynthMovie(t, []synthTrack{video}, 600, 600)
	for cut := 0; cut < len(file); cut += 3 {
		_, _ = PlanClip(bytes.NewReader(file[:cut]), int64(cut), 0, 0.5)
	}
}

func FuzzPlanClip(f *testing.F) {
	video := synthTrack{handler: "vide", timescale: 600, durations: repeatUint32(20, 30), sizes: repeatUint32(10, 30), sync: []int{1, 16}, editMedia: 0, fill: 2}
	audio := synthTrack{handler: "soun", timescale: 48000, durations: repeatUint32(1024, 20), sizes: repeatUint32(4, 20), editMedia: 2112, fill: 3}
	seed, _ := buildSynthMovie(f, []synthTrack{video, audio}, 600, 600)
	f.Add(seed, 0.2, 0.8)
	f.Fuzz(func(t *testing.T, data []byte, start, end float64) {
		clip, err := PlanClip(bytes.NewReader(data), int64(len(data)), start, end)
		if err != nil {
			return
		}
		var out bytes.Buffer
		written, err := clip.WriteTo(&out)
		if err != nil || written != clip.Size() {
			t.Fatalf("WriteTo = %d/%d %v", written, clip.Size(), err)
		}
	})
}
