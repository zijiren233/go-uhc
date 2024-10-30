package uhc

import (
	"bufio"
	"context"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"time"

	utls "github.com/refraction-networking/utls"
	"golang.org/x/net/http2"
	"golang.org/x/net/proxy"
)

var DefaultTransport http.RoundTripper = &Transport{}

type TransportOption func(*Transport)

func WithTimeout(timeout time.Duration) TransportOption {
	return func(t *Transport) {
		t.Timeout = timeout
	}
}

func WithBaseRoundTripper(base http.RoundTripper) TransportOption {
	return func(t *Transport) {
		switch base := base.(type) {
		case *http.Transport:
			t.httpTransport = base.Clone()
		case *http2.Transport:
			base.MaxHeaderListSize = maxHeaderListSize
			t.h2Transport = base
		}
	}
}

func WithClientHelloID(clientHelloID utls.ClientHelloID) TransportOption {
	return func(t *Transport) {
		t.ClientHelloID = clientHelloID
	}
}

func WithInsecureSkipVerify(insecureSkipVerify bool) TransportOption {
	return func(t *Transport) {
		t.InsecureSkipVerify = insecureSkipVerify
	}
}

func NewTransport(opts ...TransportOption) *Transport {
	t := &Transport{}
	for _, opt := range opts {
		opt(t)
	}
	return t
}

type Transport struct {
	ClientHelloID      utls.ClientHelloID
	httpTransport      *http.Transport
	h2Transport        *http2.Transport
	ProxySocks5        *url.URL
	Timeout            time.Duration
	InsecureSkipVerify bool
}

type utlsHttpBody struct {
	conn    *utls.UConn
	rawBody io.ReadCloser
}

var _ io.ReadCloser = (*utlsHttpBody)(nil)

func (u *utlsHttpBody) Read(p []byte) (int, error) {
	return u.rawBody.Read(p)
}

func (u *utlsHttpBody) Close() error {
	defer u.conn.Close()
	return u.rawBody.Close()
}

const maxHeaderListSize = 262144

var (
	defaultClientHelloID = utls.HelloChrome_Auto
	defaultHttpTransport = http.DefaultTransport.(*http.Transport).Clone()
	defaultH2Transport   = &http2.Transport{MaxHeaderListSize: maxHeaderListSize}
)

func init() {
	defaultHttpTransport.MaxResponseHeaderBytes = maxHeaderListSize
}

func (t *Transport) RoundTrip(req *http.Request) (*http.Response, error) {
	if req.URL.Scheme == "http" {
		return getHttpRoundTripper(t.httpTransport).RoundTrip(req)
	}
	if req.URL.Scheme != "https" {
		return nil, fmt.Errorf("unsupported scheme: %s", req.URL.Scheme)
	}

	ctx := req.Context()
	if t.Timeout > 0 {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, t.Timeout)
		defer cancel()
	}

	clientHelloID := t.ClientHelloID
	if clientHelloID.IsSet() {
		clientHelloID = defaultClientHelloID
	}

	address := net.JoinHostPort(req.URL.Hostname(), getRequestPort(req))
	conn, err := t.dialContext(ctx, "tcp", address)
	if err != nil {
		return nil, fmt.Errorf("dial %s failed: %w", address, err)
	}

	config := &utls.Config{
		ServerName:         req.URL.Hostname(),
		InsecureSkipVerify: t.InsecureSkipVerify,
	}
	uTlsConn := utls.UClient(conn, config, clientHelloID)
	if err := uTlsConn.HandshakeContext(ctx); err != nil {
		conn.Close()
		return nil, fmt.Errorf("utls handshake failed: %w", err)
	}

	resp, err := doHttpOverConn(t.h2Transport, req, uTlsConn, uTlsConn.ConnectionState().NegotiatedProtocol)
	if err != nil {
		uTlsConn.Close()
		return nil, fmt.Errorf("do http over conn failed: %w", err)
	}

	resp.Body = &utlsHttpBody{conn: uTlsConn, rawBody: resp.Body}
	return resp, nil
}

func getRequestPort(req *http.Request) string {
	if port := req.URL.Port(); port != "" {
		return port
	}
	if req.URL.Scheme == "https" {
		return "443"
	}
	return "80"
}

func (t *Transport) dialContext(ctx context.Context, network, address string) (net.Conn, error) {
	if t.ProxySocks5 == nil {
		return proxy.Dial(ctx, network, address)
	}

	dialer, err := proxy.FromURL(t.ProxySocks5, proxy.Direct)
	if err != nil {
		return nil, err
	}

	if contextDialer, ok := dialer.(proxy.ContextDialer); ok {
		return contextDialer.DialContext(ctx, network, address)
	}
	return dialer.Dial(network, address)
}

func getHttpRoundTripper(rt *http.Transport) http.RoundTripper {
	if rt == nil {
		return defaultHttpTransport
	}

	return rt
}

func getH2RoundTripper(rt *http2.Transport, conn net.Conn) (http.RoundTripper, error) {
	if rt == nil {
		return defaultH2Transport.NewClientConn(conn)
	}

	rt.MaxHeaderListSize = maxHeaderListSize
	return rt.NewClientConn(conn)
}

func doHttpOverConn(rt *http2.Transport, req *http.Request, conn net.Conn, alpn string) (*http.Response, error) {
	switch alpn {
	case "h2":
		req.Proto = "HTTP/2.0"
		req.ProtoMajor = 2
		req.ProtoMinor = 0

		rt, err := getH2RoundTripper(rt, conn)
		if err != nil {
			return nil, fmt.Errorf("get http/2 round tripper failed: %w", err)
		}

		resp, err := rt.RoundTrip(req)
		if err != nil {
			return nil, fmt.Errorf("do http/2 request failed: %w", err)
		}
		return resp, nil

	case "http/1.1", "":
		req.Proto = "HTTP/1.1"
		req.ProtoMajor = 1
		req.ProtoMinor = 1

		if err := req.Write(conn); err != nil {
			return nil, fmt.Errorf("write http/1.1 request failed: %w", err)
		}
		return http.ReadResponse(bufio.NewReader(conn), req)

	default:
		return nil, fmt.Errorf("unsupported ALPN: %v", alpn)
	}
}
