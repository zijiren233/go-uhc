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
	return func(options *Transport) {
		options.Timeout = timeout
	}
}

func WithBaseRoundTripper(base http.RoundTripper) TransportOption {
	return func(options *Transport) {
		options.Base = base
	}
}

func WithClientHelloID(clientHelloID utls.ClientHelloID) TransportOption {
	return func(options *Transport) {
		options.ClientHelloID = clientHelloID
	}
}

func WithInsecureSkipVerify(insecureSkipVerify bool) TransportOption {
	return func(options *Transport) {
		options.InsecureSkipVerify = insecureSkipVerify
	}
}

func NewTransport(opts ...TransportOption) *Transport {
	rt := &Transport{}
	for _, opt := range opts {
		opt(rt)
	}
	return rt
}

type Transport struct {
	Base               http.RoundTripper
	ClientHelloID      utls.ClientHelloID
	InsecureSkipVerify bool
	Timeout            time.Duration
	ProxySocks5        *url.URL
}

var _ io.ReadCloser = (*utlsHttpBody)(nil)

type utlsHttpBody struct {
	conn    *utls.UConn
	rawBody io.ReadCloser
}

func (u *utlsHttpBody) Read(p []byte) (n int, err error) {
	return u.rawBody.Read(p)
}

func (u *utlsHttpBody) Close() error {
	defer u.conn.Close()
	return u.rawBody.Close()
}

var (
	defaultClientHelloID = utls.HelloChrome_Auto
)

func (u *Transport) RoundTrip(req *http.Request) (*http.Response, error) {
	ctx := req.Context()
	if u.Timeout != 0 {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, u.Timeout)
		defer cancel()
	}
	clientHelloID := u.ClientHelloID
	if !clientHelloID.IsSet() {
		clientHelloID = defaultClientHelloID
	}

	address := net.JoinHostPort(req.URL.Hostname(), getRequestPort(req))
	conn, err := u.dialContext(ctx, "tcp", address)
	if err != nil {
		return nil, fmt.Errorf("dial %s failed: %w", address, err)
	}

	config := utls.Config{
		ServerName:         req.URL.Hostname(),
		InsecureSkipVerify: u.InsecureSkipVerify,
	}
	uTlsConn := utls.UClient(conn, &config, clientHelloID)
	err = uTlsConn.HandshakeContext(ctx)
	if err != nil {
		return nil, fmt.Errorf("utls handshake failed: %w", err)
	}

	resp, err := doHttpOverConn(u.Base, req, uTlsConn, uTlsConn.ConnectionState().NegotiatedProtocol)
	if err != nil {
		_ = uTlsConn.Close()
		return nil, fmt.Errorf("do http over conn failed: %w", err)
	}
	resp.Body = &utlsHttpBody{uTlsConn, resp.Body}
	return resp, nil
}

func getRequestPort(req *http.Request) string {
	port := req.URL.Port()
	if port == "" {
		switch req.URL.Scheme {
		case "https":
			port = "443"
		default:
			port = "80"
		}
	}
	return port
}

func (u *Transport) dialContext(ctx context.Context, network string, address string) (net.Conn, error) {
	if u.ProxySocks5 != nil {
		d, err := proxy.FromURL(u.ProxySocks5, proxy.Direct)
		if err != nil {
			return nil, err
		}
		if f, ok := d.(proxy.ContextDialer); ok {
			return f.DialContext(ctx, network, address)
		} else {
			return d.Dial(network, address)
		}
	} else {
		return proxy.Dial(ctx, network, address)
	}
}

func getH2RoundTripper(rt http.RoundTripper, conn net.Conn) (http.RoundTripper, error) {
	if rt != nil {
		tr, ok := rt.(*http.Transport)
		if ok {
			tr = tr.Clone()
			tr.MaxResponseHeaderBytes = 262144
			tr.DialContext = func(ctx context.Context, network, addr string) (net.Conn, error) {
				return conn, nil
			}
			h2transport, err := http2.ConfigureTransports(tr)
			if err != nil {
				return nil, err
			}
			c, err := h2transport.NewClientConn(conn)
			if err != nil {
				return nil, err
			}
			return c, nil
		} else if h2tr, ok := rt.(*http2.Transport); ok {
			h2tr.MaxHeaderListSize = 262144
			c, err := h2tr.NewClientConn(conn)
			if err != nil {
				return nil, err
			}
			return c, nil
		} else {
			return nil, fmt.Errorf("unsupported RoundTripper: %T", rt)
		}
	} else {
		tr := http2.Transport{MaxHeaderListSize: 262144}
		c, err := tr.NewClientConn(conn)
		if err != nil {
			return nil, err
		}
		return c, nil
	}
}

func doHttpOverConn(rt http.RoundTripper, req *http.Request, conn net.Conn, alpn string) (*http.Response, error) {
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
		err := req.Write(conn)
		if err != nil {
			return nil, fmt.Errorf("get http/1.1 round tripper failed: %w", err)
		}
		return http.ReadResponse(bufio.NewReader(conn), req)
	default:
		return nil, fmt.Errorf("unsupported ALPN: %v", alpn)
	}
}
