/******************************************************************************
 *
 *  Description :
 *
 *    Handler of gRPC connections. See also hdl_websock.go for websockets and
 *    hdl_longpoll.go for long polling.
 *
 *****************************************************************************/

package main

import (
	"crypto/tls"
	"io"
	"log"
	"net"
	"time"

	"github.com/tinode/chat/pbx"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials"
	"google.golang.org/grpc/reflection"
	"google.golang.org/grpc/channelz/service"
	"google.golang.org/grpc/keepalive"
)

type grpcNodeServer struct {
}

func (sess *Session) closeGrpc() {
	if sess.proto == GRPC {
		sess.lock.Lock()
		sess.grpcnode = nil
		sess.lock.Unlock()
	}
}

// Equivalent of starting a new session and a read loop in one
func (*grpcNodeServer) MessageLoop(stream pbx.Node_MessageLoopServer) error {
	sess, _ := globals.sessionStore.NewSession(stream, "")

	defer func() {
		sess.closeGrpc()
		sess.cleanUp(false)
	}()

	go sess.writeGrpcLoop()

	for {
		in, err := stream.Recv()
		if err == io.EOF {
			return nil
		}
		if err != nil {
			log.Println("grpc: recv", sess.sid, err)
			return err
		}
		log.Println("grpc in:", truncateStringIfTooLong(in.String()), sess.sid)
		sess.dispatch(pbCliDeserialize(in))

		sess.lock.Lock()
		if sess.grpcnode == nil {
			sess.lock.Unlock()
			break
		}
		sess.lock.Unlock()
	}

	return nil
}

func (sess *Session) writeGrpcLoop() {

	defer func() {
		sess.closeGrpc() // exit MessageLoop
	}()

	for {
		select {
		case msg, ok := <-sess.send:
			if !ok {
				// channel closed
				return
			}
			if err := grpcWrite(sess, msg); err != nil {
				log.Println("grpc: write", sess.sid, err)
				return
			}
		case msg := <-sess.stop:
			// Shutdown requested, don't care if the message is delivered
			if msg != nil {
				grpcWrite(sess, msg)
			}
			return

		case topic := <-sess.detach:
			sess.delSub(topic)
		}
	}
}

func grpcWrite(sess *Session, msg interface{}) error {
	out := sess.grpcnode
	if out != nil {
		// Will panic if msg is not of *pbx.ServerMsg type. This is an intentional panic.
		return out.Send(msg.(*pbx.ServerMsg))
	}
	return nil
}

func serveGrpc(addr string, tlsConf *tls.Config) (*grpc.Server, error) {
	if addr == "" {
		return nil, nil
	}

	lis, err := net.Listen("tcp", addr)
	if err != nil {
		return nil, err
	}

	secure := ""
	var opts []grpc.ServerOption
	opts = append(opts, grpc.MaxRecvMsgSize(int(globals.maxMessageSize)))
	if tlsConf != nil {
		opts = append(opts, grpc.Creds(credentials.NewTLS(tlsConf)))
		secure = " secure"
	}
	opts = append(opts, grpc.KeepaliveEnforcementPolicy(keepalive.EnforcementPolicy{
		// MinTime is the minimum amount of time a client should wait before sending
		// a keepalive ping. The current default value is 5 minutes.
		MinTime:             1 * time.Second,

		// If true, server allows keepalive pings even when there are no active
		// streams(RPCs). If false, and client sends ping when there are no active
		// streams, server will send GOAWAY and close the connection. false by default.
		PermitWithoutStream: true,
	}))

	opts = append(opts, grpc.KeepaliveParams(keepalive.ServerParameters{
		// MaxConnectionIdle is a duration for the amount of time after which an
		// idle connection would be closed by sending a GoAway. Idleness duration is
		// defined since the most recent time the number of outstanding RPCs became
		// zero or the connection establishment. The current default value is infinity.
		// MaxConnectionIdle:     15 * time.Second,

		// MaxConnectionAge is a duration for the maximum amount of time a
		// connection may exist before it will be closed by sending a GoAway. A
		// random jitter of +/-10% will be added to MaxConnectionAge to spread out
		// connection storms. The current default value is infinity.
		// MaxConnectionAge:      30 * time.Second,

		// MaxConnectionAgeGrace is an additive period after MaxConnectionAge after
		// which the connection will be forcibly closed. The current default value is infinity.
		// MaxConnectionAgeGrace: 5 * time.Second,

		// After a duration of this time if the server doesn't see any activity it
		// pings the client to see if the transport is still alive. // The current default value is 2 hours.
		Time:                  60 * time.Second,

		// After having pinged for keepalive check, the server waits for a duration
		// of Timeout and if no activity is seen even after that the connection is
		// closed. The current default value is 20 seconds.
		Timeout:               20 * time.Second,
	}))

	srv := grpc.NewServer(opts...)
	reflection.Register(srv)
	service.RegisterChannelzServiceToServer(srv)
	pbx.RegisterNodeServer(srv, &grpcNodeServer{})
	log.Printf("gRPC/%s%s server is registered at [%s]", grpc.Version, secure, addr)

	go func() {
		if err := srv.Serve(lis); err != nil {
			log.Println("gRPC server failed:", err)
		}
	}()

	return srv, nil
}
