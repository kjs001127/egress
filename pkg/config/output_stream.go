package config

import (
	"github.com/livekit/egress/pkg/types"
	"github.com/livekit/protocol/livekit"
)

type StreamConfig struct {
	outputConfig

	Urls       []string
	StreamInfo map[string]*livekit.StreamInfo
}

func (p *PipelineConfig) GetStreamConfig() *StreamConfig {
	o, ok := p.Outputs[types.EgressTypeStream]
	if !ok {
		return nil
	}
	return o.(*StreamConfig)
}

func (p *PipelineConfig) GetWebsocketConfig() *StreamConfig {
	o, ok := p.Outputs[types.EgressTypeWebsocket]
	if !ok {
		return nil
	}
	return o.(*StreamConfig)
}

func (p *PipelineConfig) getStreamConfig(outputType types.OutputType, urls []string) (*StreamConfig, error) {
	conf := &StreamConfig{
		outputConfig: outputConfig{OutputType: outputType},
	}

	switch outputType {
	case types.OutputTypeRTMP:
		p.AudioOutCodec = types.MimeTypeAAC
		p.VideoOutCodec = types.MimeTypeH264
		conf.Urls = urls

	case types.OutputTypeRaw:
		p.AudioOutCodec = types.MimeTypeRawAudio
		conf.Urls = urls
	}

	// Use a 4s default key frame interval for streaming
	if p.KeyFrameInterval == 0 {
		p.KeyFrameInterval = 4
	}

	conf.StreamInfo = make(map[string]*livekit.StreamInfo)
	var streamInfoList []*livekit.StreamInfo
	for _, rawUrl := range urls {
		redacted, err := p.ValidateUrl(rawUrl, outputType)
		if err != nil {
			return nil, err
		}

		info := &livekit.StreamInfo{Url: redacted}
		conf.StreamInfo[rawUrl] = info
		streamInfoList = append(streamInfoList, info)
	}

	return conf, nil
}
