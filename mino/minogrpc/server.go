package minogrpc

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"io"
	"math/big"
	"net"
	"net/http"
	"time"

	"github.com/golang/protobuf/proto"
	"github.com/golang/protobuf/ptypes"
	"go.dedis.ch/fabric"
	"go.dedis.ch/fabric/encoding"
	"go.dedis.ch/fabric/mino"
	"golang.org/x/xerrors"
	"google.golang.org/grpc"
	"google.golang.org/grpc/backoff"
	"google.golang.org/grpc/credentials"
	"google.golang.org/grpc/metadata"
)

const (
	headerURIKey        = "apiuri"
	headerAddressKey    = "addr"
	certificateDuration = time.Hour * 24 * 180
)

var (
	// defaultMinConnectTimeout is the minimum amount of time we are willing to
	// wait for a grpc connection to complete
	defaultMinConnectTimeout = 7 * time.Second
	// defaultContextTimeout is the amount of time we are willing to wait for a
	// remote procedure call to finish. This value should always be higher than
	// defaultMinConnectTimeout in order to capture http server errors.
	defaultContextTimeout = 10 * time.Second
)

// Server represents the entity that accepts incoming requests and invoke the
// corresponding RPCs.
type Server struct {
	grpcSrv *grpc.Server

	cert      *tls.Certificate
	addr      mino.Address
	listener  net.Listener
	httpSrv   *http.Server
	StartChan chan struct{}

	// neighbours contains the certificate and details about known peers.
	neighbours map[string]Peer

	handlers map[string]mino.Handler

	localStreamClients map[string]Overlay_StreamClient
}

type ctxURIKey string

const ctxKey = ctxURIKey("URI")

// Roster is a set of peers that will work together
// to execute protocols.
type Roster []Peer

// Peer is a public identity for a given node.
type Peer struct {
	Address     string
	Certificate *x509.Certificate
}

// RPC represents an RPC that has been registered by a client, which allows
// clients to call an RPC that will execute the provided handler.
//
// - implements mino.RPC
type RPC struct {
	handler mino.Handler
	srv     Server
	uri     string
}

// Call implements mino.RPC.Call(). It calls the RPC on each provided address.
func (rpc RPC) Call(req proto.Message,
	players mino.Players) (<-chan proto.Message, <-chan error) {

	out := make(chan proto.Message, players.Len())
	errs := make(chan error, players.Len())

	m, err := ptypes.MarshalAny(req)
	if err != nil {
		errs <- xerrors.Errorf("failed to marshal msg to any: %v", err)
		return out, errs
	}

	sendMsg := &OverlayMsg{
		Message: m,
	}

	go func() {
		iter := players.AddressIterator()
		for iter.HasNext() {
			addrStr := iter.GetNext().String()
			clientConn, err := rpc.srv.getConnection(addrStr)
			if err != nil {
				errs <- xerrors.Errorf("failed to get client conn for '%s': %v",
					addrStr, err)
				continue
			}
			cl := NewOverlayClient(clientConn)

			ctx, ctxCancelFunc := context.WithTimeout(context.Background(),
				defaultContextTimeout)
			defer ctxCancelFunc()

			header := metadata.New(map[string]string{headerURIKey: rpc.uri})
			ctx = metadata.NewOutgoingContext(ctx, header)

			callResp, err := cl.Call(ctx, sendMsg)
			if err != nil {
				errs <- xerrors.Errorf("failed to call client '%s': %v", addrStr, err)
				continue
			}

			var resp ptypes.DynamicAny
			err = ptypes.UnmarshalAny(callResp.Message, &resp)
			if err != nil {
				errs <- encoding.NewAnyDecodingError(resp, err)
				continue
			}

			out <- resp.Message
		}

		close(out)
	}()

	return out, errs
}

// Stream implements mino.RPC.Stream.
func (rpc RPC) Stream(ctx context.Context,
	players mino.Players) (in mino.Sender, out mino.Receiver) {

	// if every player produces an error the buffer should be large enought so
	// that we are never blocked in the for loop and we can termninate this
	// function.
	errs := make(chan error, players.Len())

	orchSender := sender{
		address:      address{},
		participants: make([]player, players.Len()),
		name:         "orchestrator",
	}

	orchRecv := receiver{
		errs: errs,
		// it is okay to have a blocking chan here because every use of it is in
		// a goroutine, where we don't mind if it blocks.
		in:   make(chan *OverlayMsg),
		name: "orchestrator",
	}

	// Creating a stream for each provided addr
	for i := 0; players.AddressIterator().HasNext(); i++ {
		addr := players.AddressIterator().GetNext()

		clientConn, err := rpc.srv.getConnection(addr.String())
		if err != nil {
			// TODO: try another path (maybe use another node to relay that
			// message)
			fabric.Logger.Error().Msgf("failed to get client conn for client '%s': %v",
				addr.String(), err)
			errs <- xerrors.Errorf("failed to get client conn for client '%s': %v",
				addr.String(), err)
			continue
		}
		cl := NewOverlayClient(clientConn)

		header := metadata.New(map[string]string{
			headerURIKey:     rpc.uri,
			headerAddressKey: ""})
		ctx = metadata.NewOutgoingContext(ctx, header)

		stream, err := cl.Stream(ctx)
		if err != nil {
			fabric.Logger.Error().Msgf("failed to get stream for client '%s': %v",
				addr.String(), err)
			errs <- xerrors.Errorf("failed to get stream for client '%s': %v",
				addr.String(), err)
			continue
		}

		orchSender.participants[i] = player{
			address:      address{addr.String()},
			streamClient: stream,
		}

		go func() {
			for {
				msg, err := stream.Recv()
				if err == io.EOF {
					return
				}

				if err != nil {
					fabric.Logger.Error().Msgf("failed to receive for client '%s': %v",
						addr.String(), err)
					errs <- xerrors.Errorf("failed to receive for client '%s': %v",
						addr.String(), err)
					return
				}

				orchRecv.in <- msg
			}
		}()
	}

	return orchSender, orchRecv
}

// CreateServer sets up a new server
func CreateServer(addr mino.Address) (*Server, error) {
	if addr.String() == "" {
		return nil, xerrors.New("addr.String() should not give an empty string")
	}

	cert, err := makeCertificate()
	if err != nil {
		return nil, xerrors.Errorf("failed to make certificate: %v", err)
	}

	srv := grpc.NewServer()

	server := &Server{
		grpcSrv:    srv,
		cert:       cert,
		addr:       addr,
		listener:   nil,
		StartChan:  make(chan struct{}),
		neighbours: make(map[string]Peer),
		handlers:   make(map[string]mino.Handler),
	}

	RegisterOverlayServer(srv, &overlayService{
		handlers: server.handlers,
	})

	return server, nil
}

// StartServer makes the server start listening and wait until its started
func (srv *Server) StartServer() {

	go func() {
		err := srv.Serve()
		// TODO: better handle this error
		if err != nil {
			fabric.Logger.Fatal().Msg("failed to start the server " + err.Error())
		}
	}()

	<-srv.StartChan
}

// Serve starts the HTTP server that forwards gRPC calls to the gRPC server
func (srv *Server) Serve() error {
	lis, err := net.Listen("tcp4", srv.addr.String())
	if err != nil {
		return xerrors.Errorf("failed to listen: %v", err)
	}

	srv.listener = lis

	close(srv.StartChan)

	srv.httpSrv = &http.Server{
		TLSConfig: &tls.Config{
			// TODO: a LE certificate or similar must be used alongside the
			// actual server certificate for the browser to accept the TLS
			// connection.
			Certificates: []tls.Certificate{*srv.cert},
			ClientAuth:   tls.RequestClientCert,
		},
		Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if r.Header.Get("Content-Type") == "application/grpc" {
				srv.grpcSrv.ServeHTTP(w, r)
			}
		}),
	}

	// This call always returns an error
	err = srv.httpSrv.ServeTLS(lis, "", "")
	if err != http.ErrServerClosed {
		return xerrors.Errorf("failed to serve: %v", err)
	}

	return nil
}

// getConnection creates a gRPC connection from the server to the client
func (srv *Server) getConnection(addr string) (*grpc.ClientConn, error) {
	if addr == "" {
		return nil, xerrors.New("empty address is not allowed")
	}

	neighbour, ok := srv.neighbours[addr]
	if !ok {
		return nil, xerrors.Errorf("couldn't find neighbour [%s]", addr)
	}

	pool := x509.NewCertPool()
	pool.AddCert(neighbour.Certificate)

	ta := credentials.NewTLS(&tls.Config{
		Certificates: []tls.Certificate{*srv.cert},
		RootCAs:      pool,
	})

	// Connecting using TLS and the distant server certificate as the root.
	conn, err := grpc.Dial(addr, grpc.WithTransportCredentials(ta),
		grpc.WithConnectParams(grpc.ConnectParams{
			Backoff:           backoff.DefaultConfig,
			MinConnectTimeout: defaultMinConnectTimeout,
		}))
	if err != nil {
		return nil, xerrors.Errorf("failed to create a dial connection: %v", err)
	}

	return conn, nil
}

func makeCertificate() (*tls.Certificate, error) {
	priv, err := ecdsa.GenerateKey(elliptic.P521(), rand.Reader)
	if err != nil {
		return nil, xerrors.Errorf("Couldn't generate the private key: %+v", err)
	}

	tmpl := &x509.Certificate{
		SerialNumber: big.NewInt(1),
		IPAddresses:  []net.IP{net.ParseIP("127.0.0.1")},
		NotBefore:    time.Now(),
		NotAfter:     time.Now().Add(certificateDuration),

		KeyUsage:              x509.KeyUsageKeyEncipherment | x509.KeyUsageDigitalSignature,
		ExtKeyUsage:           []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
		BasicConstraintsValid: true,
	}

	buf, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &priv.PublicKey, priv)
	if err != nil {
		return nil, xerrors.Errorf("Couldn't create the certificate: %+v", err)
	}

	cert, err := x509.ParseCertificate(buf)
	if err != nil {
		return nil, xerrors.Errorf("Couldn't parse the certificate: %+v", err)
	}

	return &tls.Certificate{
		Certificate: [][]byte{buf},
		PrivateKey:  priv,
		Leaf:        cert,
	}, nil
}

// sender implements mino.Sender{}
type sender struct {
	address      address
	participants []player
	name         string
}

type player struct {
	address      address
	streamClient overlayStream
}

// send implements mino.Sender.Send()
func (s sender) Send(msg proto.Message, addrs ...mino.Address) error {

	ok := false
	for _, addr := range addrs {
		player := s.getParticipant(addr)
		if player == nil {
			continue
		}

		ok = true
		msgAny, err := ptypes.MarshalAny(msg)
		if err != nil {
			return encoding.NewAnyEncodingError(msg, err)
		}

		envelope := &Envelope{
			From:    s.address.String(),
			To:      []string{addr.String()},
			Message: msgAny,
		}

		envelopeAny, err := ptypes.MarshalAny(envelope)
		if err != nil {
			return encoding.NewAnyEncodingError(msg, err)
		}

		sendMsg := &OverlayMsg{
			Message: envelopeAny,
		}
		err = player.streamClient.Send(sendMsg)
		if err != nil {
			return xerrors.Errorf("failed to call the send on client stream: %v", err)
		}
	}

	if !ok {
		fabric.Logger.Warn().Msg("warning: the given address didn't match " +
			"any of the RPC. No message sent.")
	}

	return nil
}

// receiver implements mino.receiver{}
type receiver struct {
	errs chan error
	in   chan *OverlayMsg
	name string
}

// Recv implements mino.receiver.Recv()
// TODO: check the error chan
func (r receiver) Recv(ctx context.Context) (mino.Address, proto.Message, error) {

	// TODO: close the channel
	var msg *OverlayMsg
	var err error

	select {
	case msg = <-r.in:
	case err = <-r.errs:
	}

	if err != nil {
		return nil, nil, xerrors.Errorf("got an error from the error chan: %v", err)
	}

	// we check it to prevent a panic on msg.Message
	if msg == nil {
		return nil, nil, xerrors.New("message is nil")
	}

	enveloppe := &Envelope{}
	err = ptypes.UnmarshalAny(msg.Message, enveloppe)
	if err != nil {
		return nil, nil, encoding.NewAnyDecodingError(msg.Message, err)
	}

	var dynamicAny ptypes.DynamicAny
	err = ptypes.UnmarshalAny(enveloppe.Message, &dynamicAny)
	if err != nil {
		return nil, nil, encoding.NewAnyDecodingError(enveloppe.Message, err)
	}

	return address{id: enveloppe.From}, dynamicAny.Message, nil
}

// This interface is used to have a common object between Overlay_StreamServer
// and Overlay_StreamClient. We need it because the orchastrator is setting
// Overlay_StreamClient, while the RPC (in overlay.go) is setting an
// Overlay_StreamServer
type overlayStream interface {
	Send(*OverlayMsg) error
	Recv() (*OverlayMsg, error)
}

// getParticipant checks if a participant exists on the list of participant.
// Returns nil if the participant is not in the list.
func (s sender) getParticipant(addr mino.Address) *player {
	for _, p := range s.participants {
		if p.address.String() == addr.String() {
			return &p
		}
	}
	return nil
}

// simpleNode implements mino.Node{}
type simpleNode struct {
	addr address
}

// getAddress implements mino.Node.GetAddress()
func (o simpleNode) GetAddress() mino.Address {
	return o.addr
}
