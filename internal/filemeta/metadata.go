// Package filemeta extracts and encrypts versioned file metadata. Metadata is
// stored separately from the file index so new attributes do not require a
// schema column for every format-specific field.
package filemeta

import (
	"bytes"
	"context"
	"crypto/aes"
	"crypto/cipher"
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"cryp/internal/procgroup"
)

const (
	SchemaVersion    = 2
	ExtractorVersion = 2
	maxProbeOutput   = 4 * 1024 * 1024
)

type Binding struct {
	ContentHash string `json:"contentHash,omitempty"`
	Size        int64  `json:"size"`
	ModTime     int64  `json:"modTime"`
}

// Stream is the normalized description of one media stream. String-valued
// ratios/time bases are intentionally preserved verbatim (for example
// "30000/1001") so callers can make exact frame/timestamp decisions later.
type Stream struct {
	Index              int               `json:"index"`
	Type               string            `json:"type"`
	Codec              string            `json:"codec,omitempty"`
	CodecLongName      string            `json:"codecLongName,omitempty"`
	Profile            string            `json:"profile,omitempty"`
	CodecTag           string            `json:"codecTag,omitempty"`
	DurationSeconds    float64           `json:"durationSeconds,omitempty"`
	StartTimeSeconds   float64           `json:"startTimeSeconds,omitempty"`
	DurationTS         int64             `json:"durationTS,omitempty"`
	FrameCount         int64             `json:"frameCount,omitempty"`
	TimeBase           string            `json:"timeBase,omitempty"`
	BitRate            int64             `json:"bitRate,omitempty"`
	Width              int               `json:"width,omitempty"`
	Height             int               `json:"height,omitempty"`
	CodedWidth         int               `json:"codedWidth,omitempty"`
	CodedHeight        int               `json:"codedHeight,omitempty"`
	PixelFormat        string            `json:"pixelFormat,omitempty"`
	FrameRate          string            `json:"frameRate,omitempty"`
	AverageFrameRate   string            `json:"averageFrameRate,omitempty"`
	SampleAspectRatio  string            `json:"sampleAspectRatio,omitempty"`
	DisplayAspectRatio string            `json:"displayAspectRatio,omitempty"`
	Rotation           int               `json:"rotation,omitempty"`
	ColorRange         string            `json:"colorRange,omitempty"`
	ColorSpace         string            `json:"colorSpace,omitempty"`
	ColorTransfer      string            `json:"colorTransfer,omitempty"`
	ColorPrimaries     string            `json:"colorPrimaries,omitempty"`
	FieldOrder         string            `json:"fieldOrder,omitempty"`
	BitsPerRawSample   int               `json:"bitsPerRawSample,omitempty"`
	SampleFormat       string            `json:"sampleFormat,omitempty"`
	SampleRate         int               `json:"sampleRate,omitempty"`
	Channels           int               `json:"channels,omitempty"`
	ChannelLayout      string            `json:"channelLayout,omitempty"`
	Language           string            `json:"language,omitempty"`
	Title              string            `json:"title,omitempty"`
	HandlerName        string            `json:"handlerName,omitempty"`
	Disposition        map[string]int    `json:"disposition,omitempty"`
	Tags               map[string]string `json:"tags,omitempty"`
}

type Chapter struct {
	ID               int               `json:"id"`
	StartTimeSeconds float64           `json:"startTimeSeconds,omitempty"`
	EndTimeSeconds   float64           `json:"endTimeSeconds,omitempty"`
	TimeBase         string            `json:"timeBase,omitempty"`
	Title            string            `json:"title,omitempty"`
	Tags             map[string]string `json:"tags,omitempty"`
}

type Media struct {
	DurationSeconds  float64           `json:"durationSeconds,omitempty"`
	Format           string            `json:"format,omitempty"`
	FormatLongName   string            `json:"formatLongName,omitempty"`
	StartTimeSeconds float64           `json:"startTimeSeconds,omitempty"`
	Size             int64             `json:"size,omitempty"`
	BitRate          int64             `json:"bitRate,omitempty"`
	ProbeScore       int               `json:"probeScore,omitempty"`
	StreamCount      int               `json:"streamCount,omitempty"`
	ProgramCount     int               `json:"programCount,omitempty"`
	Width            int               `json:"width,omitempty"`
	Height           int               `json:"height,omitempty"`
	VideoCodec       string            `json:"videoCodec,omitempty"`
	AudioCodec       string            `json:"audioCodec,omitempty"`
	AudioChannels    int               `json:"audioChannels,omitempty"`
	AudioSampleRate  int               `json:"audioSampleRate,omitempty"`
	Streams          []Stream          `json:"streams,omitempty"`
	Chapters         []Chapter         `json:"chapters,omitempty"`
	Tags             map[string]string `json:"tags,omitempty"`
}

type Record struct {
	SchemaVersion    int     `json:"schemaVersion"`
	ExtractorVersion int     `json:"extractorVersion"`
	Binding          Binding `json:"binding"`
	Media            *Media  `json:"media,omitempty"`
}

func (r *Record) Normalize() {
	if r.SchemaVersion == 0 {
		r.SchemaVersion = SchemaVersion
	}
	if r.ExtractorVersion == 0 {
		r.ExtractorVersion = ExtractorVersion
	}
}

func (r Record) Duration() float64 {
	if r.Media == nil || !validDuration(r.Media.DurationSeconds) {
		return 0
	}
	return r.Media.DurationSeconds
}

func validDuration(value float64) bool {
	return value > 0 && !math.IsNaN(value) && !math.IsInf(value, 0)
}

func metadataKey(masterKey []byte, vaultID string) ([]byte, error) {
	if len(masterKey) != 32 {
		return nil, errors.New("invalid metadata master key")
	}
	mac := hmac.New(sha256.New, masterKey)
	_, _ = mac.Write([]byte("cryp:file-metadata-key:v1\x00"))
	_, _ = mac.Write([]byte(vaultID))
	return mac.Sum(nil), nil
}

func metadataAAD(vaultID, pathKey string) []byte {
	return []byte("cryp:file-metadata:v1\x00" + vaultID + "\x00" + pathKey)
}

func Seal(masterKey []byte, vaultID, pathKey string, record Record) ([]byte, error) {
	record.Normalize()
	plaintext, err := json.Marshal(record)
	if err != nil {
		return nil, fmt.Errorf("encode metadata: %w", err)
	}
	defer zero(plaintext)
	key, err := metadataKey(masterKey, vaultID)
	if err != nil {
		return nil, err
	}
	defer zero(key)
	block, err := aes.NewCipher(key)
	if err != nil {
		return nil, err
	}
	gcm, err := cipher.NewGCM(block)
	if err != nil {
		return nil, err
	}
	nonce := make([]byte, gcm.NonceSize())
	if _, err := io.ReadFull(rand.Reader, nonce); err != nil {
		return nil, fmt.Errorf("metadata nonce: %w", err)
	}
	return gcm.Seal(nonce, nonce, plaintext, metadataAAD(vaultID, pathKey)), nil
}

func Open(masterKey []byte, vaultID, pathKey string, sealed []byte) (Record, error) {
	var record Record
	key, err := metadataKey(masterKey, vaultID)
	if err != nil {
		return record, err
	}
	defer zero(key)
	block, err := aes.NewCipher(key)
	if err != nil {
		return record, err
	}
	gcm, err := cipher.NewGCM(block)
	if err != nil {
		return record, err
	}
	if len(sealed) < gcm.NonceSize()+gcm.Overhead() {
		return record, errors.New("encrypted metadata is truncated")
	}
	nonce := sealed[:gcm.NonceSize()]
	plaintext, err := gcm.Open(nil, nonce, sealed[gcm.NonceSize():], metadataAAD(vaultID, pathKey))
	if err != nil {
		return record, errors.New("encrypted metadata authentication failed")
	}
	defer zero(plaintext)
	if err := json.Unmarshal(plaintext, &record); err != nil {
		return Record{}, fmt.Errorf("decode metadata: %w", err)
	}
	if record.SchemaVersion != SchemaVersion {
		return Record{}, fmt.Errorf("unsupported metadata schema %d", record.SchemaVersion)
	}
	return record, nil
}

func zero(value []byte) {
	for index := range value {
		value[index] = 0
	}
}

func IsMediaPath(path string) bool {
	switch strings.ToLower(filepath.Ext(path)) {
	case ".mp4", ".m4v", ".mov", ".mkv", ".webm", ".avi", ".wmv", ".flv", ".ts", ".mts", ".m2ts", ".mpg", ".mpeg", ".3gp", ".3g2", ".vob", ".ogv", ".asf", ".rm", ".rmvb", ".divx", ".f4v", ".mxf", ".h264", ".h265", ".hevc", ".mp3", ".m4a", ".aac", ".flac", ".wav", ".ogg", ".opus":
		return true
	default:
		return false
	}
}

func ffprobeBinary() string {
	if binary := strings.TrimSpace(os.Getenv("CRYP_FFPROBE_BIN")); binary != "" {
		return binary
	}
	return "ffprobe"
}

type probeOutput struct {
	Format struct {
		Duration   string            `json:"duration"`
		FormatName string            `json:"format_name"`
		FormatLong string            `json:"format_long_name"`
		StartTime  string            `json:"start_time"`
		Size       string            `json:"size"`
		BitRate    string            `json:"bit_rate"`
		ProbeScore int               `json:"probe_score"`
		NbStreams  int               `json:"nb_streams"`
		NbPrograms int               `json:"nb_programs"`
		Tags       map[string]string `json:"tags"`
	} `json:"format"`
	Streams []struct {
		Index              int               `json:"index"`
		CodecType          string            `json:"codec_type"`
		CodecName          string            `json:"codec_name"`
		CodecLongName      string            `json:"codec_long_name"`
		Profile            string            `json:"profile"`
		CodecTag           string            `json:"codec_tag_string"`
		Duration           string            `json:"duration"`
		StartTime          string            `json:"start_time"`
		DurationTS         string            `json:"duration_ts"`
		FrameCount         string            `json:"nb_frames"`
		TimeBase           string            `json:"time_base"`
		BitRate            string            `json:"bit_rate"`
		SampleRate         string            `json:"sample_rate"`
		Width              int               `json:"width"`
		Height             int               `json:"height"`
		CodedWidth         int               `json:"coded_width"`
		CodedHeight        int               `json:"coded_height"`
		PixelFormat        string            `json:"pix_fmt"`
		FrameRate          string            `json:"r_frame_rate"`
		AverageFrameRate   string            `json:"avg_frame_rate"`
		SampleAspectRatio  string            `json:"sample_aspect_ratio"`
		DisplayAspectRatio string            `json:"display_aspect_ratio"`
		Rotation           string            `json:"rotation"`
		ColorRange         string            `json:"color_range"`
		ColorSpace         string            `json:"color_space"`
		ColorTransfer      string            `json:"color_transfer"`
		ColorPrimaries     string            `json:"color_primaries"`
		FieldOrder         string            `json:"field_order"`
		BitsPerRawSample   int               `json:"bits_per_raw_sample"`
		SampleFormat       string            `json:"sample_fmt"`
		Channels           int               `json:"channels"`
		ChannelLayout      string            `json:"channel_layout"`
		Language           string            `json:"TAG:language"`
		Title              string            `json:"TAG:title"`
		HandlerName        string            `json:"TAG:handler_name"`
		Disposition        map[string]int    `json:"disposition"`
		Tags               map[string]string `json:"tags"`
	} `json:"streams"`
	Chapters []struct {
		ID        int               `json:"id"`
		StartTime string            `json:"start_time"`
		EndTime   string            `json:"end_time"`
		TimeBase  string            `json:"time_base"`
		Tags      map[string]string `json:"tags"`
	} `json:"chapters"`
}

type cappedBuffer struct {
	bytes.Buffer
	limit    int
	overflow bool
}

func (buffer *cappedBuffer) Write(value []byte) (int, error) {
	written := len(value)
	remaining := buffer.limit - buffer.Len()
	if remaining <= 0 {
		buffer.overflow = true
		return written, nil
	}
	if len(value) > remaining {
		buffer.overflow = true
		value = value[:remaining]
	}
	_, _ = buffer.Buffer.Write(value)
	return written, nil
}

func Probe(ctx context.Context, input, headers string, timeout time.Duration) (Record, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	if timeout <= 0 {
		timeout = 5 * time.Second
	}
	probeCtx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	args := []string{"-v", "error"}
	if headers != "" {
		args = append(args, "-headers", headers)
	}
	args = append(args,
		"-show_entries", "format=duration,format_name,format_long_name,start_time,size,bit_rate,probe_score,nb_streams,nb_programs:format_tags=title,artist,album,date,comment,genre,encoder,creation_time:stream=index,codec_type,codec_name,codec_long_name,profile,codec_tag_string,duration,start_time,duration_ts,nb_frames,time_base,bit_rate,sample_rate,width,height,coded_width,coded_height,pix_fmt,r_frame_rate,avg_frame_rate,sample_aspect_ratio,display_aspect_ratio,rotation,color_range,color_space,color_transfer,color_primaries,field_order,bits_per_raw_sample,sample_fmt,channels,channel_layout,disposition:stream_tags=language,title,handler_name:chapter=id,start_time,end_time,time_base:chapter_tags=title",
		"-show_chapters",
		"-of", "json", input,
	)
	cmd := exec.CommandContext(probeCtx, ffprobeBinary(), args...)
	cmd.WaitDelay = 2 * time.Second
	procgroup.Configure(cmd)
	output := cappedBuffer{limit: maxProbeOutput}
	stderr := procgroup.NewTailBuffer(64 * 1024)
	cmd.Stdout = &output
	cmd.Stderr = stderr
	if err := cmd.Run(); err != nil {
		if probeCtx.Err() != nil {
			return Record{}, probeCtx.Err()
		}
		return Record{}, fmt.Errorf("ffprobe: %w stderr=%s", err, strings.TrimSpace(stderr.String()))
	}
	if output.overflow {
		return Record{}, errors.New("ffprobe metadata output is too large")
	}
	return decodeProbeOutput(output.Bytes())
}

func decodeProbeOutput(output []byte) (Record, error) {
	var decoded probeOutput
	if err := json.Unmarshal(output, &decoded); err != nil {
		return Record{}, fmt.Errorf("decode ffprobe metadata: %w", err)
	}
	media := &Media{
		Format: decoded.Format.FormatName, FormatLongName: decoded.Format.FormatLong,
		StartTimeSeconds: parseFloat(decoded.Format.StartTime), Size: parseInt64(decoded.Format.Size),
		BitRate: parseInt64(decoded.Format.BitRate), ProbeScore: decoded.Format.ProbeScore,
		StreamCount: decoded.Format.NbStreams, ProgramCount: decoded.Format.NbPrograms,
		Tags:    decoded.Format.Tags,
		Streams: make([]Stream, 0, len(decoded.Streams)), Chapters: make([]Chapter, 0, len(decoded.Chapters)),
	}
	duration := parseFloat(decoded.Format.Duration)
	for _, stream := range decoded.Streams {
		streamDuration := parseFloat(stream.Duration)
		duration = math.Max(duration, streamDuration)
		normalized := Stream{
			Index: stream.Index, Type: stream.CodecType, Codec: stream.CodecName, CodecLongName: stream.CodecLongName,
			Profile: stream.Profile, CodecTag: stream.CodecTag, DurationSeconds: streamDuration,
			StartTimeSeconds: parseFloat(stream.StartTime), DurationTS: parseInt64(stream.DurationTS), FrameCount: parseInt64(stream.FrameCount),
			TimeBase: stream.TimeBase, BitRate: parseInt64(stream.BitRate),
			Width: stream.Width, Height: stream.Height, CodedWidth: stream.CodedWidth, CodedHeight: stream.CodedHeight,
			PixelFormat: stream.PixelFormat, FrameRate: stream.FrameRate, AverageFrameRate: stream.AverageFrameRate,
			SampleAspectRatio: stream.SampleAspectRatio, DisplayAspectRatio: stream.DisplayAspectRatio,
			Rotation: int(parseInt64(stream.Rotation)), ColorRange: stream.ColorRange, ColorSpace: stream.ColorSpace,
			ColorTransfer: stream.ColorTransfer, ColorPrimaries: stream.ColorPrimaries, FieldOrder: stream.FieldOrder,
			BitsPerRawSample: stream.BitsPerRawSample, SampleFormat: stream.SampleFormat,
			SampleRate: int(parseInt64(stream.SampleRate)), Channels: stream.Channels, ChannelLayout: stream.ChannelLayout,
			Language: stream.Language, Title: stream.Title, HandlerName: stream.HandlerName,
			Disposition: stream.Disposition, Tags: stream.Tags,
		}
		if normalized.Language == "" && stream.Tags != nil {
			normalized.Language = stream.Tags["language"]
		}
		if normalized.Title == "" && stream.Tags != nil {
			normalized.Title = stream.Tags["title"]
		}
		if normalized.HandlerName == "" && stream.Tags != nil {
			normalized.HandlerName = stream.Tags["handler_name"]
		}
		media.Streams = append(media.Streams, normalized)
		switch stream.CodecType {
		case "video":
			if media.VideoCodec == "" {
				media.VideoCodec = stream.CodecName
				media.Width = stream.Width
				media.Height = stream.Height
			}
		case "audio":
			if media.AudioCodec == "" {
				media.AudioCodec = stream.CodecName
				media.AudioChannels = stream.Channels
				media.AudioSampleRate = int(parseInt64(stream.SampleRate))
			}
		}
		if media.BitRate == 0 {
			media.BitRate = parseInt64(stream.BitRate)
		}
	}
	for _, chapter := range decoded.Chapters {
		title := ""
		if chapter.Tags != nil {
			title = chapter.Tags["title"]
		}
		media.Chapters = append(media.Chapters, Chapter{ID: chapter.ID, StartTimeSeconds: parseFloat(chapter.StartTime), EndTimeSeconds: parseFloat(chapter.EndTime), TimeBase: chapter.TimeBase, Title: title, Tags: chapter.Tags})
	}
	if validDuration(duration) {
		media.DurationSeconds = duration
	}
	if media.DurationSeconds == 0 && media.VideoCodec == "" && media.AudioCodec == "" {
		return Record{}, errors.New("ffprobe returned no usable media metadata")
	}
	record := Record{SchemaVersion: SchemaVersion, ExtractorVersion: ExtractorVersion, Media: media}
	return record, nil
}

func parseFloat(value string) float64 {
	parsed, _ := strconv.ParseFloat(strings.TrimSpace(value), 64)
	if math.IsNaN(parsed) || math.IsInf(parsed, 0) || parsed < 0 {
		return 0
	}
	return parsed
}

func parseInt64(value string) int64 {
	parsed, _ := strconv.ParseInt(strings.TrimSpace(value), 10, 64)
	if parsed < 0 {
		return 0
	}
	return parsed
}
