package service

import (
	"context"
	"net"

	"github.com/frostbyte73/core"
	"google.golang.org/grpc"
	"google.golang.org/protobuf/proto"

	"github.com/livekit/egress/pkg/config"
	"github.com/livekit/egress/pkg/errors"
	"github.com/livekit/egress/pkg/ipc"
	"github.com/livekit/egress/pkg/pipeline"
	"github.com/livekit/protocol/egress"
	"github.com/livekit/protocol/livekit"
	"github.com/livekit/protocol/logger"
	"github.com/livekit/protocol/tracer"
)

type HandlerV0 struct {
	ipc.UnimplementedEgressHandlerServer

	conf       *config.PipelineConfig
	rpcServer  egress.RPCServer
	grpcServer *grpc.Server
	kill       core.Fuse
}

func NewHandlerV0(conf *config.PipelineConfig, rpcServer egress.RPCServer) (*HandlerV0, error) {
	h := &HandlerV0{
		conf:       conf,
		rpcServer:  rpcServer,
		grpcServer: grpc.NewServer(),
		kill:       core.NewFuse(),
	}

	listener, err := net.Listen(network, getSocketAddress(conf.TmpDir))
	if err != nil {
		return nil, err
	}

	ipc.RegisterEgressHandlerServer(h.grpcServer, h)

	go func() {
		err := h.grpcServer.Serve(listener)
		if err != nil {
			logger.Errorw("failed to start grpc handler", err)
		}
	}()

	return h, nil
}

func (h *HandlerV0) Run() error {
	ctx, span := tracer.Start(context.Background(), "HandlerV0.Run")
	defer span.End()

	p, err := h.buildPipeline(ctx)
	if err != nil {
		span.RecordError(err)
		if errors.IsFatal(err) {
			return err
		} else {
			return nil
		}
	}

	// subscribe to request channel
	requests, err := h.rpcServer.EgressSubscription(context.Background(), p.Info.EgressId)
	if err != nil {
		span.RecordError(err)
		logger.Errorw("failed to subscribe to egress", err)
		return nil
	}
	defer func() {
		if err := requests.Close(); err != nil {
			logger.Errorw("failed to unsubscribe from request channel", err)
		}
	}()

	// start egress
	result := make(chan *livekit.EgressInfo, 1)
	go func() {
		result <- p.Run(ctx)
	}()

	kill := h.kill.Watch()
	for {
		select {
		case <-kill:
			// kill signal received
			p.SendEOS(ctx)

		case res := <-result:
			// recording finished
			h.sendUpdate(ctx, res)
			return nil

		case msg := <-requests.Channel():
			// request received
			request := &livekit.EgressRequest{}
			err = proto.Unmarshal(requests.Payload(msg), request)
			if err != nil {
				logger.Errorw("failed to read request", err, "egressID", p.Info.EgressId)
				continue
			}
			logger.Debugw("handling request", "egressID", p.Info.EgressId, "requestID", request.RequestId)

			switch r := request.Request.(type) {
			case *livekit.EgressRequest_UpdateStream:
				err = p.UpdateStream(ctx, r.UpdateStream)
			case *livekit.EgressRequest_Stop:
				p.SendEOS(ctx)
			default:
				err = errors.ErrInvalidRPC
			}

			h.sendResponse(ctx, request, p.Info, err)
		}
	}
}

func (h *HandlerV0) buildPipeline(ctx context.Context) (*pipeline.Pipeline, error) {
	ctx, span := tracer.Start(ctx, "HandlerV0.buildPipeline")
	defer span.End()

	// build/verify params
	p, err := pipeline.New(ctx, h.conf, h.sendUpdate)
	if err != nil {
		h.conf.Info.Error = err.Error()
		h.conf.Info.Status = livekit.EgressStatus_EGRESS_FAILED
		h.sendUpdate(ctx, h.conf.Info)
		span.RecordError(err)
		return nil, err
	}

	return p, nil
}

func (h *HandlerV0) sendUpdate(ctx context.Context, info *livekit.EgressInfo) {
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

	if err := h.rpcServer.SendUpdate(ctx, info); err != nil {
		logger.Errorw("failed to send update", err)
	}
}

func (h *HandlerV0) sendResponse(ctx context.Context, req *livekit.EgressRequest, info *livekit.EgressInfo, err error) {
	args := []interface{}{
		"egressID", info.EgressId,
		"requestID", req.RequestId,
		"senderID", req.SenderId,
	}

	if err != nil {
		logger.Warnw("request failed", err, args...)
	} else {
		logger.Debugw("request handled", args...)
	}

	if err = h.rpcServer.SendResponse(ctx, req, info, err); err != nil {
		logger.Errorw("failed to send response", err, args...)
	}
}

func (h *HandlerV0) Kill() {
	h.kill.Break()
}
