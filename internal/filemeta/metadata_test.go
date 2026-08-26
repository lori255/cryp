package filemeta

import (
	"bytes"
	"testing"
)

func TestSealOpenRoundTripAndAADBinding(t *testing.T) {
	key := bytes.Repeat([]byte{0x42}, 32)
	record := Record{
		Binding: Binding{ContentHash: "protected", Size: 1234, ModTime: 5678},
		Media: &Media{
			DurationSeconds: 42.5,
			Format:          "mov,mp4",
			Width:           1920,
			Height:          1080,
			VideoCodec:      "h264",
			AudioCodec:      "aac",
		},
	}
	sealed, err := Seal(key, "vault-a", "path-a", record)
	if err != nil {
		t.Fatalf("Seal: %v", err)
	}
	opened, err := Open(key, "vault-a", "path-a", sealed)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	if opened.SchemaVersion != SchemaVersion || opened.ExtractorVersion != ExtractorVersion {
		t.Fatalf("versions = %d/%d", opened.SchemaVersion, opened.ExtractorVersion)
	}
	if opened.Binding != record.Binding || opened.Media == nil || opened.Media.DurationSeconds != 42.5 || opened.Media.VideoCodec != "h264" {
		t.Fatalf("opened metadata = %#v", opened)
	}
	if _, err := Open(key, "vault-b", "path-a", sealed); err == nil {
		t.Fatal("metadata opened under a different vault")
	}
	if _, err := Open(key, "vault-a", "path-b", sealed); err == nil {
		t.Fatal("metadata opened under a different path")
	}
}

func TestSealUsesFreshNonce(t *testing.T) {
	key := bytes.Repeat([]byte{0x24}, 32)
	record := Record{Media: &Media{DurationSeconds: 1}}
	first, err := Seal(key, "vault", "path", record)
	if err != nil {
		t.Fatal(err)
	}
	second, err := Seal(key, "vault", "path", record)
	if err != nil {
		t.Fatal(err)
	}
	if bytes.Equal(first, second) {
		t.Fatal("metadata encryption reused its nonce")
	}
}

func TestDecodeProbeOutputCollectsCommonMediaFields(t *testing.T) {
	record, err := decodeProbeOutput([]byte(`{
		"format":{"duration":"63.25","format_name":"matroska,webm","format_long_name":"Matroska / WebM","size":"123456","bit_rate":"2400000","probe_score":100,"nb_streams":3,"tags":{"title":"demo"}},
		"streams":[
			{"index":0,"codec_type":"video","codec_name":"hevc","codec_long_name":"H.265","profile":"Main 10","width":3840,"height":2160,"pix_fmt":"yuv420p10le","avg_frame_rate":"30000/1001","time_base":"1/1000","tags":{"language":"und","title":"video"}},
			{"index":1,"codec_type":"audio","codec_name":"aac","channels":6,"sample_rate":"48000","channel_layout":"5.1","tags":{"language":"eng","title":"English"}},
			{"index":2,"codec_type":"subtitle","codec_name":"ass","tags":{"language":"jpn","title":"Japanese"}}
		],
		"chapters":[{"id":1,"start_time":"0","end_time":"12.5","time_base":"1/1000","tags":{"title":"Intro"}}]
	}`))
	if err != nil {
		t.Fatalf("decodeProbeOutput: %v", err)
	}
	media := record.Media
	if media == nil || media.DurationSeconds != 63.25 || media.Format != "matroska,webm" ||
		media.BitRate != 2400000 || media.Width != 3840 || media.Height != 2160 ||
		media.VideoCodec != "hevc" || media.AudioCodec != "aac" ||
		media.AudioChannels != 6 || media.AudioSampleRate != 48000 || media.FormatLongName != "Matroska / WebM" ||
		media.Size != 123456 || media.ProbeScore != 100 || len(media.Streams) != 3 || len(media.Chapters) != 1 ||
		media.Streams[0].AverageFrameRate != "30000/1001" || media.Streams[1].ChannelLayout != "5.1" ||
		media.Streams[1].Language != "eng" || media.Streams[2].Type != "subtitle" || media.Chapters[0].Title != "Intro" {
		t.Fatalf("media = %#v", media)
	}
}
