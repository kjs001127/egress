//go:build integration

package test

import (
	"encoding/json"
	"fmt"
	"math"
	"os/exec"
	"regexp"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/livekit/egress/pkg/config"
	"github.com/livekit/egress/pkg/types"
	"github.com/livekit/protocol/livekit"
)

const (
	maxRetries = 5
	minDelay   = time.Millisecond * 100
	maxDelay   = time.Second * 5
)

var (
	segmentTimeRegexp = regexp.MustCompile(`_(\d{14})(\d{3})\.ts`)
)

type FFProbeInfo struct {
	Streams []struct {
		CodecName string `json:"codec_name"`
		CodecType string `json:"codec_type"`
		Profile   string `json:"profile"`

		// audio
		SampleRate    string `json:"sample_rate"`
		Channels      int    `json:"channels"`
		ChannelLayout string `json:"channel_layout"`

		// video
		Width        int32  `json:"width"`
		Height       int32  `json:"height"`
		RFrameRate   string `json:"r_frame_rate"`
		AvgFrameRate string `json:"avg_frame_rate"`
		BitRate      string `json:"bit_rate"`
	} `json:"streams"`
	Format struct {
		Filename   string `json:"filename"`
		FormatName string `json:"format_name"`
		Duration   string `json:"duration"`
		Size       string `json:"size"`
		ProbeScore int    `json:"probe_score"`
		Tags       struct {
			Encoder string `json:"encoder"`
		} `json:"tags"`
	} `json:"format"`
}

func ffprobe(input string) (*FFProbeInfo, error) {
	args := []string{
		"-v", "quiet",
		"-hide_banner",
		"-show_format",
		"-show_streams",
		"-print_format", "json",
	}

	if strings.HasSuffix(input, ".raw") {
		args = append(args,
			"-f", "s16le",
			"-ac", "2",
			"-ar", "48k",
		)
	}

	args = append(args, input)

	cmd := exec.Command("ffprobe", args...)
	out, err := cmd.Output()
	if err != nil {
		return nil, err
	}

	info := &FFProbeInfo{}
	err = json.Unmarshal(out, info)
	return info, err
}

func verify(t *testing.T, in string, p *config.PipelineConfig, res *livekit.EgressInfo, egressType types.EgressType, withMuting bool, sourceFramerate float64) {
	info, err := ffprobe(in)
	require.NoError(t, err, "input %s does not exist", in)

	switch p.Outputs[egressType].GetOutputType() {
	case types.OutputTypeRaw:
		require.Equal(t, 0, info.Format.ProbeScore)
	case types.OutputTypeIVF:
		require.Equal(t, 98, info.Format.ProbeScore)
	default:
		require.Equal(t, 100, info.Format.ProbeScore)
	}

	switch egressType {
	case types.EgressTypeFile:
		// size
		require.NotEqual(t, "0", info.Format.Size)

		// duration
		fileRes := res.GetFile()
		if fileRes == nil {
			fileRes = res.FileResults[0]
		}
		expected := float64(fileRes.Duration) / 1e9
		actual, err := strconv.ParseFloat(info.Format.Duration, 64)
		require.NoError(t, err)

		// file duration can be different from egress duration based on keyframes, muting, and latency
		delta := 4.5
		switch p.Info.Request.(type) {
		case *livekit.EgressInfo_RoomComposite:
			require.InDelta(t, expected, actual, delta)

		case *livekit.EgressInfo_Track:
			if p.AudioEnabled {
				if withMuting {
					delta = 6
				}
				require.InDelta(t, expected, actual, delta)
			}
		}

	case types.EgressTypeSegments:
		actual, err := strconv.ParseFloat(info.Format.Duration, 64)
		require.NoError(t, err)

		require.Len(t, res.GetSegmentResults(), 1)
		segments := res.GetSegmentResults()[0]
		expected := int64(math.Ceil(actual / float64(p.GetSegmentConfig().SegmentDuration)))
		require.True(t, segments.SegmentCount == expected || segments.SegmentCount == expected-1)
	}

	// check stream info
	var hasAudio, hasVideo bool
	for _, stream := range info.Streams {
		switch stream.CodecType {
		case "audio":
			hasAudio = true

			// codec
			switch p.AudioOutCodec {
			case types.MimeTypeAAC:
				require.Equal(t, "aac", stream.CodecName)
				require.Equal(t, fmt.Sprint(p.AudioFrequency), stream.SampleRate)
				require.Equal(t, "stereo", stream.ChannelLayout)

			case types.MimeTypeOpus:
				require.Equal(t, "opus", stream.CodecName)
				require.Equal(t, "48000", stream.SampleRate)
				require.Equal(t, "stereo", stream.ChannelLayout)

			case types.MimeTypeRawAudio:
				require.Equal(t, "pcm_s16le", stream.CodecName)
				require.Equal(t, "48000", stream.SampleRate)
			}

			// channels
			require.Equal(t, 2, stream.Channels)

			// audio bitrate
			if p.Outputs[egressType].GetOutputType() == types.OutputTypeMP4 {
				bitrate, err := strconv.Atoi(stream.BitRate)
				require.NoError(t, err)
				require.NotZero(t, bitrate)
			}

		case "video":
			hasVideo = true

			// codec and profile
			switch p.VideoOutCodec {
			case types.MimeTypeH264:
				require.Equal(t, "h264", stream.CodecName)

				if p.VideoTranscoding {
					switch p.VideoProfile {
					case types.ProfileBaseline:
						require.Equal(t, "Constrained Baseline", stream.Profile)
					case types.ProfileMain:
						require.Equal(t, "Main", stream.Profile)
					case types.ProfileHigh:
						require.Equal(t, "High", stream.Profile)
					}
				}
			case types.MimeTypeVP8:
				require.Equal(t, "vp8", stream.CodecName)
			}

			switch p.Outputs[egressType].GetOutputType() {
			case types.OutputTypeIVF:
				require.Equal(t, "vp8", stream.CodecName)

			case types.OutputTypeMP4:
				require.Equal(t, "h264", stream.CodecName)

				if p.VideoTranscoding {
					// bitrate, not available for HLS or WebM
					bitrate, err := strconv.Atoi(stream.BitRate)
					require.NoError(t, err)
					require.NotZero(t, bitrate)
					require.Less(t, int32(bitrate), p.VideoBitrate*1010)

					// framerate
					frac := strings.Split(stream.AvgFrameRate, "/")
					require.Len(t, frac, 2)
					n, err := strconv.ParseFloat(frac[0], 64)
					require.NoError(t, err)
					d, err := strconv.ParseFloat(frac[1], 64)
					require.NoError(t, err)
					require.NotZero(t, d)
					require.Less(t, n/d, float64(p.Framerate)*1.05)
					require.Greater(t, n/d, float64(sourceFramerate)*0.8)

				}
				fallthrough

			case types.OutputTypeHLS:
				require.Equal(t, "h264", stream.CodecName)

				if p.VideoTranscoding {
					// dimensions
					require.Equal(t, p.Width, stream.Width)
					require.Equal(t, p.Height, stream.Height)
				}
			}

		default:
			t.Fatalf("unrecognized stream type %s", stream.CodecType)
		}
	}

	if p.AudioEnabled {
		require.True(t, hasAudio)
		require.NotEmpty(t, p.AudioOutCodec)
	}

	if p.VideoEnabled {
		require.True(t, hasVideo)
		require.NotEmpty(t, p.VideoOutCodec)
	}
}
