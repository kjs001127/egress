//go:build integration

package test

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/gorilla/websocket"
	"github.com/stretchr/testify/require"

	"github.com/livekit/egress/pkg/config"
	"github.com/livekit/egress/pkg/pipeline"
	"github.com/livekit/egress/pkg/types"
	"github.com/livekit/protocol/livekit"
	"github.com/livekit/protocol/logger"
	"github.com/livekit/protocol/rpc"
	"github.com/livekit/protocol/utils"
)

func (r *Runner) testTrack(t *testing.T) {
	if !r.runTrackTests() {
		return
	}

	r.sourceFramerate = 23.97
	r.testTrackFile(t)
	r.testTrackStream(t)
}

func (r *Runner) testTrackFile(t *testing.T) {
	if !r.runFileTests() {
		return
	}

	t.Run("Track/File", func(t *testing.T) {
		for _, test := range []*testCase{
			{
				name:       "OPUS",
				audioOnly:  true,
				audioCodec: types.MimeTypeOpus,
				outputType: types.OutputTypeOGG,
				filename:   "t_{track_source}_{time}.ogg",
			},
			{
				name:       "VP8",
				videoOnly:  true,
				videoCodec: types.MimeTypeVP8,
				outputType: types.OutputTypeWebM,
				filename:   "t_{track_type}_{time}.webm",
			},
			{
				name:       "H264",
				videoOnly:  true,
				videoCodec: types.MimeTypeH264,
				outputType: types.OutputTypeMP4,
				filename:   "t_{track_id}_{time}.mp4",
			},
		} {
			r.runSDKTest(t, test.name, test.audioCodec, test.videoCodec, func(t *testing.T, audioTrackID, videoTrackID string) {
				trackID := audioTrackID
				if trackID == "" {
					trackID = videoTrackID
				}

				trackRequest := &livekit.TrackEgressRequest{
					RoomName: r.room.Name(),
					TrackId:  trackID,
					Output: &livekit.TrackEgressRequest_File{
						File: &livekit.DirectFileOutput{
							Filepath: getFilePath(r.ServiceConfig, test.filename),
						},
					},
				}

				req := &rpc.StartEgressRequest{
					EgressId: utils.NewGuid(utils.EgressPrefix),
					Request: &rpc.StartEgressRequest_Track{
						Track: trackRequest,
					},
				}

				r.runFileTest(t, req, test)
			})
			if r.Short {
				return
			}
		}
	})
}

func (r *Runner) testTrackStream(t *testing.T) {
	if !r.runStreamTests() {
		return
	}

	t.Run("Track/Stream", func(t *testing.T) {
		now := time.Now().Unix()
		for _, test := range []*testCase{
			{
				name:       "Websocket",
				audioOnly:  true,
				audioCodec: types.MimeTypeOpus,
				filename:   fmt.Sprintf("track-ws-%v.raw", now),
			},
		} {
			r.runSDKTest(t, test.name, test.audioCodec, test.videoCodec, func(t *testing.T, audioTrackID, videoTrackID string) {
				trackID := audioTrackID
				if trackID == "" {
					trackID = videoTrackID
				}

				filepath := getFilePath(r.ServiceConfig, test.filename)
				wss := newTestWebsocketServer(filepath)
				s := httptest.NewServer(http.HandlerFunc(wss.handleWebsocket))
				defer func() {
					wss.close()
					s.Close()
				}()

				req := &rpc.StartEgressRequest{
					EgressId: utils.NewGuid(utils.EgressPrefix),
					Request: &rpc.StartEgressRequest_Track{
						Track: &livekit.TrackEgressRequest{
							RoomName: r.room.Name(),
							TrackId:  trackID,
							Output: &livekit.TrackEgressRequest_WebsocketUrl{
								WebsocketUrl: "ws" + strings.TrimPrefix(s.URL, "http"),
							},
						},
					},
				}

				ctx := context.Background()

				p, err := config.GetValidatedPipelineConfig(r.ServiceConfig, req)
				require.NoError(t, err)
				p.GstReady = make(chan struct{})

				rec, err := pipeline.New(ctx, p, func(_ context.Context, _ *livekit.EgressInfo) {})
				require.NoError(t, err)

				go func() {
					time.Sleep(time.Second * 35)
					rec.SendEOS(ctx)
				}()

				res := rec.Run(ctx)
				verify(t, filepath, p, res, types.EgressTypeWebsocket, r.Muting, r.sourceFramerate)
			})
			if r.Short {
				return
			}
		}
	})
}

type websocketTestServer struct {
	path string
	file *os.File
	conn *websocket.Conn
	done chan struct{}
}

func newTestWebsocketServer(filepath string) *websocketTestServer {
	return &websocketTestServer{
		path: filepath,
		done: make(chan struct{}),
	}
}

func (s *websocketTestServer) handleWebsocket(w http.ResponseWriter, r *http.Request) {
	var err error

	s.file, err = os.Create(s.path)
	if err != nil {
		logger.Errorw("could not create file", err)
		return
	}

	// accept ws connection
	upgrader := websocket.Upgrader{}
	s.conn, err = upgrader.Upgrade(w, r, nil)
	if err != nil {
		logger.Errorw("could not accept ws connection", err)
		return
	}

	go func() {
		defer func() {
			_ = s.file.Close()

			// close the connection only if it's not closed already
			if !websocket.IsUnexpectedCloseError(err) {
				_ = s.conn.Close()
			}
		}()

		for {
			select {
			case <-s.done:
				return
			default:
				mt, msg, err := s.conn.ReadMessage()
				if err != nil {
					if !websocket.IsUnexpectedCloseError(err) {
						logger.Errorw("unexpected ws close", err)
					}
					return
				}

				switch mt {
				case websocket.BinaryMessage:
					_, err = s.file.Write(msg)
					if err != nil {
						logger.Errorw("could not write to file", err)
						return
					}
				}
			}
		}
	}()
}

func (s *websocketTestServer) close() {
	close(s.done)
}
