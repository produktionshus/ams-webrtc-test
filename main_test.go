package main

import (
	"bytes"
	"os"
	"testing"
	"time"
)

// Set AMS_TEST_MP4 to an H264+Opus MP4, e.g. Ant Media's test.mp4 (25 fps, ~146 s).
func TestLoadMP4(t *testing.T) {
	path := os.Getenv("AMS_TEST_MP4")
	if path == "" {
		t.Skip("AMS_TEST_MP4 not set")
	}
	video, audio, err := loadMP4(path)
	if err != nil {
		t.Fatal(err)
	}
	if len(audio) == 0 {
		t.Fatal("no opus samples")
	}
	// First sample is a keyframe: Annex-B start code followed by an SPS (NAL type 7).
	if !bytes.HasPrefix(video[0].data, []byte{0, 0, 0, 1}) || video[0].data[4]&0x1f != 7 {
		t.Fatalf("first video sample does not start with SPS: % x", video[0].data[:8])
	}
	var vd, ad time.Duration
	for _, s := range video {
		vd += s.dur
	}
	for _, s := range audio {
		ad += s.dur
	}
	if diff := vd - ad; diff > time.Second || diff < -time.Second {
		t.Fatalf("video %v and audio %v durations differ", vd, ad)
	}
	t.Logf("video %d samples %v, audio %d samples %v", len(video), vd, len(audio), ad)
}
