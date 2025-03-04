package service

import (
	"context"
	"net"
	"strings"
	"time"

	"github.com/frostbyte73/core"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	"github.com/livekit/egress/pkg/config"
	"github.com/livekit/egress/pkg/errors"
	"github.com/livekit/egress/pkg/ipc"
	"github.com/livekit/egress/pkg/pipeline"
	"github.com/livekit/egress/pkg/pprof"
	"github.com/livekit/protocol/livekit"
	"github.com/livekit/protocol/logger"
	"github.com/livekit/protocol/rpc"
	"github.com/livekit/protocol/tracer"
	"github.com/livekit/psrpc"
)

const network = "unix"

type Handler struct {
	ipc.UnimplementedEgressHandlerServer

	conf       *config.PipelineConfig
	pipeline   *pipeline.Pipeline
	rpcServer  rpc.EgressHandlerServer
	ioClient   rpc.IOInfoClient
	grpcServer *grpc.Server
	kill       core.Fuse
}

func NewHandler(conf *config.PipelineConfig, bus psrpc.MessageBus, ioClient rpc.IOInfoClient) (*Handler, error) {
	h := &Handler{
		conf:       conf,
		ioClient:   ioClient,
		grpcServer: grpc.NewServer(),
		kill:       core.NewFuse(),
	}

	rpcServer, err := rpc.NewEgressHandlerServer(conf.HandlerID, h, bus)
	if err != nil {
		return nil, errors.Fatal(err)
	}
	if err = rpcServer.RegisterUpdateStreamTopic(conf.Info.EgressId); err != nil {
		return nil, errors.Fatal(err)
	}
	if err = rpcServer.RegisterStopEgressTopic(conf.Info.EgressId); err != nil {
		return nil, errors.Fatal(err)
	}
	h.rpcServer = rpcServer

	listener, err := net.Listen(network, getSocketAddress(conf.TmpDir))
	if err != nil {
		return nil, errors.Fatal(err)
	}

	ipc.RegisterEgressHandlerServer(h.grpcServer, h)

	go func() {
		err := h.grpcServer.Serve(listener)
		if err != nil {
			logger.Errorw("failed to start grpc handler", err)
		}
	}()

	h.pipeline, err = pipeline.New(context.Background(), h.conf, h.sendUpdate)
	if err != nil {
		if !errors.IsFatal(err) {
			// user error, send update
			now := time.Now().UnixNano()
			conf.Info.UpdatedAt = now
			conf.Info.EndedAt = now
			conf.Info.Status = livekit.EgressStatus_EGRESS_FAILED
			conf.Info.Error = err.Error()
			h.sendUpdate(context.Background(), conf.Info)
		}
		return nil, err
	}

	return h, nil
}

func (h *Handler) Run() error {
	ctx, span := tracer.Start(context.Background(), "Handler.Run")
	defer span.End()

	// start egress
	result := make(chan *livekit.EgressInfo, 1)
	go func() {
		result <- h.pipeline.Run(ctx)
	}()

	kill := h.kill.Watch()
	for {
		select {
		case <-kill:
			// kill signal received
			h.pipeline.SendEOS(ctx)

		case res := <-result:
			// recording finished
			h.sendUpdate(ctx, res)
			h.rpcServer.Shutdown()
			h.grpcServer.Stop()
			return nil
		}
	}
}

func (h *Handler) UpdateStream(ctx context.Context, req *livekit.UpdateStreamRequest) (*livekit.EgressInfo, error) {
	ctx, span := tracer.Start(ctx, "Handler.UpdateStream")
	defer span.End()

	if h.pipeline == nil {
		return nil, errors.ErrEgressNotFound
	}

	err := h.pipeline.UpdateStream(ctx, req)
	if err != nil {
		return nil, err
	}
	return h.pipeline.Info, nil
}

func (h *Handler) StopEgress(ctx context.Context, _ *livekit.StopEgressRequest) (*livekit.EgressInfo, error) {
	ctx, span := tracer.Start(ctx, "Handler.StopEgress")
	defer span.End()

	if h.pipeline == nil {
		return nil, errors.ErrEgressNotFound
	}

	h.pipeline.SendEOS(ctx)
	return h.pipeline.Info, nil
}

func (h *Handler) GetPipelineDot(ctx context.Context, _ *ipc.GstPipelineDebugDotRequest) (*ipc.GstPipelineDebugDotResponse, error) {
	ctx, span := tracer.Start(ctx, "Handler.GetPipelineDot")
	defer span.End()

	if h.pipeline == nil {
		return nil, errors.ErrEgressNotFound
	}

	res := make(chan string, 1)
	go func() {
		res <- h.pipeline.GetGstPipelineDebugDot()
	}()

	select {
	case r := <-res:
		return &ipc.GstPipelineDebugDotResponse{
			DotFile: r,
		}, nil

	case <-time.After(2 * time.Second):
		return nil, status.New(codes.DeadlineExceeded, "timed out requesting pipeline debug info").Err()
	}
}

func (h *Handler) GetPProf(ctx context.Context, req *ipc.PProfRequest) (*ipc.PProfResponse, error) {
	ctx, span := tracer.Start(ctx, "Handler.GetPProf")
	defer span.End()

	if h.pipeline == nil {
		return nil, errors.ErrEgressNotFound
	}

	b, err := pprof.GetProfileData(ctx, req.ProfileName, int(req.Timeout), int(req.Debug))
	if err != nil {
		return nil, err
	}

	return &ipc.PProfResponse{
		PprofFile: b,
	}, nil
}

func (h *Handler) Kill() {
	h.kill.Break()
}

func (h *Handler) sendUpdate(ctx context.Context, info *livekit.EgressInfo) {
	sendUpdate(ctx, h.ioClient, info)
}

func sendUpdate(ctx context.Context, c rpc.IOInfoClient, info *livekit.EgressInfo) {
	requestType, outputType := getTypes(info)
	switch info.Status {
	case livekit.EgressStatus_EGRESS_FAILED:
		logger.Warnw("egress failed", errors.New(info.Error),
			"egressID", info.EgressId,
			"request_type", requestType,
			"output_type", outputType,
		)
	case livekit.EgressStatus_EGRESS_COMPLETE:
		logger.Infow("egress completed",
			"egressID", info.EgressId,
			"request_type", requestType,
			"output_type", outputType,
		)
	default:
		logger.Infow("egress updated",
			"egressID", info.EgressId,
			"request_type", requestType,
			"output_type", outputType,
			"status", info.Status,
		)
	}

	if _, err := c.UpdateEgressInfo(ctx, info); err != nil {
		logger.Errorw("failed to send update", err)
	}
}

func getTypes(info *livekit.EgressInfo) (requestType string, outputType string) {
	switch req := info.Request.(type) {
	case *livekit.EgressInfo_RoomComposite:
		requestType = "room_composite"
		outputType = getOutputType(req.RoomComposite)
	case *livekit.EgressInfo_Web:
		requestType = "web"
		outputType = getOutputType(req.Web)
	case *livekit.EgressInfo_TrackComposite:
		requestType = "track_composite"
		outputType = getOutputType(req.TrackComposite)
	case *livekit.EgressInfo_Track:
		requestType = "track"
		switch req.Track.Output.(type) {
		case *livekit.TrackEgressRequest_File:
			outputType = "file"
		case *livekit.TrackEgressRequest_WebsocketUrl:
			outputType = "websocket"
		}
	}

	return
}

func getOutputType(r config.EncodedOutput) string {
	if r.GetFile() != nil {
		return "file"
	}
	if r.GetStream() != nil {
		return "stream"
	}
	if r.GetSegments() != nil {
		return "segments"
	}
	outputs := make([]string, 0)
	if len(r.GetFileOutputs()) > 0 {
		outputs = append(outputs, "file")
	}
	if len(r.GetStreamOutputs()) > 0 {
		outputs = append(outputs, "stream")
	}
	if len(r.GetSegmentOutputs()) > 0 {
		outputs = append(outputs, "segments")
	}
	return strings.Join(outputs, ", ")
}
