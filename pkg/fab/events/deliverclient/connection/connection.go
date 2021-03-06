/*
Copyright SecureKey Technologies Inc. All Rights Reserved.

SPDX-License-Identifier: Apache-2.0
*/

package connection

import (
	"context"
	"fmt"
	"io"

	"github.com/hyperledger/fabric-sdk-go/third_party/github.com/hyperledger/fabric/protos/utils"

	"github.com/golang/protobuf/proto"
	fabcontext "github.com/hyperledger/fabric-sdk-go/pkg/context"
	"github.com/hyperledger/fabric-sdk-go/pkg/fab/comm"
	clientdisp "github.com/hyperledger/fabric-sdk-go/pkg/fab/events/client/dispatcher"
	"github.com/hyperledger/fabric-sdk-go/pkg/logging"
	"github.com/hyperledger/fabric-sdk-go/pkg/options"
	"github.com/pkg/errors"

	"github.com/hyperledger/fabric-sdk-go/internal/github.com/hyperledger/fabric/common/crypto"
	ab "github.com/hyperledger/fabric-sdk-go/internal/github.com/hyperledger/fabric/protos/orderer"
	cb "github.com/hyperledger/fabric-sdk-go/third_party/github.com/hyperledger/fabric/protos/common"
	pb "github.com/hyperledger/fabric-sdk-go/third_party/github.com/hyperledger/fabric/protos/peer"
	"google.golang.org/grpc"
)

var logger = logging.NewLogger("fabric_sdk_go")

type deliverStream interface {
	grpc.ClientStream
	Send(*cb.Envelope) error
	Recv() (*pb.DeliverResponse, error)
}

// DeliverConnection manages the connection to the deliver server
type DeliverConnection struct {
	comm.GRPCConnection
}

// StreamProvider creates a deliver stream
type StreamProvider func(pb.DeliverClient) (deliverStream, error)

var (
	// Deliver creates a Deliver stream
	Deliver = func(client pb.DeliverClient) (deliverStream, error) {
		return client.Deliver(context.Background())
	}

	// DeliverFiltered creates a DeliverFiltered stream
	DeliverFiltered = func(client pb.DeliverClient) (deliverStream, error) {
		return client.DeliverFiltered(context.Background())
	}
)

// New returns a new Deliver Server connection
func New(ctx fabcontext.Context, channelID string, streamProvider StreamProvider, url string, opts ...options.Opt) (*DeliverConnection, error) {
	if channelID == "" {
		return nil, errors.New("channel ID not provided")
	}

	connect, err := comm.NewConnection(
		ctx, channelID,
		func(grpcconn *grpc.ClientConn) (grpc.ClientStream, error) {
			return streamProvider(pb.NewDeliverClient(grpcconn))
		},
		url, opts...,
	)
	if err != nil {
		return nil, err
	}

	return &DeliverConnection{
		GRPCConnection: *connect,
	}, nil
}

func (c *DeliverConnection) deliverStream() deliverStream {
	if c.Stream() == nil {
		return nil
	}
	stream, ok := c.Stream().(deliverStream)
	if !ok {
		panic(fmt.Sprintf("invalid DeliverStream type %T", c.Stream()))
	}
	return stream
}

// Send sends a seek request to the deliver server
func (c *DeliverConnection) Send(seekInfo *ab.SeekInfo) error {
	if c.Closed() {
		return errors.New("connection is closed")
	}

	logger.Debugf("Sending %v\n", seekInfo)

	env, err := c.createSignedEnvelope(seekInfo)
	if err != nil {
		return err
	}

	return c.deliverStream().Send(env)
}

// Receive receives events from the deliver server
func (c *DeliverConnection) Receive(eventch chan<- interface{}) {
	for {
		stream := c.deliverStream()
		if stream == nil {
			logger.Warnf("The stream has closed. Terminating loop.\n")
			break
		}

		in, err := stream.Recv()

		if c.Closed() {
			logger.Debugf("The connection has closed. Terminating loop.\n")
			break
		}

		if err == io.EOF {
			// This signifies that the stream has been terminated at the client-side. No need to send an event.
			logger.Debugf("Received EOF from stream.\n")
			break
		}

		if err != nil {
			logger.Errorf("Received error from stream: [%s]. Sending disconnected event.\n", err)
			eventch <- clientdisp.NewDisconnectedEvent(err)
			break
		}

		eventch <- in
	}
	logger.Debugf("Exiting stream listener\n")
}

func (c *DeliverConnection) createSignedEnvelope(msg proto.Message) (*cb.Envelope, error) {
	// TODO: Do we need to make these configurable?
	var msgVersion int32
	var epoch uint64

	payloadChannelHeader := utils.MakeChannelHeader(cb.HeaderType_DELIVER_SEEK_INFO, msgVersion, c.ChannelID(), epoch)
	payloadChannelHeader.TlsCertHash = c.TLSCertHash()
	var err error

	data, err := proto.Marshal(msg)
	if err != nil {
		return nil, err
	}

	identity, err := c.Context().Identity()
	if err != nil {
		return nil, err
	}

	nonce, err := crypto.GetRandomNonce()
	if err != nil {
		return nil, err
	}

	payloadSignatureHeader := &cb.SignatureHeader{
		Creator: identity,
		Nonce:   nonce,
	}

	paylBytes := utils.MarshalOrPanic(&cb.Payload{
		Header: utils.MakePayloadHeader(payloadChannelHeader, payloadSignatureHeader),
		Data:   data,
	})

	signature, err := c.Context().SigningManager().Sign(data, c.Context().PrivateKey())
	if err != nil {
		return nil, err
	}

	return &cb.Envelope{Payload: paylBytes, Signature: signature}, nil
}
